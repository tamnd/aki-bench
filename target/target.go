// Package target launches or connects to the servers under test: aki,
// redis-server, and valkey-server. Two modes are supported. In connect mode the
// caller points at an already-running host:port. In launch mode the package
// spawns the server binary on a free port, waits for it to accept connections,
// and stops it on Close. When a binary is absent the launcher reports a skip so
// the harness can carry on with the targets that are present.
package target

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/tamnd/aki-bench/cpuset"
)

// Kind identifies which server a target is.
type Kind string

const (
	Aki    Kind = "aki"
	Redis  Kind = "redis"
	Valkey Kind = "valkey"
)

// Durability selects the fairness configuration a launched server runs with.
// The two configs let the report compare like for like: in-memory aki against
// in-memory Redis and Valkey, and durable against durable.
type Durability int

const (
	// InMemory disables persistence on every server so the comparison isolates
	// command execution. For Redis and Valkey that means no save points and no
	// appendonly. aki runs with the append-only file off so commits do not fsync.
	InMemory Durability = iota
	// Durable turns on an fsync-per-write config on every server: appendonly with
	// appendfsync always everywhere, including aki. This is the config that
	// actually proves a fair durable-vs-durable number.
	Durable
)

// Spec describes how to obtain one target.
type Spec struct {
	Kind       Kind
	Binary     string     // path or name of the server binary for launch mode
	Addr       string     // host:port for connect mode; empty means launch
	Durability Durability // fairness config for launch mode

	// AkiEngine and AkiNet select aki's storage engine and networking model for
	// the string point path (the -aki-engine and -aki-net server flags). They are
	// ignored for Redis and Valkey. The default engine is f3, the spec 2064/f3
	// sharded clean-room rewrite that is the product; it is served by the f3srv
	// binary, which takes a bare --addr with no subcommand and no persistence flags
	// (in-memory only), so its launch line is its own case below. f1raw is the prior
	// single-tier engine, served by the f1srv binary, which accepts the same
	// `server --addr ...` flag shape as the aki binary so that launch path is
	// identical. The legacy engines (btree, hybrid, hot) are slower and served by
	// the aki binary; the caller picks the matching binary so a baseline never
	// silently measures the wrong path. The sqlo1 engine is the spec 2064/sqlo1
	// driver, served by the sqlo1srv binary, whose S0 flag surface is just -addr;
	// it too gets its own case.
	AkiEngine string
	AkiNet    string

	// AkiExtraArgs are extra server flags appended verbatim to a launched aki or
	// f1srv command, after the durability and engine flags. It is how a scenario
	// engages an opt-in server path the default launch would leave off: the set
	// campaign passes -set-algebra-merge here so the launched f1srv maintains the
	// per-set sorted member-hash arrays and answers SINTER/SDIFF/SINTERCARD through
	// the doc-24 two-pointer merge rather than the point-probe, which is the path
	// the 2x gate is meant to measure. It is ignored for Redis and Valkey, which
	// take their own flags, and in connect mode, where the server is already up.
	AkiExtraArgs []string

	// CPUList, when set, pins the launched server to those cores via taskset (a
	// Linux taskset -c list such as "0-3"). It is applied identically to aki,
	// Redis, and Valkey so the comparison stays fair, and it is the server half of
	// the CPU partition that keeps the co-located load generator from stealing the
	// server's cores. It is ignored in connect mode and on non-Linux platforms.
	CPUList string
}

// Target is a running or reachable server.
type Target struct {
	Kind    Kind
	Addr    string
	cmd     *exec.Cmd
	dataDir string
}

// SkipError reports that a target could not be provided for a benign reason,
// most often a missing binary. The harness treats it as "skip this target"
// rather than a hard failure.
type SkipError struct {
	Kind   Kind
	Reason string
}

func (e *SkipError) Error() string {
	return fmt.Sprintf("target %s skipped: %s", e.Kind, e.Reason)
}

// IsSkip reports whether err is a SkipError.
func IsSkip(err error) bool {
	var se *SkipError
	return errors.As(err, &se)
}

// Provide returns a ready Target for the spec. In connect mode it verifies the
// address is reachable. In launch mode it spawns the binary, or returns a
// SkipError if the binary is not on PATH.
func Provide(s Spec) (*Target, error) {
	if s.Addr != "" {
		if err := waitReachable(s.Addr, 2*time.Second); err != nil {
			return nil, &SkipError{Kind: s.Kind, Reason: "address " + s.Addr + " not reachable"}
		}
		return &Target{Kind: s.Kind, Addr: s.Addr}, nil
	}
	return launch(s)
}

// launchAttempts is how many times launch tries to bring a server up before it
// gives up. A single try is fragile on a busy multi-cell sweep: a transient load
// spike or a brief memory-pressure window can push one server's startup past the
// reachability deadline, and one such miss would abort the whole matrix. A second
// attempt on a fresh port and data directory rides through the blip; a real
// misconfiguration still fails both times and surfaces the captured startup log.
const launchAttempts = 2

