package workload

import (
	"math"
	"math/bits"
	"sort"
)

// Selector maps a connection id and that connection's sequence number to an
// index in [0, n). It is the access pattern: uniform spreads evenly across the
// space, zipfian concentrates on a small head. A selector must be a pure
// deterministic function of (conn, seq), with no state and no per-call
// randomness, for two reasons. First, the runner calls it concurrently from
// every connection goroutine, so any shared mutable state would race. Second,
// the three engines (aki, redis, valkey) are measured one after another with
// the same selector, and the comparison is only fair if each engine sees the
// identical key sequence, which a stateful RNG would not give.
//
// The connection id has to be an input. Every connection's seq counter starts
// at zero, so a selector that ignores conn hands all C connections the same
// key stream, and the union of keys a window touches collapses to one
// connection's worth: total ops divided by C, not total ops. That is the bug
// the f3 M0 re-run surfaced (tamnd/aki#542): SET and INCR cells at -keys
// 1000000 touched only 50-90k distinct keys in an 8s window.
type Selector func(conn int, seq int64) int64

// uniformSelector draws uniformly, with replacement, over [0, n): the (conn,
// seq) pair is mixed into a 64-bit hash and reduced to the space without
// modulo bias. Every draw is independent of the last, so for d draws the
// expected distinct-key count is the standard n*(1-(1-1/n)^d) and a window
// long enough to issue a multiple of n ops really does exercise the whole
// space. The old seq-mod-n walk looked like a stronger guarantee (exact
// coverage at d = n) but only per connection; across connections it was pure
// overlap.
func uniformSelector(n int64) Selector {
	if n < 1 {
		n = 1
	}
	un := uint64(n)
	return func(conn int, seq int64) int64 {
		hi, _ := bits.Mul64(connSeqHash(conn, seq), un)
		return int64(hi)
	}
}

// zipf holds a precomputed cumulative-weight table for a Zipfian distribution over
// ranks [0, n) with weight 1/(rank+1)^s. The table is built once and read only, so
// index lookups are concurrency-safe and the whole thing is a pure function of the
// (conn, seq)-derived uniform draw.
type zipf struct {
	cum   []float64 // cum[i] = sum of weights 0..i, ascending, last entry is the total
	total float64
}

// newZipf builds the cumulative table. s is the skew exponent: s near 0 is almost
// uniform, s = 0.99 is the classic YCSB hot-key shape, s = 1.2 is hotter still. The
// table is O(n) memory, which the caller controls through the member or key count;
// for the collection probes (up to a few million members) that is a few tens of MB,
// built once at startup.
func newZipf(n int64, s float64) *zipf {
	if n < 1 {
		n = 1
	}
	if s <= 0 {
		s = 0.99
	}
	cum := make([]float64, n)
	var acc float64
	for i := int64(0); i < n; i++ {
		// weight of rank i is 1/(i+1)^s, the Zipfian law over ranks.
		acc += 1.0 / math.Pow(float64(i+1), s)
		cum[i] = acc
	}
	return &zipf{cum: cum, total: acc}
}

// index maps a uniform draw u in [0, 1) to a rank by inverse-CDF: find the first
// rank whose cumulative weight crosses u*total. Binary search makes it O(log n).
func (z *zipf) index(u float64) int64 {
	target := u * z.total
	i := sort.Search(len(z.cum), func(k int) bool { return z.cum[k] >= target })
	if i >= len(z.cum) {
		i = len(z.cum) - 1
	}
	return int64(i)
}

// zipfianSelector turns (conn, seq) into a uniform draw with a hash, then maps
// it through the inverse CDF. Over many draws the aggregate distribution is
// Zipfian, and a given (conn, seq) always maps to the same rank, which keeps
// the access pattern reproducible and identical across engines while giving
// every connection its own draw stream.
func zipfianSelector(n int64, s float64) Selector {
	z := newZipf(n, s)
	return func(conn int, seq int64) int64 {
		return z.index(hash01(conn, seq))
	}
}

// hash01 maps (conn, seq) to a uniform double in [0, 1) by taking the top 53
// bits of the mixed hash as the mantissa. Deterministic and stateless: the
// same pair always yields the same draw, so the selector built on it is
// reproducible.
func hash01(conn int, seq int64) float64 {
	// top 53 bits over 2^53 gives a uniform double in [0, 1).
	return float64(connSeqHash(conn, seq)>>11) / float64(uint64(1)<<53)
}

// connSeqHash mixes the connection id and the per-connection sequence number
// into one well-distributed 64-bit word. The seq is spread by the golden-ratio
// multiplier and the conn is pre-scrambled through the finalizer, so two
// connections at the same seq land far apart; each input then runs through the
// splitmix64 finalizer, whose avalanche makes every output bit depend on every
// input bit.
func connSeqHash(conn int, seq int64) uint64 {
	return mix64(mix64(uint64(conn)+0x9e3779b97f4a7c15) + uint64(seq)*0x9e3779b97f4a7c15)
}

// mix64 is the splitmix64 finalizer, a cheap full-avalanche integer mixer.
func mix64(x uint64) uint64 {
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
