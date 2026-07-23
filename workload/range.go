package workload

import (
	"strconv"

	"github.com/tamnd/aki-bench/load"
)

// This file adds the range, scan, and algebra workloads that round out the
// collection coverage. The point-read plans in collection.go measure a single
// element lookup; these measure the bound-not-materialize paths the spec set
// cares about most: a bounded window over an ordered collection (LRANGE, ZRANGE,
// ZRANGEBYSCORE), a bounded cursor step (HSCAN, SSCAN), the whole-collection read
// that is the materialize worst case (SMEMBERS, HGETALL), and the streaming set
// algebra over two sources (SINTER, SUNION).
//
// Every plan reuses the Plan shape from collection.go: a single-connection
// sequential preload that fully populates the probed collection, then a measured
// probe. The algebra plans populate two collections in one preload pass by
// alternating the destination key, so the probe can intersect or union them.

// rangeWindow is the number of elements a bounded range or scan probe asks for.
// It is small relative to a large collection on purpose: the whole point of the
// bound-not-materialize rule is that the cost tracks the window, not the
// collection size, so the probe has to request a window much smaller than the
// member space to measure that property.
const rangeWindow = 100

// LRange builds a list of Members elements and probes it with a bounded LRANGE
// over a window that starts at a random in-range index. The list is built with
// RPUSH so element i sits at list index i, which makes the requested window
// deterministic and always a hit.
func LRange(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	rpush := []byte("RPUSH")
	lk := []byte("list:" + collKey)
	lrange := []byte("LRANGE")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{rpush, lk, memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			start := windowStart(sel(conn, seq), int64(s.Members))
			stop := start + rangeWindow - 1
			return [][]byte{lrange, lk, intArg(start), intArg(stop)}
		},
	}
}

// ZRange builds a sorted set of Members members and probes it with a bounded
// ZRANGE by rank over a window that starts at a random in-range rank. Members get
// score equal to their index, so rank order matches insertion order and the
// window is deterministic.
func ZRange(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zadd := []byte("ZADD")
	zk := []byte("zset:" + collKey)
	zrange := []byte("ZRANGE")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{zadd, zk, intArg(seq), memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			start := windowStart(sel(conn, seq), int64(s.Members))
			stop := start + rangeWindow - 1
			return [][]byte{zrange, zk, intArg(start), intArg(stop)}
		},
	}
}

// ZRangeByScore builds a sorted set of Members members and probes it with a
// bounded ZRANGEBYSCORE over a score window. Scores equal the member index, so a
// window [lo, lo+window-1] selects a known slice and exercises the score index
// rather than the rank index.
func ZRangeByScore(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zadd := []byte("ZADD")
	zk := []byte("zset:" + collKey)
	zrangebyscore := []byte("ZRANGEBYSCORE")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{zadd, zk, intArg(seq), memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			lo := windowStart(sel(conn, seq), int64(s.Members))
			hi := lo + rangeWindow - 1
			return [][]byte{zrangebyscore, zk, intArg(lo), intArg(hi)}
		},
	}
}

// HScan builds a hash of Members fields and probes it with a single bounded HSCAN
// step from cursor 0 with COUNT. HSCAN is the bound-not-materialize alternative to
// HGETALL: it returns at most a COUNT-sized batch, so its cost tracks the window,
// not the hash size.
func HScan(s Spec) Plan {
	s = s.withDefaults()
	hset := []byte("HSET")
	hk := []byte("hash:" + collKey)
	hscan := []byte("HSCAN")
	val := value(s.ValueSize)
	zero := []byte("0")
	count := []byte("COUNT")
	cnt := intArg(rangeWindow)
	field := func(idx int64) []byte { return []byte("f" + strconv.FormatInt(idx, 10)) }
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{hset, hk, field(seq), val}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hscan, hk, zero, count, cnt}
		},
	}
}

