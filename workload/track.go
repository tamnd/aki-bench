package workload

import (
	"math"
	"math/bits"
	"sync/atomic"
)

// trackerBitmapMax is the largest space the tracker counts exactly with a
// bitmap: 1<<27 slots is 16 MiB of bits, cheap for a load generator. Larger
// spaces fall back to a HyperLogLog estimate so memory stays flat no matter
// what -keys says.
const trackerBitmapMax = 1 << 27

// hllBits sizes the HyperLogLog register file: 2^14 registers is 64 KiB as
// atomics and a ~0.8% standard error, far tighter than the 20% deviation the
// report cares about.
const hllBits = 14

// Tracker estimates how many distinct key-space indices a run actually
// selected, so a result row can self-report its keyspace coverage instead of
// assuming the -keys flag was exercised. The f3 M0 re-run (tamnd/aki#542)
// found write windows touching 5% of their nominal space; with this in the
// row that shows up in the JSON instead of in a postmortem.
//
// Observe is called from every connection goroutine on the measured path, so
// both modes are lock-free: the bitmap sets bits with an atomic Or behind a
// plain load (revisits of an already-set bit, the common case on a hot
// distribution, never write), and the HLL raises registers with a CAS max.
type Tracker struct {
	space int64
	draws atomic.Int64
	bits  []atomic.Uint64 // exact bitmap, one bit per slot, when the space is small enough
	regs  []atomic.Uint32 // HLL registers otherwise
}

// NewTracker returns a tracker for a key space of the given size. Spaces up
// to trackerBitmapMax count exactly; larger ones estimate.
func NewTracker(space int) *Tracker {
	if space < 1 {
		space = 1
	}
	t := &Tracker{space: int64(space)}
	if t.space <= trackerBitmapMax {
		t.bits = make([]atomic.Uint64, (space+63)/64)
	} else {
		t.regs = make([]atomic.Uint32, 1<<hllBits)
	}
	return t
}

// Observe records one selected index.
func (t *Tracker) Observe(idx int64) {
	t.draws.Add(1)
	if t.bits != nil {
		w, b := idx>>6, uint64(1)<<(idx&63)
		if t.bits[w].Load()&b == 0 {
			t.bits[w].Or(b)
		}
		return
	}
	x := mix64(uint64(idx))
	j := x >> (64 - hllBits)
	// rank of the first set bit in the remaining 64-hllBits bits, 1-based;
	// the or-ed sentinel caps an all-zero tail without a branch.
	rho := uint32(bits.LeadingZeros64(x<<hllBits|1<<(hllBits-1))) + 1
	for {
		cur := t.regs[j].Load()
		if rho <= cur || t.regs[j].CompareAndSwap(cur, rho) {
			return
		}
	}
}

// Draws returns how many observations were recorded.
func (t *Tracker) Draws() int64 { return t.draws.Load() }

// Estimate returns the distinct-index count: exact in bitmap mode, the
// standard HLL estimate (with the small-range linear-counting correction)
// otherwise, clamped to the space.
func (t *Tracker) Estimate() int64 {
	if t.bits != nil {
		var n int64
		for i := range t.bits {
			n += int64(bits.OnesCount64(t.bits[i].Load()))
		}
		return n
	}
	m := float64(uint64(1) << hllBits)
	var sum float64
	zeros := 0
	for i := range t.regs {
		r := t.regs[i].Load()
		sum += math.Ldexp(1, -int(r))
		if r == 0 {
			zeros++
		}
	}
	alpha := 0.7213 / (1 + 1.079/m)
	est := alpha * m * m / sum
	if est <= 2.5*m && zeros > 0 {
		est = m * math.Log(m/float64(zeros))
	}
	if est > float64(t.space) {
		est = float64(t.space)
	}
	return int64(est)
}

// Reset clears the tracker. The runner calls this between the warmup drive
// and the measured window so the coverage figure describes only the measured
// seconds; warmup replays the same deterministic stream but for a different
// number of ops, and counting it would inflate the window's coverage.
func (t *Tracker) Reset() {
	t.draws.Store(0)
	for i := range t.bits {
		t.bits[i].Store(0)
	}
	for i := range t.regs {
		t.regs[i].Store(0)
	}
}

// tracked wraps a selector so every draw lands in the tracker as well.
func tracked(sel Selector, t *Tracker) Selector {
	if t == nil {
		return sel
	}
	return func(conn int, seq int64) int64 {
		i := sel(conn, seq)
		t.Observe(i)
		return i
	}
}
