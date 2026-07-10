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
	MinUs     float64 `json:"min_us,omitempty"` // smallest recorded latency in microseconds
	P50us     float64 `json:"p50_us"`           // latency percentiles in microseconds
	P99us     float64 `json:"p99_us"`
	P999us    float64 `json:"p999_us"`

	// BytesPerSec is the wire bandwidth the run moved against this target, both
	// directions summed. It is the CF19/CF20 column: a giant-value cell where
	// every server sits at the box's copy ceiling is bandwidth-bound and its
	// ops/s parity is manufactured, so bytes/s travels with every row.
	BytesPerSec float64 `json:"bytes_per_sec"`

	// RespVer is the RESP protocol version the harness spoke to this target.
	// The load client never sends HELLO, so every connection stays on the RESP2
	// wire format for every server; recording the constant makes the CF19
	// matched-protocol readback explicit in the row instead of assumed by the
	// reader. If the client ever grows RESP3 negotiation this must become the
	// negotiated value read back from the HELLO reply, not a constant.
	RespVer int `json:"resp_ver"`

	// RSSBytes is the server process's resident set size probed right after the
	// measured window, and UsedMemory is the server's own INFO memory
	// accounting. Both are zero when they could not be measured (connect mode,
	// non-Linux for RSS; a server with no INFO for UsedMemory) and the JSON
	// omits them rather than reporting a fake zero footprint. BytesPerKey is
	// UsedMemory over the keyspace when both are known, the F14 per-entry
	// figure.
	RSSBytes    int64   `json:"rss_bytes,omitempty"`
	UsedMemory  int64   `json:"used_memory,omitempty"`
	BytesPerKey float64 `json:"bytes_per_key,omitempty"`
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
		Target:      targetName,
		Workload:    workload,
		Ops:         r.Ops,
		Errors:      r.Errors,
		OpsPerSec:   r.OpsPerSec(),
		MinUs:       toMicros(r.Hist.Min()),
		P50us:       toMicros(r.Hist.ValueAtPercentile(50)),
		P99us:       toMicros(r.Hist.ValueAtPercentile(99)),
		P999us:      toMicros(r.Hist.ValueAtPercentile(99.9)),
		BytesPerSec: r.BytesPerSec(),
		RespVer:     2, // the load client never negotiates RESP3; see the field doc
	}
}

// WithMemory returns the entry carrying the F14 memory columns: the server's
// RSS as probed after the window, its self-reported used_memory, and the
// derived bytes-per-key when the accounting figure and the keyspace are both
// known. A zero rss or usedMemory means "not measured" and leaves its column
// out of the JSON; the derived figure prefers used_memory (the
// apples-to-apples accounting number) and is never derived from RSS, whose
// allocator slack would inflate it.
func (e Entry) WithMemory(rss, usedMemory int64, keyspace int) Entry {
	e.RSSBytes = rss
	e.UsedMemory = usedMemory
	if usedMemory > 0 && keyspace > 0 {
		e.BytesPerKey = float64(usedMemory) / float64(keyspace)
	}
	return e
}

// Skipped builds an Entry marking a target that was not run.
func Skipped(targetName, workload string) Entry {
	return Entry{Target: targetName, Workload: workload, Skipped: true}
}

func toMicros(ns int64) float64 { return float64(ns) / 1000.0 }

// Cell is the workload-size tuple a comparison was measured at, the doc 18
// section 2.1 axes. f1 gated almost everything at one shape and the size axis
// stayed invisible until the postmortem; carrying the full tuple in every
// emitted row is what makes a band-matrix result quotable as the cell it
// actually measured. CardBand is the -card token as given (1, 10, 10k, 1M),
// empty when the run named its sizes with -keys/-members directly; Keyspace and
// Members are the effective counts either way, so the raw shape is always in
// the row.
type Cell struct {
	CardBand    string  `json:"card_band,omitempty"`
	Keyspace    int     `json:"keyspace"`
	Members     int     `json:"members,omitempty"`
	ValueSize   int     `json:"value_size"`
	Dist        string  `json:"dist"`
	ZipfS       float64 `json:"zipf_s,omitempty"` // set when Dist is zipfian
	Pipeline    int     `json:"pipeline"`
	Connections int     `json:"connections"`
}

// Comparison holds the three target entries for one workload plus the gate
// verdict.
type Comparison struct {
	Workload  string    `json:"workload"`
	Cell      Cell      `json:"cell"`
	Timestamp time.Time `json:"timestamp"`
	Aki       Entry     `json:"aki"`
	Redis     Entry     `json:"redis"`
	Valkey    Entry     `json:"valkey"`
	Gate      Gate      `json:"gate"`

	// GeneratorBound marks a closed-loop row where the load generator, not any
	// server, was the ceiling. Such a row measures the generator's capacity and
	// must never be quoted as a server capacity number. See DetectGeneratorBound
	// for the two conditions that set it; Note carries the evidence.
	GeneratorBound     bool   `json:"generator_bound"`
	GeneratorBoundNote string `json:"generator_bound_note,omitempty"`
}

// DefaultGeneratorBoundEpsilon is the relative throughput spread under which
// three closed-loop targets count as "all landed at the same ceiling". Ten
// percent is comfortably wider than run-to-run noise on a pinned box and far
// narrower than any real capacity gap the gate cares about.
const DefaultGeneratorBoundEpsilon = 0.10

