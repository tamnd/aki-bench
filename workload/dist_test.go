package workload

import "testing"

// uniform must reproduce the old seq-mod-n behavior exactly, so existing runs are
// unchanged when no -dist is passed.
func TestUniformSelectorMatchesMod(t *testing.T) {
	sel := uniformSelector(1000)
	for _, seq := range []int64{0, 1, 999, 1000, 1001, 123456} {
		if got, want := sel(seq), seq%1000; got != want {
			t.Fatalf("uniform(%d) = %d, want %d", seq, got, want)
		}
	}
}

// A selector must be a pure function of seq: the same seq always yields the same
// index, on both uniform and zipfian. This is what keeps the access pattern
// identical across the three engines.
func TestSelectorDeterministic(t *testing.T) {
	for _, sel := range []Selector{uniformSelector(10000), zipfianSelector(10000, 0.99)} {
		for _, seq := range []int64{0, 7, 42, 99999, 1 << 30} {
			first := sel(seq)
			for range 3 {
				if got := sel(seq); got != first {
					t.Fatalf("selector not deterministic at seq %d: %d then %d", seq, first, got)
				}
			}
		}
	}
}

// Every index a selector returns must land in [0, n).
func TestSelectorInRange(t *testing.T) {
	const n = 5000
	for _, sel := range []Selector{uniformSelector(n), zipfianSelector(n, 1.1)} {
		for seq := int64(0); seq < 100000; seq++ {
			i := sel(seq)
			if i < 0 || i >= n {
				t.Fatalf("index %d out of range [0,%d) at seq %d", i, n, seq)
			}
		}
	}
}

// Zipfian must concentrate traffic on a small head: a large majority of draws over
// a big space should land in the top few percent of ranks. Uniform must not.
func TestZipfianIsSkewed(t *testing.T) {
	const n = 100000
	const samples = 200000
	zsel := zipfianSelector(n, 0.99)
	usel := uniformSelector(n)

	head := int64(n / 100) // top 1% of ranks
	var zHead, uHead int
	for seq := int64(0); seq < samples; seq++ {
		if zsel(seq) < head {
			zHead++
		}
		if usel(seq) < head {
			uHead++
		}
	}
	zFrac := float64(zHead) / samples
	uFrac := float64(uHead) / samples
	// Uniform puts ~1% in the top 1%. Zipfian at s=0.99 should put far more.
	if uFrac > 0.05 {
		t.Fatalf("uniform should not concentrate on the head, got %.3f in top 1%%", uFrac)
	}
	if zFrac < 0.20 {
		t.Fatalf("zipfian should concentrate on the head, got only %.3f in top 1%%", zFrac)
	}
}

// A hotter exponent must concentrate more than a milder one.
func TestZipfianExponentMonotonic(t *testing.T) {
	const n = 100000
	const samples = 100000
	head := int64(n / 100)
	frac := func(s float64) float64 {
		sel := zipfianSelector(n, s)
		var c int
		for seq := int64(0); seq < samples; seq++ {
			if sel(seq) < head {
				c++
			}
		}
		return float64(c) / samples
	}
	mild := frac(0.7)
	hot := frac(1.3)
	if hot <= mild {
		t.Fatalf("hotter exponent should concentrate more: s=1.3 gave %.3f, s=0.7 gave %.3f", hot, mild)
	}
}
