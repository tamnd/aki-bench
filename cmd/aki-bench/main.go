// Command aki-bench runs a named workload against aki, Redis, and Valkey and
// prints a side-by-side comparison plus the 2x gate verdict. It can launch the
// servers itself or connect to addresses already running, emit JSON for CI, and
// run the compatibility smoke check instead of a load run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/aki-bench/cpuset"
	"github.com/tamnd/aki-bench/load"
	"github.com/tamnd/aki-bench/report"
	"github.com/tamnd/aki-bench/smoke"
	"github.com/tamnd/aki-bench/target"
	"github.com/tamnd/aki-bench/workload"
)

// defaultClientGOGC is the GC target the load generator runs itself at unless the
// operator overrides it. This is not a server tuning knob: it tunes the benchmark
// client, which is a Go process, and under the runtime default of 100 that process
// becomes the bottleneck on the very workloads it is supposed to measure. At P16
// pipeline the windowed range and scan replies (LRANGE, ZRANGE, HSCAN) land tens
// of thousands of reply buffers per second in the client, the collector fires
// often enough to draft the reading goroutines into GC assist, and the client
// caps out below what the server can serve while its own p99 balloons. That stall
// silently penalizes the fastest target: it under-reports aki's throughput and
// inflates aki's tail, while a server-bound target like Redis (~350k ops/sec,
// well under the client ceiling) is untouched, so the same run measures the two
// on different instruments. The fix is to keep the measuring instrument off the
// critical path. With the client held here the same manually launched servers
// report aki at its true rate (a 200k-member LRANGE P16 cell moves from a
// client-limited 2.5x with a p99 tail regression to a server-limited 3.5x with no
// regression), and Redis and Valkey do not move because they were never
// client-limited. 2000 matches the f1srv server default and leaves ample headroom
// over the bounded reply buffers a bench run allocates, so there is no OOM risk
// across the short measured window.
const defaultClientGOGC = 2000

// tuneClientGC raises the load generator's own GC target so the benchmark client
// never becomes the bottleneck it is trying to measure. An explicit GOGC in the
// environment wins (the runtime already honored it at startup and an operator who
// set it means it), and an explicit -client-gogc on the command line wins over
// both. It returns the effective target for the startup banner. See
// defaultClientGOGC for why this is a correctness fix, not a thumb on the scale.
func tuneClientGC(flagVal int, flagExplicit bool) int {
	if envVal, envSet := os.LookupEnv("GOGC"); envSet && !flagExplicit {
		if n, err := strconv.Atoi(envVal); err == nil {
			return n
		}
		return flagVal
	}
	debug.SetGCPercent(flagVal)
	return flagVal
}

// errGateNotMet signals that the run completed but the 2x speedup gate failed.
// It maps to exit code 2. It is returned rather than calling os.Exit inside run
// so that run's deferred Close calls actually fire: os.Exit skips defers, which
// would orphan every launched aki/redis/valkey trio (a leak that piles up fast
// across a multi-cell sweep, since most cells fail the gate).
var errGateNotMet = errors.New("gate not met")

