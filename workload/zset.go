package workload

import (
	"strconv"

	"github.com/tamnd/aki-bench/load"
)

// This file gives the sorted set its point-path operator coverage as per-collection
// probes: every zset point and meta command the M6 slice-1 surface answers, measured
// against one large preloaded zset so the comparison is aki versus Redis versus Valkey
// on the same operator in the same regime. collection.go carries the two order-index
// reads (zscore, zrank); this file fills in the rest of the point surface (zcard,
// zmscore, zincrby, add-into-one-large-zset, and destructive remove) so a sweep reports
// a full zset ratio matrix rather than a single score probe.
//
// Every plan builds the same single zset, zsetProbeKey, with Members members m0..m{n-1}
// scored 0..n-1 over one sequential preload pass, then probes one operator against it.
// The reads and meta ops are non-destructive so the zset stays fully populated for the
// whole run; the increment, add, and remove probes note their own regime effects inline.

// zsetProbeKey is the single zset every zset plan builds and probes. It matches the key
// ZScore and ZRank already target ("zset:" + collKey) so every zset plan works against
// one consistent large zset. One large zset is the case the audit cares about: on the
// larger-than-memory side a multi-million member dual-family sub-tree paged from disk,
// and on the in-memory-fit side the descent a modern layout must answer as fast as a
// flat Redis listpack/skiplist probe.
var zsetProbeKey = []byte("zset:" + collKey)

// zsetPreload writes one scored member per sequence step into zsetProbeKey, so a single
// sequential connection walking 0..Members-1 populates every member exactly once with a
// distinct score equal to its index.
func zsetPreload() load.CommandGen {
	zadd := []byte("ZADD")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{zadd, zsetProbeKey, []byte(strconv.FormatInt(seq, 10)), memberName(seq)}
	}
}

// ZCard builds a zset of Members members and probes it with ZCARD, the O(1) cardinality.
// It reads the maintained header count with no scan, so it is the cheapest zset op and
// the one where a per-member storage model must not regress into a count-by-scan, the
// zset analogue of SCARD and HLEN.
func ZCard(s Spec) Plan {
	s = s.withDefaults()
	zcard := []byte("ZCARD")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    zsetPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{zcard, zsetProbeKey}
		},
	}
}

// ZMScore builds a zset of Members members and probes it with ZMSCORE over a window of
// members, the multi-member batch score read. Every probed member exists, so the batch
// is a full hit that resolves as a window of point probes on the member-family index,
// the zset analogue of HMGET and SMISMEMBER.
func ZMScore(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zmscore := []byte("ZMSCORE")
	w := min(int64(s.Members), int64(rangeWindow))
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    zsetPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			start := windowStart(sel(seq), int64(s.Members))
			argv := make([][]byte, 0, 2+w)
			argv = append(argv, zmscore, zsetProbeKey)
			for i := range w {
				argv = append(argv, memberName(start+i))
			}
			return argv
		},
	}
}

// ZAddMember builds a zset of Members members and probes it with ZADD of a random
// existing member at its own score, the update-in-place write hot path into one large
// zset. This is distinct from the flat zadd workload, which spreads single-member writes
// across the whole key space of many small zsets; here every write lands in the same
// large collection on an already present member at an unchanged score, so it resolves to
// the locate-then-no-op branch (find the member row in a big dual-family sub-tree, read
// the old score, see it unchanged, add nothing), which is the branch a write model must
// keep cheap since it touches neither the score family nor the header count.
func ZAddMember(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zadd := []byte("ZADD")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    zsetPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			idx := sel(seq)
			return [][]byte{zadd, zsetProbeKey, []byte(strconv.FormatInt(idx, 10)), memberName(idx)}
		},
	}
}

// ZIncrBy builds a zset of Members members and probes it with ZINCRBY of 0 on a random
// existing member, the score-mutation write path. A zero increment reads the old score,
// computes an unchanged new score, and takes the no-op branch that skips the score-family
// delete-and-reinsert, so it measures the member-row read plus the increment decision
// without draining the zset or churning the order index. It is non-destructive, so the
// zset stays fully populated for the whole run.
func ZIncrBy(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zincrby := []byte("ZINCRBY")
	zero := []byte("0")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    zsetPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{zincrby, zsetProbeKey, zero, memberName(sel(seq))}
		},
	}
}

// ZRem builds a zset of Members members and probes it with ZREM on a random member, the
// destructive member removal that clears both the member-family and score-family rows.
// Like SREM and HDEL it drains the collection over a sustained run: once a member is
// gone, re-removing it returns 0, the same cheap path on aki, Redis, and Valkey, so a
// drained tail does not bias the ratio even though it stops measuring the populated-remove
// cost. Size Members at or above the op budget for a run that removes a live member
// throughout.
func ZRem(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zrem := []byte("ZREM")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    zsetPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{zrem, zsetProbeKey, memberName(sel(seq))}
		},
	}
}

// zsetPlans returns the zset point-path operator plans this file adds, keyed by name.
// They merge into PlanRegistry so main dispatches them the same way as the other
// collection plans.
func zsetPlans() map[string]func(Spec) Plan {
	return map[string]func(Spec) Plan{
		"zcard":      ZCard,
		"zmscore":    ZMScore,
		"zaddmember": ZAddMember,
		"zincrby":    ZIncrBy,
		"zrem":       ZRem,
	}
}

// zsetPlanNames lists the zset point-path operator workloads in a stable order.
func zsetPlanNames() []string {
	return []string{"zcard", "zmscore", "zaddmember", "zincrby", "zrem"}
}
