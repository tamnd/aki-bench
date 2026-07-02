package workload

import (
	"github.com/tamnd/aki-bench/load"
)

// This file gives the list type full operator coverage as per-collection probes,
// the list analogue of set.go and zset.go. range.go already carries the three list
// reads it groups with the other range probes (lrange, lindex, lpop); this file
// fills in the rest of the surface so a sweep reports a full list ratio matrix: the
// O(1) length, the random-index write (lset), the value search (lpos), the interior
// mutations (linsert, lrem), the two-key move (rpoplpush), and the deep push into
// one large list (lpushhead, rpushtail) that is the write hot path the in-memory-fit
// audit flags as the weakest cell.
//
// Every plan builds the same single list, listProbeKey, with Members elements
// m0..m{n-1} in index order over one sequential RPUSH preload pass, then probes one
// operator against it. That matches how LRange, LIndex, and LPop in range.go build
// their list, so every list plan works against one consistent large list. The reads
// and meta ops leave the list intact; the mutating probes note their own drain or
// growth effect inline the way set.go does.

// listProbeKey is the single list every list plan builds and probes. It matches the
// key the range.go list plans target ("list:" + collKey) so the whole list surface
// is measured against one large list. One large list is the case the audit cares
// about: on the larger-than-memory side an index-window sub-tree paged from disk,
// and on the in-memory-fit side the push a modern layout must answer as fast as a
// Redis quicklist tail-append.
var listProbeKey = []byte("list:" + collKey)

// listPreload writes one element per sequence step into listProbeKey with RPUSH, so
// a single sequential connection walking 0..Members-1 puts element i at list index i.
// Deterministic index order is what makes the lset, lindex, and linsert probes land
// on a known element.
func listPreload() load.CommandGen {
	rpush := []byte("RPUSH")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{rpush, listProbeKey, memberName(seq)}
	}
}

// LLen builds a list of Members elements and probes it with LLEN, the O(1) length.
// It reads the maintained header count with no walk, so it is the cheapest list op
// and the one a per-element storage model must not regress into a count-by-scan, the
// list analogue of SCARD and HLEN.
func LLen(s Spec) Plan {
	s = s.withDefaults()
	llen := []byte("LLEN")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{llen, listProbeKey}
		},
	}
}

// LSet builds a list of Members elements and probes it with LSET at a random index,
// the in-place overwrite. It seeks a single element by index in a large list and
// rewrites its value with no reflow of its neighbors, so it measures the point-write
// path into one big list rather than the tail append the flat lpush/rpush workloads
// spread across many small lists. The written value is the same size class as the
// element it replaces, so the list neither grows nor shrinks over the run.
func LSet(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	lset := []byte("LSET")
	val := value(s.ValueSize)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{lset, listProbeKey, intArg(sel(seq)), val}
		},
	}
}

// LPos builds a list of Members elements and probes it with LPOS for a random
// element value, the value search. Element m{i} sits at index i, so the probe asks
// for a value that exists and the server returns its index; a per-element model must
// answer it off the member index as a bounded seek, not an O(n) walk from the head.
// It is non-destructive, so the list stays fully populated for the run.
func LPos(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	lpos := []byte("LPOS")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{lpos, listProbeKey, memberName(sel(seq))}
		},
	}
}

// LInsert builds a list of Members elements and probes it with LINSERT BEFORE a
// random pivot, the interior insert. It finds an existing element by value and
// splices a new element in before it, the interior mutation the list model resolves
// on the index window without reflowing the whole list (spec 2064/f1_rewrite_ltm
// list model). The list grows by one element per probe, so over a sustained run it
// measures the populated-insert cost rather than a create-empty artifact.
func LInsert(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	linsert := []byte("LINSERT")
	before := []byte("BEFORE")
	ins := []byte("inserted")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{linsert, listProbeKey, before, memberName(sel(seq)), ins}
		},
	}
}

// LRem builds a list of Members elements and probes it with LREM of a random element
// value, the destructive value removal. Like SREM it drains the list over a sustained
// run: once an element is gone, re-removing it returns 0, the same cheap not-found
// path across aki, Redis, and Valkey, so a drained tail does not bias the ratio. Size
// Members at or above the op budget for a run that removes a live element throughout.
func LRem(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	lrem := []byte("LREM")
	count := intArg(1)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{lrem, listProbeKey, count, memberName(sel(seq))}
		},
	}
}

// RPopLPush builds a list of Members elements and probes it with RPOPLPUSH from
// listProbeKey into a sibling destination list, the two-key atomic move. It is the
// first list write that touches two collections at once, so it exercises the two-key
// path the point ops never needed: pop the source tail and push it onto the
// destination head while both header counts stay in step. Like LPop it drains the
// source over a sustained run, one element per probe; the destination grows to mirror
// it, so it measures the populated move cost throughout, and once the source drains
// RPOPLPUSH returns nil on the same cheap empty path across all three servers.
func RPopLPush(s Spec) Plan {
	s = s.withDefaults()
	rpoplpush := []byte("RPOPLPUSH")
	dstKey := []byte("list:" + collKey + ":moved")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{rpoplpush, listProbeKey, dstKey}
		},
	}
}

// RPushTail builds a list of Members elements and probes it with RPUSH of one element
// onto listProbeKey, the deep tail append. This is distinct from the flat rpush
// workload, which spreads single-element pushes across the whole key space of many
// short lists; here every push lands on the tail of the same large list, so it
// measures the append hot path into a deep collection, the case the in-memory-fit
// audit flags as the weakest write (lpush/rpush collapse at pipeline depth). The list
// grows one element per probe, so it stays the deep-list append throughout.
func RPushTail(s Spec) Plan {
	s = s.withDefaults()
	rpush := []byte("RPUSH")
	val := value(s.ValueSize)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{rpush, listProbeKey, val}
		},
	}
}

// LPushHead is RPushTail's head-side twin: LPUSH of one element onto the head of the
// same large list. It measures the head-insert hot path into a deep collection, which
// on a per-element index is the more demanding side because a new head element shifts
// every following element's logical position; the model must absorb that without
// reflowing the list (spec 2064/f1_rewrite_ltm list model). The list grows one element
// per probe, so it stays the deep-list prepend throughout.
func LPushHead(s Spec) Plan {
	s = s.withDefaults()
	lpush := []byte("LPUSH")
	val := value(s.ValueSize)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    listPreload(),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{lpush, listProbeKey, val}
		},
	}
}

// listPlans returns the list operator plans this file adds, keyed by name. They merge
// into PlanRegistry so main dispatches them the same way as the other collection plans.
func listPlans() map[string]func(Spec) Plan {
	return map[string]func(Spec) Plan{
		"llen":      LLen,
		"lset":      LSet,
		"lpos":      LPos,
		"linsert":   LInsert,
		"lrem":      LRem,
		"rpoplpush": RPopLPush,
		"rpushtail": RPushTail,
		"lpushhead": LPushHead,
	}
}

// listPlanNames lists the list operator workloads in a stable order.
func listPlanNames() []string {
	return []string{"llen", "lset", "lpos", "linsert", "lrem", "rpoplpush", "rpushtail", "lpushhead"}
}