// SScan builds a set of Members elements and probes it with a single bounded SSCAN
// step from cursor 0 with COUNT, the bounded alternative to SMEMBERS.
func SScan(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sk := []byte("set:" + collKey)
	sscan := []byte("SSCAN")
	zero := []byte("0")
	count := []byte("COUNT")
	cnt := intArg(rangeWindow)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{sadd, sk, memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{sscan, sk, zero, count, cnt}
		},
	}
}

// SMembers builds a set of Members elements and probes it with SMEMBERS, which
// returns the whole set. This is the materialize worst case on purpose: it is the
// command the audit flags as a risk, and the regime where a streaming reply path
// has to keep aki ahead even though the reply is the entire collection. Keep
// Members modest for this probe; a multi-million element SMEMBERS is a reply-size
// benchmark, not a storage benchmark.
func SMembers(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sk := []byte("set:" + collKey)
	smembers := []byte("SMEMBERS")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{sadd, sk, memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{smembers, sk}
		},
	}
}

// HGetAll builds a hash of Members fields and probes it with HGETALL, the hash
// materialize worst case, paired with SMembers for the set side.
func HGetAll(s Spec) Plan {
	s = s.withDefaults()
	hset := []byte("HSET")
	hk := []byte("hash:" + collKey)
	hgetall := []byte("HGETALL")
	val := value(s.ValueSize)
	field := func(idx int64) []byte { return []byte("f" + strconv.FormatInt(idx, 10)) }
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{hset, hk, field(seq), val}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hgetall, hk}
		},
	}
}

// setAKey and setBKey are the two sources the algebra plans build and combine, and setDest
// is the separate destination the STORE forms write into (never a source, so the store takes
// the non-aliased streaming path, which is the common case the audit measures).
var (
	setAKey = []byte("set:" + collKey + ":a")
	setBKey = []byte("set:" + collKey + ":b")
	setDest = []byte("set:" + collKey + ":out")
)

// algebraPreload populates two half-overlapping sets over one sequential pass. Even
// sequence steps write set a over m0..m{members-1}, odd steps write set b over the shifted
// range m{members/2}..m{members+members/2-1}, so each set ends with members distinct members
// and the two share their upper/lower half band of members/2 members. The overlap scales
// with the set size so it stays a real middle band at any sweep size, keeping every algebra
// form doing real work: SINTER and SINTERCARD return about half the members, SDIFF about
// half, and SUNION about one and a half times members, rather than the degenerate full
// overlap (SDIFF empty, SINTER is either source) or zero overlap (SINTER empty, SDIFF is the
// whole first source) a fixed shift would drift into as the size changes.
func algebraPreload(sadd []byte, members int) load.CommandGen {
	shift := int64(members / 2)
	return func(conn int, seq int64) [][]byte {
		if seq%2 == 0 {
			return [][]byte{sadd, setAKey, memberName(seq / 2)}
		}
		return [][]byte{sadd, setBKey, memberName(seq/2 + shift)}
	}
}

