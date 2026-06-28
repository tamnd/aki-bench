// Package report turns load results into a side-by-side comparison and the 2x
// acceptance gate. It prints a human table and emits JSON for CI artifacts.
// The gate is the single function that decides whether aki met its goal: aki must
// be at least 2x the throughput of both the current Redis and Valkey on the same
// workload, and no worse on tail latency. Each entry carries the server's
// self-reported version so the verdict names the exact builds it was measured
// against.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/tamnd/aki-bench/load"
)

// Entry is one target's measured result for a workload.
type Entry struct {
	Target    string  `json:"target"`            // aki, redis, or valkey
	Version   string  `json:"version,omitempty"` // server's self-reported identity, e.g. "redis 8.8.0"
	Workload  string  `json:"workload"`          // workload name
	Skipped   bool    `json:"skipped"`           // true when the target was absent
	Ops       int64   `json:"ops"`               // successful operations
	Errors    int64   `json:"errors"`            // failed operations
	OpsPerSec float64 `json:"ops_per_sec"`
	P50us     float64 `json:"p50_us"` // latency percentiles in microseconds
	P99us     float64 `json:"p99_us"`
	P999us    float64 `json:"p999_us"`
}

// WithVersion returns the entry tagged with a server's self-reported identity,
// so the comparison records the exact version measured rather than the binary
// name the operator launched. A run that benches an old redis-server on PATH and
// labels it the target version is the failure this closes.
func (e Entry) WithVersion(v string) Entry {
	e.Version = v
	return e
}

// FromResult builds an Entry from a load.Result.
func FromResult(targetName, workload string, r load.Result) Entry {
	return Entry{
		Target:    targetName,
		Workload:  workload,
		Ops:       r.Ops,
		Errors:    r.Errors,
		OpsPerSec: r.OpsPerSec(),
		P50us:     toMicros(r.Hist.ValueAtPercentile(50)),
		P99us:     toMicros(r.Hist.ValueAtPercentile(99)),
		P999us:    toMicros(r.Hist.ValueAtPercentile(99.9)),
	}
}

// Skipped builds an Entry marking a target that was not run.
func Skipped(targetName, workload string) Entry {
	return Entry{Target: targetName, Workload: workload, Skipped: true}
}

func toMicros(ns int64) float64 { return float64(ns) / 1000.0 }

// Comparison holds the three target entries for one workload plus the gate
// verdict.
type Comparison struct {
	Workload  string    `json:"workload"`
	Timestamp time.Time `json:"timestamp"`
	Aki       Entry     `json:"aki"`
	Redis     Entry     `json:"redis"`
	Valkey    Entry     `json:"valkey"`
	Gate      Gate      `json:"gate"`
}

// Gate is the 2x acceptance verdict.
type Gate struct {
	Pass               bool    `json:"pass"`
	Reason             string  `json:"reason"`
	Required           float64 `json:"required_speedup"` // the multiplier aki must beat
	SpeedupVsRedis     float64 `json:"speedup_vs_redis"`
	SpeedupVsValkey    float64 `json:"speedup_vs_valkey"`
	TailRegressedRedis bool    `json:"tail_regressed_vs_redis"`
	TailRegressedValk  bool    `json:"tail_regressed_vs_valkey"`
}

// DefaultRequiredSpeedup is the project goal: 2x the current Redis and Valkey.
const DefaultRequiredSpeedup = 2.0

// EvaluateGate is the exact acceptance gate.
// It returns pass only when aki, Redis, and Valkey were all measured (none
// skipped), aki's throughput is at least required times each of Redis and
// Valkey, and aki's p99 latency is not worse than either competitor on the same
// workload.
// Requiring all three present is deliberate: a gate that "passes" because a
// competitor was missing would be meaningless.
func EvaluateGate(aki, redis, valkey Entry, required float64) Gate {
	g := Gate{Required: required}

	if aki.Skipped || redis.Skipped || valkey.Skipped {
		g.Pass = false
		g.Reason = "one or more targets were skipped, gate needs aki, redis, and valkey present"
		return g
	}
	if aki.OpsPerSec <= 0 || redis.OpsPerSec <= 0 || valkey.OpsPerSec <= 0 {
		g.Pass = false
		g.Reason = "a target reported zero throughput"
		return g
	}

	g.SpeedupVsRedis = aki.OpsPerSec / redis.OpsPerSec
	g.SpeedupVsValkey = aki.OpsPerSec / valkey.OpsPerSec
	g.TailRegressedRedis = aki.P99us > redis.P99us
	g.TailRegressedValk = aki.P99us > valkey.P99us

	switch {
	case g.SpeedupVsRedis < required:
		g.Reason = fmt.Sprintf("aki is %.2fx Redis, below the %.1fx bar", g.SpeedupVsRedis, required)
	case g.SpeedupVsValkey < required:
		g.Reason = fmt.Sprintf("aki is %.2fx Valkey, below the %.1fx bar", g.SpeedupVsValkey, required)
	case g.TailRegressedRedis:
		g.Reason = "aki p99 latency is worse than Redis"
	case g.TailRegressedValk:
		g.Reason = "aki p99 latency is worse than Valkey"
	default:
		g.Pass = true
		g.Reason = fmt.Sprintf("aki is %.2fx Redis and %.2fx Valkey with no tail regression",
			g.SpeedupVsRedis, g.SpeedupVsValkey)
	}
	return g
}

// NewComparison assembles a Comparison and runs the gate.
func NewComparison(workload string, aki, redis, valkey Entry, required float64) Comparison {
	return Comparison{
		Workload:  workload,
		Timestamp: time.Now().UTC(),
		Aki:       aki,
		Redis:     redis,
		Valkey:    valkey,
		Gate:      EvaluateGate(aki, redis, valkey, required),
	}
}

// WriteTable prints the side-by-side comparison as an aligned text table.
func (c Comparison) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "workload: %s\n", c.Workload)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "target\tversion\tops/sec\tp50 us\tp99 us\tp999 us\tnote")
	for _, e := range []Entry{c.Aki, c.Redis, c.Valkey} {
		ver := e.Version
		if ver == "" {
			ver = "-"
		}
		if e.Skipped {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\tskipped\n", e.Target, ver)
			continue
		}
		note := ""
		if e.Errors > 0 {
			note = fmt.Sprintf("%d errors", e.Errors)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.0f\t%.1f\t%.1f\t%.1f\t%s\n",
			e.Target, ver, e.OpsPerSec, e.P50us, e.P99us, e.P999us, note)
	}
	tw.Flush()

	if !c.Aki.Skipped && !c.Redis.Skipped {
		fmt.Fprintf(w, "speedup vs redis:  %.2fx\n", c.Gate.SpeedupVsRedis)
	}
	if !c.Aki.Skipped && !c.Valkey.Skipped {
		fmt.Fprintf(w, "speedup vs valkey: %.2fx\n", c.Gate.SpeedupVsValkey)
	}
	verdict := "FAIL"
	if c.Gate.Pass {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "gate (%.1fx): %s, %s\n", c.Gate.Required, verdict, c.Gate.Reason)
}

// WriteJSON emits the comparison as indented JSON for CI artifacts.
func (c Comparison) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}
