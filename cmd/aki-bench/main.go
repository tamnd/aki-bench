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
	"strings"
	"time"

	"github.com/tamnd/aki-bench/load"
	"github.com/tamnd/aki-bench/report"
	"github.com/tamnd/aki-bench/smoke"
	"github.com/tamnd/aki-bench/target"
	"github.com/tamnd/aki-bench/workload"
)

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
		akiBin     = fs.String("aki-bin", "aki", "aki binary for launch mode")
		redisBin   = fs.String("redis-bin", "redis-server", "redis binary for launch mode")
		valkeyBin  = fs.String("valkey-bin", "valkey-server", "valkey binary for launch mode")
		akiEngine  = fs.String("aki-engine", "hot", "aki string-path engine in launch mode: btree, hybrid, or hot (default; the engine aki is optimized for, so a baseline never silently measures the old B-tree)")
		akiNet     = fs.String("aki-net", "", "aki networking model in launch mode: empty for the default goroutine loop, reactor, or uring (Linux only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	duration, err := time.ParseDuration(*durationStr)
	if err != nil {
		return fmt.Errorf("bad -duration: %w", err)
	}

	dur := target.InMemory
	if *durable {
		dur = target.Durable
	}

	// The hot and hybrid engines are in-memory only in this slice, so a durable
	// comparison on them would pit a non-durable aki against fsync-per-write Redis
	// and Valkey, which is not a fair durable-vs-durable number. In durable mode
	// fall back to the durable B-tree and say so, rather than quietly reporting an
	// unfair win.
	engine := *akiEngine
	if dur == target.Durable && (engine == "hot" || engine == "hybrid") {
		fmt.Fprintf(os.Stderr, "durable mode: %s engine is in-memory only, using btree for a fair durable comparison\n", engine)
		engine = "btree"
	}

	specs := []target.Spec{
		{Kind: target.Aki, Binary: *akiBin, Addr: *akiAddr, Durability: dur, AkiEngine: engine, AkiNet: *akiNet},
		{Kind: target.Redis, Binary: *redisBin, Addr: *redisAddr, Durability: dur},
		{Kind: target.Valkey, Binary: *valkeyBin, Addr: *valkeyAddr, Durability: dur},
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
		return runSmoke(targets)
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
		res, err := load.Run(context.Background(), cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run %s: %v\n", k, err)
			return report.Skipped(name, *wl)
		}
		return report.FromResult(name, *wl, res)
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

// allWorkloadNames lists every selectable workload: the flat generators plus the
// collection point-read plans.
func allWorkloadNames() []string {
	return append(append([]string{}, workload.Names()...), workload.PlanNames()...)
}

func runSmoke(targets map[target.Kind]*target.Target) error {
	names := map[target.Kind]string{target.Aki: "aki", target.Redis: "redis", target.Valkey: "valkey"}
	allPass := true
	for _, k := range []target.Kind{target.Aki, target.Redis, target.Valkey} {
		t, ok := targets[k]
		if !ok {
			continue
		}
		res := smoke.Run(names[k], t.Addr)
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
