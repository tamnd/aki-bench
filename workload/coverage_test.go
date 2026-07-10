package workload

import (
	"math"
	"testing"
)

// expectedUniformDistinct is the expected number of distinct slots after draws
// uniform picks with replacement from a space of k slots: k*(1-(1-1/k)^draws).
func expectedUniformDistinct(k, draws int64) float64 {
	return float64(k) * (1 - math.Pow(1-1/float64(k), float64(draws)))
}

// TestUniformCoverageAtWindowScale reproduces the f3 M0 re-run finding
// (tamnd/aki#542): a measured window's worth of draws against a nominal 1M key
// space must actually spread over the space. The run shape mirrors a real
// window: many connections, each with its own seq counter starting at zero,
// which is exactly how load.Run drives a generator. Before the fix every
// connection walked the same seq-mod-n prefix, so 2M draws over 50 connections
// touched only 40k distinct keys instead of the ~865k uniform sampling
// predicts.
func TestUniformCoverageAtWindowScale(t *testing.T) {
	const (
		keys        = 1_000_000
		conns       = 50
		drawsPerCon = 40_000 // 2M draws total, about an 8s window at 250k ops/s
	)
	sel := uniformSelector(keys)
	seen := make([]bool, keys)
	distinct := 0
	for conn := 0; conn < conns; conn++ {
		for seq := int64(0); seq < drawsPerCon; seq++ {
			i := sel(conn, seq)
			if !seen[i] {
				seen[i] = true
				distinct++
			}
		}
	}
	want := expectedUniformDistinct(keys, conns*drawsPerCon)
	if diff := math.Abs(float64(distinct)-want) / want; diff > 0.03 {
		t.Fatalf("distinct keys = %d, want about %.0f (uniform expectation for %d draws over %d keys), off by %.1f%%",
			distinct, want, conns*drawsPerCon, keys, diff*100)
	}
}

// Two connections must not issue the same key stream. Per-connection seq
// counters all start at zero, so a selector that ignores conn hands every
// connection an identical stream and the union of touched keys collapses to
// one connection's worth.
func TestSelectorStreamsDifferByConnection(t *testing.T) {
	for name, sel := range map[string]Selector{
		"uniform": uniformSelector(1_000_000),
		"zipfian": zipfianSelector(1_000_000, 0.99),
	} {
		same := 0
		const probe = 1000
		for seq := int64(0); seq < probe; seq++ {
			if sel(0, seq) == sel(1, seq) {
				same++
			}
		}
		if same == probe {
			t.Fatalf("%s: connections 0 and 1 produced identical streams over %d draws", name, probe)
		}
	}
}

// Zipfian sanity at the same window scale: the head must be hot (far above the
// uniform share) and the distinct-key count must sit well below the uniform
// expectation, because most draws revisit the head.
func TestZipfianCoverageAtWindowScale(t *testing.T) {
	const (
		keys        = 1_000_000
		conns       = 50
		drawsPerCon = 40_000
	)
	sel := zipfianSelector(keys, 0.99)
	seen := make([]bool, keys)
	distinct := 0
	head := 0 // draws landing in the top 1% of ranks
	for conn := 0; conn < conns; conn++ {
		for seq := int64(0); seq < drawsPerCon; seq++ {
			i := sel(conn, seq)
			if !seen[i] {
				seen[i] = true
				distinct++
			}
			if i < keys/100 {
				head++
			}
		}
	}
	draws := conns * drawsPerCon
	headFrac := float64(head) / float64(draws)
	if headFrac < 0.20 {
		t.Fatalf("zipfian head share = %.3f, want at least 0.20 in the top 1%% of ranks", headFrac)
	}
	uniformWant := expectedUniformDistinct(keys, int64(draws))
	if float64(distinct) >= uniformWant {
		t.Fatalf("zipfian distinct = %d, should sit well below the uniform expectation %.0f", distinct, uniformWant)
	}
	if distinct < 10_000 {
		t.Fatalf("zipfian distinct = %d, too few for 2M draws over 1M ranks", distinct)
	}
}
