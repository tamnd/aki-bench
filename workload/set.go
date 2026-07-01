package workload

import (
	"github.com/tamnd/aki-bench/load"
)

// This file gives the set type full operator coverage as per-collection probes:
// every set point and meta command the servers answer, measured against one large
// preloaded set so the comparison is aki versus Redis versus Valkey on the same
// operator in the same regime. collection.go carries the set point read (sismember)
// and range.go the two whole-collection reads (smembers, sscan); this file fills in
// the rest of the point surface (scard, srem, smismember, and add-into-one-large-set)
// so a sweep reports a full set ratio matrix rather than a single membership probe.
//
// Every plan builds the same single set, setProbeKey, with Members members m0..m{n-1}
// over one sequential preload pass, then probes one operator against it. The reads and
// meta ops are non-destructive so the set stays fully populated for the whole run; the
// add and remove probes note their own regime effects inline.

// setProbeKey is the single set every set plan builds and probes. It matches the key
// SISMember, SMembers, and SScan already target ("set:" + collKey) so every set plan
// works against one consistent large set. One large set is the case the audit cares
// about: on the larger-than-memory side a multi-million member sub-tree paged from
// disk, and on the in-memory-fit side the descent a modern layout must answer as fast
// as a flat Redis intset/hashtable probe.
var setProbeKey = []byte("set:" + collKey)

// setPreload writes one member per sequence step into setProbeKey, so a single
// sequential connection walking 0..Members-1 populates every member exactly once.
func setPreload() load.CommandGen {
	sadd := []byte("SADD")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{sadd, setProbeKey, memberName(seq)}
	}
}

// SCard builds a set of Members members and probes it with SCARD, the O(1) cardinality.
// It reads the maintained header count with no scan, so it is the cheapest set op and
// the one where a per-member storage model must not regress into a count-by-scan, the
// set analogue of HLEN.
func SCard(s Spec) Plan {
	s = s.withDefaults()
	scard := []byte("SCARD")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{scard, setProbeKey}
		},
	}
}

// SMIsMember builds a set of Members members and probes it with SMISMEMBER over a
// window of members, the multi-member batch membership check. Every probed member
// exists, so the batch is a full hit that resolves as a window of point probes on the
// index, the set analogue of HMGET.
func SMIsMember(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	smismember := []byte("SMISMEMBER")
	w := min(int64(s.Members), int64(rangeWindow))
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			start := windowStart(sel(seq), int64(s.Members))
			argv := make([][]byte, 0, 2+w)
			argv = append(argv, smismember, setProbeKey)
			for i := range w {
				argv = append(argv, memberName(start+i))
			}
			return argv
		},
	}
}

// SAddMember builds a set of Members members and probes it with SADD of a random
// existing member, the write hot path into one large set. This is distinct from the
// flat sadd workload, which spreads single-member writes across the whole key space of
// many small sets; here every write lands in the same large collection on an already
// present member, so it resolves to the create-if-absent reject branch (locate the
// member row in a big sub-tree, find it present, add nothing), which is the branch a
// write model must keep cheap.
func SAddMember(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	sadd := []byte("SADD")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{sadd, setProbeKey, memberName(sel(seq))}
		},
	}
}

// SRem builds a set of Members members and probes it with SREM on a random member, the
// destructive member removal. Like HDEL it drains the collection over a sustained run:
// once a member is gone, re-removing it returns 0, the same cheap path on aki, Redis,
// and Valkey, so a drained tail does not bias the ratio even though it stops measuring
// the populated-remove cost. Size Members at or above the op budget for a run that
// removes a live member throughout.
func SRem(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	srem := []byte("SREM")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{srem, setProbeKey, memberName(sel(seq))}
		},
	}
}

// SRandMember builds a set of Members members and probes it with the no-count
// SRANDMEMBER, one uniform random member off the ordered index. This is the read the
// audit points at (spec 2064/f1_rewrite_ltm/06 section 10.1): a per-member storage model
// must answer a random member with an O(log n) order-statistic seek, never an O(n) count,
// so on a multi-million member set aki stays flat while a model that counts to a random
// offset falls behind. It is non-destructive, so the set stays fully populated for the run.
func SRandMember(s Spec) Plan {
	s = s.withDefaults()
	srandmember := []byte("SRANDMEMBER")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{srandmember, setProbeKey}
		},
	}
}

// SRandMemberCount builds a set of Members members and probes it with the positive-count
// SRANDMEMBER over a window, the distinct-sample form. Each probe draws rangeWindow
// distinct members with no duplicates, so it exercises the uniform-without-replacement
// sampler: a window of O(log n) seeks below the half-cardinality crossover, which is the
// bounded-batch cost a random-sample model must keep flat as the set grows. It is
// non-destructive, so the set stays full for the whole run.
func SRandMemberCount(s Spec) Plan {
	s = s.withDefaults()
	srandmember := []byte("SRANDMEMBER")
	cnt := intArg(min(int64(s.Members), int64(rangeWindow)))
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{srandmember, setProbeKey, cnt}
		},
	}
}

// SPop builds a set of Members members and probes it with the no-count SPOP, the
// destructive random draw. Like SRem it drains the set over a sustained run, one member
// per probe: it seeks a uniform random member off the ordered index, returns it, and
// removes it, so it measures the random-select-and-remove path a model must keep O(log n)
// on both sides rather than counting to the drawn offset. Size Members at or above the op
// budget so a live member is popped throughout; once drained, SPOP returns nil on the same
// cheap empty path across aki, Redis, and Valkey.
func SPop(s Spec) Plan {
	s = s.withDefaults()
	spop := []byte("SPOP")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    setPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{spop, setProbeKey}
		},
	}
}

// setPlans returns the set operator plans this file adds, keyed by name. They merge
// into PlanRegistry so main dispatches them the same way as the other collection plans.
func setPlans() map[string]func(Spec) Plan {
	return map[string]func(Spec) Plan{
		"scard":            SCard,
		"smismember":       SMIsMember,
		"saddmember":       SAddMember,
		"srem":             SRem,
		"srandmember":      SRandMember,
		"srandmembercount": SRandMemberCount,
		"spop":             SPop,
	}
}

// setPlanNames lists the set operator workloads in a stable order.
func setPlanNames() []string {
	return []string{"scard", "smismember", "saddmember", "srem", "srandmember", "srandmembercount", "spop"}
}
