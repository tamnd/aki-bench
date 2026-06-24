// Package target launches or connects to the servers under test: aki,
// redis-server, and valkey-server. Two modes are supported. In connect mode the
// caller points at an already-running host:port. In launch mode the package
// spawns the server binary on a free port, waits for it to accept connections,
// and stops it on Close. When a binary is absent the launcher reports a skip so
// the harness can carry on with the targets that are present.
package target

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"
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

func launch(s Spec) (*Target, error) {
	bin := s.Binary
	if bin == "" {
		bin = defaultBinary(s.Kind)
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, &SkipError{Kind: s.Kind, Reason: bin + " not found on PATH"}
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", s.Kind, err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	dataDir, err := os.MkdirTemp("", "aki-bench-"+string(s.Kind)+"-")
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", s.Kind, err)
	}

	args := launchArgs(s.Kind, port, dataDir, s.Durability)
	cmd := exec.Command(resolved, args...)
	cmd.Dir = dataDir
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("target %s: start: %w", s.Kind, err)
	}

	if err := waitReachable(addr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("target %s: did not come up on %s: %w", s.Kind, addr, err)
	}

	return &Target{Kind: s.Kind, Addr: addr, cmd: cmd, dataDir: dataDir}, nil
}

// Close stops a launched server and removes its data directory. It is a no-op
// for a connect-mode target.
func (t *Target) Close() error {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
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
// fairness config on the given port and data directory.
func launchArgs(k Kind, port int, dataDir string, d Durability) []string {
	p := strconv.Itoa(port)
	switch k {
	case Aki:
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