// identityTolerance is how far the closed-loop identity may miss and still
// count as holding. In a saturated closed loop the fastest operation is one
// that never queued anywhere but the loop itself, so min latency times
// throughput reproduces the outstanding-request count almost exactly; on a
// server-bound run the minimum is a genuinely fast service time and the
// product lands far below outstanding. Half is a generous noise budget that
// still separates the two regimes by an order of magnitude in practice.
const identityTolerance = 0.5

// DetectGeneratorBound decides whether a closed-loop row hit the load
// generator's ceiling rather than any server's. Two independent signals must
// both fire:
//
//  1. All three targets' throughputs fall within epsilon of each other. Three
//     different servers do not tie by coincidence; they tie when something
//     upstream of all of them is the bottleneck.
//  2. The closed-loop latency identity holds on every target: the minimum
//     observed latency times the throughput reproduces the outstanding-request
//     count (connections times pipeline). When a closed loop saturates, even
//     the fastest operation spends its whole life waiting on the loop, so
//     min latency collapses to outstanding/throughput. A server-bound run
//     breaks the identity because its minimum is a real service time.
//
// outstanding is connections times pipeline depth. The returned note carries
// the numbers so the row explains itself. This is the check that catches the
// redis-benchmark P16 c512 rows from the f3 M0 gate: all targets at ~2.1M
// ops/s with min latency equal to outstanding/throughput, quoted as capacity
// while the same server did 4.21M under a faster generator.
func DetectGeneratorBound(aki, redis, valkey Entry, outstanding int, epsilon float64) (bool, string) {
	if aki.Skipped || redis.Skipped || valkey.Skipped || outstanding <= 0 {
		return false, ""
	}
	entries := []Entry{aki, redis, valkey}
	lo, hi := entries[0].OpsPerSec, entries[0].OpsPerSec
	for _, e := range entries {
		if e.OpsPerSec <= 0 || e.MinUs <= 0 {
			return false, ""
		}
		lo = min(lo, e.OpsPerSec)
		hi = max(hi, e.OpsPerSec)
	}

	spread := (hi - lo) / hi
	if spread > epsilon {
		return false, ""
	}

	for _, e := range entries {
		implied := e.MinUs / 1e6 * e.OpsPerSec // min latency in seconds times ops/s
		ratio := implied / float64(outstanding)
		if ratio < 1-identityTolerance || ratio > 1+identityTolerance {
			return false, ""
		}
	}

	return true, fmt.Sprintf(
		"all targets within %.1f%% (%.0f to %.0f ops/s) and min latency times throughput reproduces the %d outstanding requests on every target; the load generator saturated, not the servers",
		spread*100, lo, hi, outstanding)
}

// FlagGeneratorBound runs the generator-bound check against the row's own cell
// shape and stamps the verdict into the comparison. Call it only for a
// closed-loop run, after Cell is set: the identity it tests is a property of a
// closed loop, and an open-loop run at a target rate ties on purpose.
func (c *Comparison) FlagGeneratorBound(epsilon float64) {
	outstanding := c.Cell.Connections * c.Cell.Pipeline
	c.GeneratorBound, c.GeneratorBoundNote = DetectGeneratorBound(c.Aki, c.Redis, c.Valkey, outstanding, epsilon)
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
	if c.Cell != (Cell{}) {
		cardNote := ""
		if c.Cell.CardBand != "" {
			cardNote = " card=" + c.Cell.CardBand
		}
		zipfNote := ""
		if c.Cell.Dist == "zipfian" {
			zipfNote = fmt.Sprintf(" s=%.2f", c.Cell.ZipfS)
		}
		fmt.Fprintf(w, "cell:%s keys=%d members=%d value=%dB dist=%s%s P%d c%d\n",
			cardNote, c.Cell.Keyspace, c.Cell.Members, c.Cell.ValueSize,
			c.Cell.Dist, zipfNote, c.Cell.Pipeline, c.Cell.Connections)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "target\tversion\tops/sec\tMB/s\tp50 us\tp99 us\tp999 us\trss MB\tnote")
	for _, e := range []Entry{c.Aki, c.Redis, c.Valkey} {
		ver := e.Version
		if ver == "" {
			ver = "-"
		}
		if e.Skipped {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t-\t-\tskipped\n", e.Target, ver)
			continue
		}
		note := ""
		if e.Errors > 0 {
			note = fmt.Sprintf("%d errors", e.Errors)
		}
		rss := "-"
		if e.RSSBytes > 0 {
			rss = fmt.Sprintf("%.1f", float64(e.RSSBytes)/(1<<20))
		}
		fmt.Fprintf(tw, "%s\t%s\t%.0f\t%.1f\t%.1f\t%.1f\t%.1f\t%s\t%s\n",
			e.Target, ver, e.OpsPerSec, e.BytesPerSec/(1<<20), e.P50us, e.P99us, e.P999us, rss, note)
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
	if c.GeneratorBound {
		fmt.Fprintf(w, "GENERATOR-BOUND: %s. This row is not a capacity measurement; do not quote it as one.\n", c.GeneratorBoundNote)
	}
}

// WriteJSON emits the comparison as indented JSON for CI artifacts.
func (c Comparison) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}
