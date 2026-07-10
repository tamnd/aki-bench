package workload

import (
	"math"
	"sync"
	"testing"
)

// Bitmap mode is exact: distinct count equals the true set size and draws
// count every observation, including revisits.
func TestTrackerBitmapExact(t *testing.T) {
	tr := NewTracker(10000)
	for i := int64(0); i < 5000; i++ {
		tr.Observe(i)
		tr.Observe(i) // revisit must not double-count distinct
	}
	if got := tr.Estimate(); got != 5000 {
		t.Fatalf("bitmap estimate = %d, want 5000 exactly", got)
	}
	if got := tr.Draws(); got != 10000 {
		t.Fatalf("draws = %d, want 10000", got)
	}
}

// HLL mode kicks in above the bitmap ceiling and must land within a few
// percent of the truth, which is far tighter than the 20% deviation the
// report flags on.
func TestTrackerHLLEstimate(t *testing.T) {
	tr := NewTracker(trackerBitmapMax + 1)
	if tr.bits != nil {
		t.Fatal("space above the bitmap ceiling should use HLL")
	}
	const truth = 500000
	for i := int64(0); i < truth; i++ {
		tr.Observe(i * 3) // spaced indices, each observed once
	}
	got := float64(tr.Estimate())
	if diff := math.Abs(got-truth) / truth; diff > 0.05 {
		t.Fatalf("HLL estimate = %.0f, want within 5%% of %d (off %.1f%%)", got, truth, diff*100)
	}
}

// Reset must zero both the distinct state and the draw counter, in both modes;
// this is what scopes the coverage figure to the measured window after warmup.
func TestTrackerReset(t *testing.T) {
	for _, space := range []int{1000, trackerBitmapMax + 1} {
		tr := NewTracker(space)
		for i := int64(0); i < 500; i++ {
			tr.Observe(i)
		}
		tr.Reset()
		if tr.Estimate() != 0 || tr.Draws() != 0 {
			t.Fatalf("space %d: after reset estimate=%d draws=%d, want 0 and 0", space, tr.Estimate(), tr.Draws())
		}
		tr.Observe(7)
		if tr.Estimate() != 1 {
			t.Fatalf("space %d: tracker unusable after reset", space)
		}
	}
}

// Observe is called from every connection goroutine; hammer it concurrently
// so the race detector gets a look, and check the exact mode still counts right.
func TestTrackerConcurrent(t *testing.T) {
	tr := NewTracker(1 << 16)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := int64(0); i < 1<<13; i++ {
				tr.Observe(int64(g)<<13 | i)
			}
		}(g)
	}
	wg.Wait()
	if got := tr.Estimate(); got != 1<<16 {
		t.Fatalf("concurrent distinct = %d, want %d", got, 1<<16)
	}
	if got := tr.Draws(); got != 1<<16 {
		t.Fatalf("concurrent draws = %d, want %d", got, 1<<16)
	}
}

// A tracked spec must record exactly what the generator drew: the measured
// window's coverage figure is only trustworthy if nothing on the generator
// path bypasses the tracker.
func TestSpecTrackObservesSelectorDraws(t *testing.T) {
	tr := NewTracker(1000)
	gen := Set(Spec{ValueSize: 8, KeyCount: 1000, Track: tr})
	seen := map[string]bool{}
	const conns, perConn = 4, 250
	for conn := 0; conn < conns; conn++ {
		for seq := int64(0); seq < perConn; seq++ {
			seen[string(gen(conn, seq)[1])] = true
		}
	}
	if got, want := tr.Draws(), int64(conns*perConn); got != want {
		t.Fatalf("draws = %d, want %d", got, want)
	}
	if got := tr.Estimate(); got != int64(len(seen)) {
		t.Fatalf("tracker distinct = %d, generator produced %d distinct keys", got, len(seen))
	}
}