func launch(s Spec) (*Target, error) {
	bin := s.Binary
	if bin == "" {
		bin = defaultBinary(s.Kind)
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, &SkipError{Kind: s.Kind, Reason: bin + " not found on PATH"}
	}

	var lastErr error
	for attempt := 1; attempt <= launchAttempts; attempt++ {
		t, err := launchOnce(s, resolved)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// launchOnce makes one attempt to spawn and reach a server. On a reachability
// failure it includes the tail of the child's own startup output in the error, so
// a real failure (a rejected data file, a bad flag, an OOM kill) is diagnosable
// instead of a bare "connection refused". Startup output is otherwise discarded.
func launchOnce(s Spec, resolved string) (*Target, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", s.Kind, err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	dataDir, err := os.MkdirTemp("", "aki-bench-"+string(s.Kind)+"-")
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", s.Kind, err)
	}

	args := launchArgs(s.Kind, port, dataDir, s.Durability, s.AkiEngine, s.AkiNet, s.AkiExtraArgs)
	runBin, runArgs := cpuset.ServerWrap(s.CPUList, resolved, args)
	cmd := exec.Command(runBin, runArgs...)
	cmd.Dir = dataDir
	// Capture the child's startup output so a failed launch can report why. The
	// buffer is only read on the error path, and startup output is a few short
	// lines, so an unbounded buffer here holds at most a screenful.
	var startup bytes.Buffer
	cmd.Stdout = &startup
	cmd.Stderr = &startup
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("target %s: start: %w", s.Kind, err)
	}

	if err := waitReachable(addr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("target %s: did not come up on %s: %w%s", s.Kind, addr, err, startupTail(&startup))
	}

	return &Target{Kind: s.Kind, Addr: addr, cmd: cmd, dataDir: dataDir}, nil
}

// startupTail renders the last few lines of a failed server's startup output for
// inclusion in an error, or an empty string when the child printed nothing.
func startupTail(buf *bytes.Buffer) string {
	const max = 512
	b := buf.Bytes()
	if len(b) == 0 {
		return ""
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return "\nstartup output:\n" + string(bytes.TrimRight(b, "\n"))
}

// RSSBytes returns the launched server's current resident set size in bytes,
// or 0 when it cannot be measured: connect mode has no PID to read, and off
// Linux there is no /proc. RSS is half of the F14 memory column (doc 18
// section 1.5): it travels next to used_memory so allocator slack or an
// unsettled arena cannot hide behind the server's own accounting, and it is
// the only memory figure available for a server that does not serve INFO.
func (t *Target) RSSBytes() int64 {
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return rssBytes(t.cmd.Process.Pid)
}

// PeakRSSBytes returns the launched server's peak resident set size in bytes
// (VmHWM), or 0 when it cannot be measured for the same reasons RSSBytes cannot:
// connect mode has no PID, and off Linux there is no /proc. Peak travels next to
// steady RSS because the LTM pitch is a memory-ceiling claim, and a peak that
// spikes above a rival during load breaks that claim even when the settled RSS
// is lower.
func (t *Target) PeakRSSBytes() int64 {
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return hwmBytes(t.cmd.Process.Pid)
}

// Close stops a launched server and removes its data directory. It is a no-op
// for a connect-mode target. The Wait after Kill is bounded by a timeout so a
// process wedged in uninterruptible I/O cannot hang the harness: across a
// multi-cell sweep one stuck Close would otherwise stall every later cell.
func (t *Target) Close() error {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		done := make(chan struct{})
		go func() {
			_ = t.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if t.dataDir != "" {
		_ = os.RemoveAll(t.dataDir)
	}
	return nil
}

func defaultBinary(k Kind) string {
	switch k {
	case Aki:
		return "aki"
	case Redis:
		return "redis-server"
	case Valkey:
		return "valkey-server"
	default:
		return string(k)
	}
}

// launchArgs builds the command-line flags that put each server in the requested
// fairness config on the given port and data directory. akiEngine and akiNet,
// when non-empty, select aki's string-path storage engine and networking model;
// they are ignored for Redis and Valkey. akiExtra is appended verbatim after the
// aki engine/net flags, the passthrough a scenario uses to engage an opt-in server
// path such as -set-algebra-merge; it too is ignored for Redis and Valkey.
func launchArgs(k Kind, port int, dataDir string, d Durability, akiEngine, akiNet string, akiExtra []string) []string {
	p := strconv.Itoa(port)
	switch k {
	case Aki:
		// f3srv is the spec 2064/f3 rewrite's binary. It has no server subcommand
		// and no persistence flags yet: the whole flag surface in M0 is --addr,
		// --shards, and --arena-mib. It runs with its shipped defaults per the
		// fairness rule (doc 18 section 6.4: the product runs what a user gets),
		// and the data dir is only its working directory, set by the launcher.
		if akiEngine == "f3" {
			return []string{"--addr", "127.0.0.1:" + p}
		}
		// sqlo1srv is the spec 2064/sqlo1 driver's binary. In S0 its whole flag
		// surface is -addr and -store, the store is the in-memory placeholder,
		// and it writes nothing, so the data dir is only its working directory.
		// When the single-file store lands the file will appear under that
		// directory and the launcher's disk accounting picks it up unchanged.
		if akiEngine == "sqlo1" {
			return []string{"-addr", "127.0.0.1:" + p}
		}
		// aki's server subcommand takes a listen address and a working directory,
		// and it speaks the same appendonly and appendfsync flags as Redis. Mapping
		// durability through those flags keeps the fairness config identical across
		// all three servers.
		args := []string{"server", "--addr", "127.0.0.1:" + p, "--dir", dataDir}
		if d == Durable {
			args = append(args, "--appendonly", "yes", "--appendfsync", "always")
		} else {
			args = append(args, "--appendonly", "no")
		}
		if akiEngine != "" {
			args = append(args, "--aki-engine", akiEngine)
		}
		if akiNet != "" {
			args = append(args, "--aki-net", akiNet)
		}
		args = append(args, akiExtra...)
		return args
	case Redis, Valkey:
		args := []string{
			"--port", p,
			"--dir", dataDir,
			"--bind", "127.0.0.1",
			"--save", "",
		}
		if d == Durable {
			args = append(args, "--appendonly", "yes", "--appendfsync", "always")
		} else {
			args = append(args, "--appendonly", "no")
		}
		return args
	default:
		return []string{"--port", p}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitReachable(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
