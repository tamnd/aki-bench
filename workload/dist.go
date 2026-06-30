package workload

import (
	"math"
	"sort"
)

// Selector maps a per-connection sequence number to an index in [0, n). It is the
// access pattern: uniform spreads evenly across the space, zipfian concentrates on
// a small head. A selector must be a pure deterministic function of seq, with no
// state and no per-call randomness, for two reasons. First, the runner calls it
// concurrently from every connection goroutine, so any shared mutable state would
// race. Second, the three engines (aki, redis, valkey) are measured one after
// another with the same selector, and the comparison is only fair if each engine
// sees the identical key sequence, which a stateful RNG would not give.
type Selector func(seq int64) int64

// uniformSelector returns index = seq mod n, the even spread the suite used before
// the access axis existed. It is the worst case for a read cache: every distinct
// seq lands on a distinct slot until the space wraps, so there is no hot subset to
// exploit. Kept as the default so existing runs are unchanged.
func uniformSelector(n int64) Selector {
	if n < 1 {
		n = 1
	}
	return func(seq int64) int64 {
		i := seq % n
		if i < 0 {
			i += n
		}
		return i
	}
}

// zipf holds a precomputed cumulative-weight table for a Zipfian distribution over
// ranks [0, n) with weight 1/(rank+1)^s. The table is built once and read only, so
// index lookups are concurrency-safe and the whole thing is a pure function of the
// seq-derived uniform draw.
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

// zipfianSelector turns the seq into a uniform draw with a hash, then maps it
// through the inverse CDF. Two distinct seqs that hash close together land on
// nearby ranks, but over many seqs the aggregate distribution is Zipfian, and a
// given seq always maps to the same rank, which keeps the access pattern
// reproducible and identical across engines.
func zipfianSelector(n int64, s float64) Selector {
	z := newZipf(n, s)
	return func(seq int64) int64 {
		return z.index(hash01(seq))
	}
}

// hash01 maps a sequence number to a uniform double in [0, 1). It runs the seq
// through splitmix64, a well-distributed integer mixer, then takes the top 53 bits
// as the mantissa of a double. Deterministic and stateless: the same seq always
// yields the same draw, so the selector built on it is reproducible.
func hash01(seq int64) float64 {
	x := uint64(seq) + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x = x ^ (x >> 31)
	// top 53 bits over 2^53 gives a uniform double in [0, 1).
	return float64(x>>11) / float64(uint64(1)<<53)
}
