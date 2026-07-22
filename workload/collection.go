package workload

import (
	"maps"
	"strconv"

	"github.com/tamnd/aki-bench/load"
)

// Plan is a collection point-read workload: a Preload generator that builds one
// collection of Members elements, and a Probe generator that does a single random
// point read against it. The two phases are separate because the collection has to
// exist and be fully populated before the measured reads start, and because the
// preload runs once on a single connection (sequential, full coverage) while the
// probe runs the normal multi-connection closed loop.
//
// Probe is what gets timed. Preload runs first, for PreloadOps operations, with
// Connections 1 so a single sequence 0..PreloadOps-1 covers every member exactly
// once. A multi-connection preload would have each connection restart its sequence
// at 0 and under-populate the collection, so the caller must drive the preload with
// one connection.
type Plan struct {
	Preload    load.CommandGen
	Probe      load.CommandGen
	PreloadOps int64
}

// collKey is the single collection every point-read plan targets. One large
// collection is the case the larger-than-memory audit cares about (a multi-million
// element sub-tree on disk) and the case the in-memory-fit audit calls DESCENT-RISK
// (a btree descent where Redis answers with one hash probe).
const collKey = "probe"

// memberName is the member at a given index. Preload writes m0..m{Members-1}; the
// probe reads m{selector(seq)}, so every probed member exists and the read is a hit,
// which is the path the audit measures (a miss short-circuits before the work).
func memberName(idx int64) []byte {
	return []byte("m" + strconv.FormatInt(idx, 10))
}

// refillName is a member name a non-draining destructive probe re-adds so the
// collection never empties (sustained below). It is unique per (conn, seq): every
// connection re-adds its own names, so a re-add is always a genuine new member and
// never collides with another connection's, which is what keeps adds and pops in
// balance. A colliding name would make the re-add a no-op while the pop still
// removed a member, so pops would outrun adds and the collection would drain after
// all, the artifact this probe exists to avoid.
func refillName(conn int, seq int64) []byte {
	return []byte("mu" + strconv.Itoa(conn) + "-" + strconv.FormatInt(seq, 10))
}

// sustained turns a destructive probe into a non-draining one by alternating a
// re-add with the pop on seq parity: even seq re-adds, odd seq pops. Half the
// operations repopulate the collection and half consume it, so its cardinality
// holds near the preload for the whole run and the pop keeps hitting a populated
// collection, which is the cost the gate row wants to measure. It also keeps
// value-bearing ops off zero: a pure drain empties the collection and every later
// pop returns nil, so all three servers report zero value-bearing throughput and
// the gate cannot score a ratio (report.gateOpsPerSec). All three servers run the
// identical add/pop mix, so the comparison stays fair. The measured figure is the
// sustained pop-under-replacement rate, not the pure-drain rate, which is the only
// honest way to hold a destructive op's collection populated one command at a time.
func sustained(readd, pop load.CommandGen) load.CommandGen {
	return func(conn int, seq int64) [][]byte {
		if seq&1 == 0 {
			return readd(conn, seq)
		}
		return pop(conn, seq)
	}
}

// SISMember builds a set of Members elements and probes it with SISMEMBER.
func SISMember(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	sadd := []byte("SADD")
	sk := []byte("set:" + collKey)
	sismember := []byte("SISMEMBER")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{sadd, sk, memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{sismember, sk, memberName(sel(conn, seq))}
		},
	}
}

// HGet builds a hash of Members fields and probes it with HGET.
func HGet(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	hset := []byte("HSET")
	hk := []byte("hash:" + collKey)
	hget := []byte("HGET")
	val := value(s.ValueSize)
	field := func(idx int64) []byte { return []byte("f" + strconv.FormatInt(idx, 10)) }
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{hset, hk, field(seq), val}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hget, hk, field(sel(conn, seq))}
		},
	}
}

// ZScore builds a sorted set of Members members and probes it with ZSCORE.
func ZScore(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zadd := []byte("ZADD")
	zk := []byte("zset:" + collKey)
	zscore := []byte("ZSCORE")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{zadd, zk, []byte(strconv.FormatInt(seq, 10)), memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{zscore, zk, memberName(sel(conn, seq))}
		},
	}
}

// ZRank builds a sorted set of Members members and probes it with ZRANK, the
// order-statistics path: the score index has to count members below the target, not
// just locate it, so it stresses the rank index rather than a plain point lookup.
func ZRank(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	zadd := []byte("ZADD")
	zk := []byte("zset:" + collKey)
	zrank := []byte("ZRANK")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{zadd, zk, []byte(strconv.FormatInt(seq, 10)), memberName(seq)}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{zrank, zk, memberName(sel(conn, seq))}
		},
	}
}

// PlanRegistry returns the collection plans keyed by name: the point-read plans
// defined here plus the range, scan, and algebra plans from range.go.
func PlanRegistry() map[string]func(Spec) Plan {
	reg := map[string]func(Spec) Plan{
		"sismember": SISMember,
		"hget":      HGet,
		"zscore":    ZScore,
		"zrank":     ZRank,
	}
	maps.Copy(reg, hashPlans())
	maps.Copy(reg, setPlans())
	maps.Copy(reg, zsetPlans())
	maps.Copy(reg, listPlans())
	maps.Copy(reg, rangePlans())
	maps.Copy(reg, streamPlans())
	return reg
}

// PlanNames lists the collection workloads in a stable order: the point-read
// plans first, then the hash operator plans, then the range, scan, and algebra
// plans, then the stream plans.
func PlanNames() []string {
	names := append([]string{"sismember", "hget", "zscore", "zrank"}, hashPlanNames()...)
	names = append(names, setPlanNames()...)
	names = append(names, zsetPlanNames()...)
	names = append(names, listPlanNames()...)
	names = append(names, rangePlanNames()...)
	return append(names, streamPlanNames()...)
}

// BuildPlan returns the plan for a collection point-read workload name, or false
// if the name is not a collection workload (it may still be a flat workload in the
// Registry).
func BuildPlan(name string, s Spec) (Plan, bool) {
	if ctor, ok := PlanRegistry()[name]; ok {
		return ctor(s), true
	}
	return Plan{}, false
}