func main() {
	err := run(os.Args[1:])
	if errors.Is(err, errGateNotMet) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aki-bench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("aki-bench", flag.ContinueOnError)
	var (
		wl          = fs.String("workload", "set", "workload name: "+strings.Join(allWorkloadNames(), ", "))
		conns       = fs.Int("connections", 50, "concurrent connections")
		pipeline    = fs.Int("pipeline", 1, "pipeline depth per connection")
		durationStr = fs.String("duration", "5s", "run length, for example 10s; set to 0 to use -requests")
		requests    = fs.Int64("requests", 0, "total requests when duration is 0")
		valueSize   = fs.Int("value-size", 64, "write payload size in bytes")
		keyCount    = fs.Int("keys", 100000, "key space size")
		members     = fs.Int("members", 0, "member space for collection point-read workloads; defaults to -keys")
		dist        = fs.String("dist", "uniform", "access pattern over the space: uniform or zipfian")
		zipfS       = fs.Float64("zipf-s", 0.99, "zipfian skew exponent when -dist is zipfian")
		readRatio   = fs.Int("read-ratio", 80, "read percent for the mixed workload")
		openLoop    = fs.Bool("open-loop", false, "open-loop mode with coordinated-omission correction")
		targetRate  = fs.Int64("rate", 0, "open-loop aggregate target ops/sec")
		durable     = fs.Bool("durable", false, "launch servers in durable config instead of in-memory")
		required    = fs.Float64("gate", report.DefaultRequiredSpeedup, "required speedup over Redis and Valkey")
		jsonOut     = fs.String("json", "", "write the comparison JSON to this file")
		smokeOnly   = fs.Bool("smoke", false, "run the compatibility smoke check instead of a load run")

		akiAddr    = fs.String("aki-addr", "", "connect to a running aki at host:port instead of launching")
		redisAddr  = fs.String("redis-addr", "", "connect to a running redis at host:port instead of launching")
		valkeyAddr = fs.String("valkey-addr", "", "connect to a running valkey at host:port instead of launching")
		akiBin     = fs.String("aki-bin", "aki", "aki binary for launch mode with a legacy engine (btree, hybrid, hot)")
		f1srvBin   = fs.String("f1srv-bin", "f1srv", "f1srv binary for launch mode with the f1raw engine (the default)")
		f3srvBin   = fs.String("f3srv-bin", "f3srv", "f3srv binary for launch mode with the f3 engine (the spec 2064/f3 rewrite)")
		redisBin   = fs.String("redis-bin", "redis-server", "redis binary for launch mode")
		valkeyBin  = fs.String("valkey-bin", "valkey-server", "valkey binary for launch mode")
		akiEngine  = fs.String("aki-engine", "f1raw", "aki engine in launch mode: f1raw (default, the fast clean-room single-tier engine served by f1srv; this is the product), f3 (the spec 2064/f3 rewrite served by f3srv; strings only in M0, becomes the product at the M0 gate run), or a legacy slower engine (btree, hybrid, hot) served by the aki binary")
		akiNet     = fs.String("aki-net", "", "aki networking model in launch mode: empty for the default goroutine loop, reactor, or uring (Linux only)")

		cpuSplit  = fs.Bool("cpu-split", true, "partition cores so the launched server and the load generator never share a core (Linux launch mode, on by default); removes the co-located-client starvation that understates the ratio; pass -cpu-split=false to co-locate")
		cpuServer = fs.String("cpu-server", "", "taskset -c list for the server half of -cpu-split, for example 0-3; empty auto-derives from the core count")
		cpuClient = fs.String("cpu-client", "", "taskset -c list for the load-generator half of -cpu-split, for example 4-5; empty auto-derives from the core count")

		clientGOGC = fs.Int("client-gogc", defaultClientGOGC, "GC target for this load generator so the measuring client never becomes the bottleneck (see the range/scan rationale in code); an explicit GOGC in the environment wins")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Tune the client's own GC before any work so the measuring instrument stays
	// off the critical path. This must happen before preload and the measured run,
	// which is where the client's allocation pressure lives.
	clientGOGCExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "client-gogc" {
			clientGOGCExplicit = true
		}
	})
	effClientGOGC := tuneClientGC(*clientGOGC, clientGOGCExplicit)
	fmt.Fprintf(os.Stderr, "aki-bench: load-generator gogc=%d\n", effClientGOGC)

	duration, err := time.ParseDuration(*durationStr)
	if err != nil {
		return fmt.Errorf("bad -duration: %w", err)
	}

	dur := target.InMemory
	if *durable {
		dur = target.Durable
	}

	// f1raw, f3, hot, and hybrid are in-memory only in this slice, so a durable
	// comparison on them would pit a non-durable aki against fsync-per-write Redis
	// and Valkey, which is not a fair durable-vs-durable number. In durable mode
	// fall back to the durable B-tree and say so, rather than quietly reporting an
	// unfair win.
	engine := *akiEngine
	if dur == target.Durable && (engine == "f1raw" || engine == "f3" || engine == "hot" || engine == "hybrid") {
		fmt.Fprintf(os.Stderr, "durable mode: %s engine is in-memory only, using btree for a fair durable comparison\n", engine)
		engine = "btree"
	}

	// f1raw is the default fast engine and it is served by its own binary, f1srv,
	// not the aki binary. The f3 rewrite is likewise served by its own binary,
	// f3srv. The legacy engines (btree, hybrid, hot) are served by the aki binary
	// through its --aki-engine flag. Pick the binary that matches the engine so
	// launch mode never points the wrong server at the engine name.
	akiServerBin := *akiBin
	switch engine {
	case "f1raw":
		akiServerBin = *f1srvBin
	case "f3":
		akiServerBin = *f3srvBin
	}

	// Pin the launched server and this load generator to disjoint cores when
	// asked. On a co-located run the Go client and the server otherwise fight for
	// the same cores, which starves the server and understates the ratio. The
	// server half is stamped into every spec so aki, Redis, and Valkey are all
	// measured under the same partition.
	launching := *akiAddr == "" || *redisAddr == "" || *valkeyAddr == ""
	serverCPUs := applyCPUSplit(*cpuSplit, *cpuServer, *cpuClient, launching)

	specs := []target.Spec{
		{Kind: target.Aki, Binary: akiServerBin, Addr: *akiAddr, Durability: dur, AkiEngine: engine, AkiNet: *akiNet, CPUList: serverCPUs},
		{Kind: target.Redis, Binary: *redisBin, Addr: *redisAddr, Durability: dur, CPUList: serverCPUs},
		{Kind: target.Valkey, Binary: *valkeyBin, Addr: *valkeyAddr, Durability: dur, CPUList: serverCPUs},
	}

	targets := map[target.Kind]*target.Target{}
	for _, s := range specs {
		t, err := target.Provide(s)
		if err != nil {
			if target.IsSkip(err) {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", s.Kind, err)
				continue
			}
			return err
		}
		targets[s.Kind] = t
		defer t.Close()
	}
	if len(targets) == 0 {
		return fmt.Errorf("no targets available, install aki/redis-server/valkey-server or pass -*-addr")
	}

	if *smokeOnly {
		// f3srv serves the M0 string surface only, so the smoke sticks to string
		// probes. The same reduced set runs against every present target so all
		// three are checked on identical round-trips.
		return runSmoke(targets, engine == "f3")
	}

	spec := workload.Spec{
		Name:      *wl,
		ValueSize: *valueSize,
		KeyCount:  *keyCount,
		Members:   *members,
		Dist:      *dist,
		ZipfS:     *zipfS,
		ReadRatio: *readRatio,
	}
	if *dist != "uniform" && *dist != "zipfian" {
		return fmt.Errorf("unknown -dist %q, choose uniform or zipfian", *dist)
	}

	// A workload is either a flat generator (get/set/...) or a collection
	// point-read plan (sismember/hget/zscore/zrank). A plan has a preload phase
	// that builds the single probed collection before the measured reads.
	gen := workload.Build(*wl, spec)
	plan, isPlan := workload.BuildPlan(*wl, spec)
	if gen == nil && !isPlan {
		return fmt.Errorf("unknown workload %q, choose one of %s", *wl, strings.Join(allWorkloadNames(), ", "))
	}

	mode := load.ClosedLoop
	if *openLoop {
		mode = load.OpenLoop
	}

	entry := func(k target.Kind, name string) report.Entry {
		t, ok := targets[k]
		if !ok {
			return report.Skipped(name, *wl)
		}
		// Start every target from an identical empty keyspace. When aki-bench
		// launches its own servers they are already empty, but when it connects to a
		// persistent reference (a redis or valkey left running across runs via
		// -redis-addr/-valkey-addr) that server still holds the previous run's keys.
		// A hgetall run that preloads a 100-field hash onto a reference that already
		// holds a 100k-field hash from a prior sweep then measures aki against a hash
		// two orders larger, a silent apples-to-oranges comparison. Flushing first
		// makes the preload authoritative for the shape every target is measured on.
		if err := flushTarget(t.Addr); err != nil {
			fmt.Fprintf(os.Stderr, "flush %s: %v\n", k, err)
			return report.Skipped(name, *wl)
		}
		// For a collection plan, build the probed collection first. The preload
		// runs on one connection so a single sequence 0..PreloadOps-1 populates
		// every member exactly once; a multi-connection preload would restart the
		// sequence per connection and under-populate the collection.
		probe := gen
		if isPlan {
			pre := load.Config{
				Addr:        t.Addr,
				Connections: 1,
				Pipeline:    256,
				Duration:    0,
				Requests:    plan.PreloadOps,
				Mode:        load.ClosedLoop,
				Gen:         plan.Preload,
			}
			if _, err := load.Run(context.Background(), pre); err != nil {
				fmt.Fprintf(os.Stderr, "preload %s: %v\n", k, err)
				return report.Skipped(name, *wl)
			}
			probe = plan.Probe
		} else if preGen, preOps, ok := workload.PreloadFor(*wl, spec); ok {
			// Flat read workloads (get, mixed) need their key space populated, or
			// every read is a miss that short-circuits before storage. Walk the key
			// space once on a single connection so every key exists, then time the
			// reads against a warm store, matching the collection-plan preload.
			pre := load.Config{
				Addr:        t.Addr,
				Connections: 1,
				Pipeline:    256,
				Duration:    0,
				Requests:    preOps,
				Mode:        load.ClosedLoop,
				Gen:         preGen,
			}
			if _, err := load.Run(context.Background(), pre); err != nil {
				fmt.Fprintf(os.Stderr, "preload %s: %v\n", k, err)
				return report.Skipped(name, *wl)
			}
		}
		cfg := load.Config{
			Addr:        t.Addr,
			Connections: *conns,
			Pipeline:    *pipeline,
			Duration:    duration,
			Requests:    *requests,
			Mode:        mode,
			TargetRate:  *targetRate,
			Gen:         probe,
		}
		// Probe the live server's self-reported version before the load so the
		// report records the exact build measured, not the binary name on PATH.
		// A failed probe is not fatal: it just leaves the version blank.
		var version string
		if info, perr := load.ProbeServer(t.Addr, 2*time.Second); perr == nil {
			version = info.String()
		} else {
			fmt.Fprintf(os.Stderr, "version probe %s: %v\n", k, perr)
		}
		res, err := load.Run(context.Background(), cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run %s: %v\n", k, err)
			return report.Skipped(name, *wl).WithVersion(version)
		}
		return report.FromResult(name, *wl, res).WithVersion(version)
	}

	cmp := report.NewComparison(*wl,
		entry(target.Aki, "aki"),
		entry(target.Redis, "redis"),
		entry(target.Valkey, "valkey"),
		*required,
	)
	cmp.WriteTable(os.Stdout)

	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := cmp.WriteJSON(f); err != nil {
			return err
		}
	}

	if !cmp.Gate.Pass {
		return errGateNotMet
	}
	return nil
}