// SInter builds two half-overlapping sets and probes them with SINTER, the streaming
// k-way-merge intersection the set model specs. PreloadOps is twice Members because
// the one preload pass fills both sets; the middle-half overlap means the merge returns
// a real result rather than a whole source, so the ratio measures the merge, not a copy.
func SInter(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sinter := []byte("SINTER")
	// The two source keys are fixed, so the whole argument vector is a constant. Build
	// it once and return the same slice every probe: the load generator only reads argv
	// to serialize it, so a shared read-only vector across connections is safe and keeps
	// the measuring client from allocating one slice header per probe, which on a
	// saturating merge run would make the client its own bottleneck.
	argv := [][]byte{sinter, setAKey, setBKey}
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// SUnion builds two half-overlapping sets and probes them with SUNION, the streaming
// union over two sources. The middle-half overlap means the two-pass merge dedups a real
// shared band rather than concatenating two disjoint sets, so it exercises the emit-once
// path SUNION's framing depends on.
func SUnion(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sunion := []byte("SUNION")
	argv := [][]byte{sunion, setAKey, setBKey} // constant argv, shared across probes (see SInter)
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// SDiff builds two half-overlapping sets and probes them with SDIFF, the streaming
// difference that walks the first set and rejects any member the second holds. The
// middle-half overlap means about half of set a survives the subtraction, so the probe
// measures the real walk-and-reject path rather than the degenerate empty or identity
// result a full or zero overlap would give.
func SDiff(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sdiff := []byte("SDIFF")
	argv := [][]byte{sdiff, setAKey, setBKey} // constant argv, shared across probes (see SInter)
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// SInterCard builds two half-overlapping sets and probes them with SINTERCARD, the
// count-only intersection that stops at an optional LIMIT. This probe passes no LIMIT, so
// it counts the whole intersection: the smallest-set-first probe path with no array to
// frame, the compute-bound cousin of SINTER where aki has no merge-streaming advantage and
// the audit watches it against Redis's flat hashtable probe.
func SInterCard(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sintercard := []byte("SINTERCARD")
	two := []byte("2")
	argv := [][]byte{sintercard, two, setAKey, setBKey} // constant argv, shared across probes (see SInter)
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// SInterStore builds two half-overlapping sets and probes them with SINTERSTORE into a
// separate destination, the intersection written out rather than replied. It measures the
// merge plus the destination write path: the smallest-set-first probe streamed straight into
// a freshly cleared destination set, O(k) memory for a result of k members. The destination
// is rebuilt every probe (cleared then rewritten), so the run measures the steady store cost
// rather than a one-time create.
func SInterStore(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sinterstore := []byte("SINTERSTORE")
	argv := [][]byte{sinterstore, setDest, setAKey, setBKey} // constant argv, shared across probes (see SInter)
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// SUnionStore builds two half-overlapping sets and probes them with SUNIONSTORE into a
// separate destination, the union written out. Unlike the SUNION read, which frames its reply
// with a two-pass count, the store streams the deduplicated k-way merge straight into the
// cleared destination in one pass, so it measures the merge plus the destination write with no
// count pass. The result is about one and a half times Members members, the largest write of
// the three STORE forms.
func SUnionStore(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sunionstore := []byte("SUNIONSTORE")
	argv := [][]byte{sunionstore, setDest, setAKey, setBKey} // constant argv, shared across probes (see SInter)
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// SDiffStore builds two half-overlapping sets and probes them with SDIFFSTORE into a separate
// destination, the difference written out. About half of set a survives the subtraction, so
// the probe streams a real walk-and-reject result into the cleared destination rather than a
// degenerate empty or identity write.
func SDiffStore(s Spec) Plan {
	s = s.withDefaults()
	sadd := []byte("SADD")
	sdiffstore := []byte("SDIFFSTORE")
	argv := [][]byte{sdiffstore, setDest, setAKey, setBKey} // constant argv, shared across probes (see SInter)
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    algebraPreload(sadd, s.Members),
		Probe: func(conn int, seq int64) [][]byte {
			return argv
		},
	}
}

// zsetAKey and zsetBKey are the two sorted-set sources the zset algebra plan
// builds and combines, the zset analogue of setAKey and setBKey.
var (
	zsetAKey = []byte("zset:" + collKey + ":a")
	zsetBKey = []byte("zset:" + collKey + ":b")
	zsetDest = []byte("zset:" + collKey + ":out")
)

// zAlgebraPreload populates two sorted sets over one sequential pass, the zset
// analogue of algebraPreload. Even sequence steps write zset a, odd steps write
// zset b, and the member id is seq/2 so each set ends with Members distinct
// scored members. The two sets fully overlap, so a union returns Members members
// and exercises the score-accumulating merge over both sources.
func zAlgebraPreload(zadd []byte) load.CommandGen {
	return func(conn int, seq int64) [][]byte {
		key := zsetAKey
		if seq%2 == 1 {
			key = zsetBKey
		}
		id := seq / 2
		return [][]byte{zadd, key, intArg(id), memberName(id)}
	}
}

// ZUnion builds two equal sorted sets and probes them with ZUNIONSTORE with
// WEIGHTS, the zset algebra workload. The store form is the one the per-type doc
// spends the most design effort bounding: it has to stream both score indexes and
// accumulate weighted scores into the destination without materializing either
// source. WEIGHTS 1 1 keeps the result well defined while still driving the
// weighted-merge path rather than the plain-union shortcut.
func ZUnion(s Spec) Plan {
	s = s.withDefaults()
	zunionstore := []byte("ZUNIONSTORE")
	two := []byte("2")
	weights := []byte("WEIGHTS")
	one := []byte("1")
	return Plan{
		PreloadOps: int64(s.Members) * 2,
		Preload:    zAlgebraPreload([]byte("ZADD")),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{zunionstore, zsetDest, two, zsetAKey, zsetBKey, weights, one, one}
		},
	}
}

// LPop builds a list of Members elements and probes it with LPOP, the list point
// read-write. It is non-draining (sustained): even seq RPUSHes a fresh
// per-connection element onto the tail, odd seq LPOPs the head, so the list holds
// its length and LPOP keeps popping a populated head for the whole run. A pure drain
// empties the list and leaves LPOP returning nil on the same cheap path for all
// three servers, zeroing value-bearing throughput so the gate cannot score it. The
// list is built with RPUSH so the head pops in insertion order.
func LPop(s Spec) Plan {
	s = s.withDefaults()
	rpush := []byte("RPUSH")
	lk := []byte("list:" + collKey)
	lpop := []byte("LPOP")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{rpush, lk, memberName(seq)}
		},
		Probe: sustained(
			func(conn int, seq int64) [][]byte {
				return [][]byte{rpush, lk, refillName(conn, seq)}
			},
			func(conn int, seq int64) [][]byte {
				return [][]byte{lpop, lk}
			},
		),
	}
}

// LIndex builds a list of Members elements and probes it with LINDEX at a random
// in-range index, the list point read at an index. RPUSH puts element i at index
// i, so the probed index is always a hit and the read resolves one position
// without walking the list.
func LIndex(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	rpush := []byte("RPUSH")
	lk := []byte("list:" + collKey)
	lindex := []byte("LINDEX")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{rpush, lk, memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{lindex, lk, intArg(sel(conn, seq))}
		},
	}
}

// windowStart clamps a selector-chosen index so a window of rangeWindow elements
// starting there stays inside the member space. Without the clamp a window near
// the end of the space would run past the last element and return a short reply,
// which would understate the work for the keys the selector concentrates on.
func windowStart(idx, members int64) int64 {
	last := max(members-rangeWindow, 0)
	if idx > last {
		idx = last
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}

// intArg formats an integer as a command argument without allocating a string
// header separately from the byte slice the client writes.
func intArg(n int64) []byte {
	return strconv.AppendInt(nil, n, 10)
}

// rangePlans returns the range, scan, and algebra plans keyed by name. They are
// merged into PlanRegistry so main dispatches them the same way as the point-read
// plans.
func rangePlans() map[string]func(Spec) Plan {
	return map[string]func(Spec) Plan{
		"lrange":        LRange,
		"lpop":          LPop,
		"lindex":        LIndex,
		"zrange":        ZRange,
		"zrangebyscore": ZRangeByScore,
		"zunion":        ZUnion,
		"hscan":         HScan,
		"sscan":         SScan,
		"smembers":      SMembers,
		"hgetall":       HGetAll,
		"sinter":        SInter,
		"sunion":        SUnion,
		"sdiff":         SDiff,
		"sintercard":    SInterCard,
		"sinterstore":   SInterStore,
		"sunionstore":   SUnionStore,
		"sdiffstore":    SDiffStore,
	}
}

// rangePlanNames lists the range, scan, and algebra workloads in a stable order.
func rangePlanNames() []string {
	return []string{"lrange", "lpop", "lindex", "zrange", "zrangebyscore", "zunion", "hscan", "sscan", "smembers", "hgetall", "sinter", "sunion", "sdiff", "sintercard", "sinterstore", "sunionstore", "sdiffstore"}
}