// cpuServerEnv carries the chosen server core list across the re-exec that pins
// the client. After taskset re-execs the process the machine view shrinks to the
// client cores, so the partition is decided once on the first pass and the server
// half is read back from this variable on the pinned pass.
const cpuServerEnv = "AKIBENCH_CPU_SERVER"

// applyCPUSplit partitions the machine between the launched server and this load
// generator and returns the taskset -c list the server should run on, or empty
// when no split applies. On the first pass it decides the partition from the full
// core count, records the server half, and re-execs this process pinned to the
// client half; that re-exec does not return on success. On the pinned pass it
// reads the server half back and returns it. A pin failure degrades to an
// unpinned run rather than aborting.
// flushTarget clears a target's keyspace with a single FLUSHALL, so every run
// starts from the same empty state whether the server was just launched or is a
// persistent reference carrying a prior run's keys. It opens one short-lived
// connection, sends the command, and waits for the reply so the flush is complete
// before preload begins. A server-side error reply is tolerated by ReadReply's
// contract (an error reply is a value, not a transport failure), which is the
// right call for f3srv: it does not serve FLUSHALL yet, and in launch mode it
// starts empty, so the flush it rejects is a no-op anyway.
func flushTarget(addr string) error {
	cl, err := load.Dial(addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := cl.WriteCommand([][]byte{[]byte("FLUSHALL")}); err != nil {
		return err
	}
	if err := cl.Flush(); err != nil {
		return err
	}
	return cl.ReadReply()
}

func applyCPUSplit(enabled bool, serverList, clientList string, launching bool) string {
	if !enabled || !launching || !cpuset.Available() {
		return ""
	}
	if v := os.Getenv(cpuServerEnv); v != "" {
		return v
	}
	n := runtime.NumCPU()
	if serverList == "" || clientList == "" {
		s, c, err := cpuset.Partition(n, cpuset.DefaultClientCores(n))
		if err != nil {
			fmt.Fprintf(os.Stderr, "cpu-split: %v, running unpinned\n", err)
			return ""
		}
		if serverList == "" {
			serverList = s
		}
		if clientList == "" {
			clientList = c
		}
	}
	fmt.Fprintf(os.Stderr, "cpu-split: server cores %s, client cores %s\n", serverList, clientList)
	_ = os.Setenv(cpuServerEnv, serverList)
	if _, err := cpuset.PinSelf(clientList); err != nil {
		fmt.Fprintf(os.Stderr, "cpu-split: pin failed: %v, running unpinned\n", err)
		_ = os.Unsetenv(cpuServerEnv)
		return ""
	}
	return serverList
}

// allWorkloadNames lists every selectable workload: the flat generators plus the
// collection point-read plans.
func allWorkloadNames() []string {
	return append(append([]string{}, workload.Names()...), workload.PlanNames()...)
}

func runSmoke(targets map[target.Kind]*target.Target, stringsOnly bool) error {
	names := map[target.Kind]string{target.Aki: "aki", target.Redis: "redis", target.Valkey: "valkey"}
	allPass := true
	for _, k := range []target.Kind{target.Aki, target.Redis, target.Valkey} {
		t, ok := targets[k]
		if !ok {
			continue
		}
		var res smoke.Result
		if stringsOnly {
			res = smoke.RunStrings(names[k], t.Addr)
		} else {
			res = smoke.Run(names[k], t.Addr)
		}
		status := "PASS"
		if !res.Pass() {
			status = "FAIL"
			allPass = false
		}
		fmt.Printf("%s smoke: %s\n", res.Target, status)
		for _, c := range res.Checks {
			mark := "ok"
			if !c.OK {
				mark = "FAIL " + c.Detail
			}
			fmt.Printf("  %-12s %s\n", c.Name, mark)
		}
	}
	if !allPass {
		return errGateNotMet
	}
	return nil
}
