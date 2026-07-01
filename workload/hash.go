package workload

import (
	"strconv"

	"github.com/tamnd/aki-bench/load"
)

// This file gives the hash type full operator coverage as per-collection probes:
// every hash command the servers answer, measured against one large preloaded hash
// so the comparison is aki versus Redis versus Valkey on the same operator in the
// same regime. collection.go carries the hash point read (hget) and range.go the two
// materialize forms (hgetall, hscan); this file fills in the rest of the surface so a
// sweep can report a full hash ratio matrix rather than a single point read.
//
// Every plan builds the same single hash, hashProbeKey, with Members fields f0..f{n-1}
// over one sequential preload pass, then probes one operator against it. The reads and
// meta ops are non-destructive so the hash stays fully populated for the whole run; the
// write and delete probes note their own regime effects inline.

// hashProbeKey is the single hash every hash plan builds and probes, the hash analogue
// of the set/zset probe keys. One large hash is the case the audit cares about: on the
// larger-than-memory side it is a multi-million field sub-tree paged from disk, and on
// the in-memory-fit side it is the descent a modern layout has to answer as fast as a
// flat Redis hash probe.
var hashProbeKey = []byte("hash:" + collKey)

// hashField names the field at an index, matching the f0..f{Members-1} space the
// preload writes so a probe field is always a hit.
func hashField(idx int64) []byte {
	return []byte("f" + strconv.FormatInt(idx, 10))
}

// hashPreload writes one field per sequence step into hashProbeKey, so a single
// sequential connection walking 0..Members-1 populates every field exactly once.
func hashPreload(val []byte) load.CommandGen {
	hset := []byte("HSET")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{hset, hashProbeKey, hashField(seq), val}
	}
}

// hashWindow returns the number of fields a multi-field probe (HMGET) requests: the
// standard range window, clamped to the member space so a small hash still yields a
// full-hit probe rather than requesting fields past the end.
func hashWindow(members int) int64 {
	return min(int64(members), int64(rangeWindow))
}

// HMGet builds a hash of Members fields and probes it with HMGET over a window of
// fields, the multi-field batch read. The window starts at a random in-range field so
// the batch is a full hit and the reply size is fixed at the window, not the hash size.
func HMGet(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	val := value(s.ValueSize)
	hmget := []byte("HMGET")
	w := hashWindow(s.Members)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			start := windowStart(sel(seq), int64(s.Members))
			argv := make([][]byte, 0, 2+w)
			argv = append(argv, hmget, hashProbeKey)
			for i := range w {
				argv = append(argv, hashField(start+i))
			}
			return argv
		},
	}
}

// HExists builds a hash of Members fields and probes it with HEXISTS on a random
// existing field, the field presence check: a single probe that resolves to a hit
// without copying the value.
func HExists(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	val := value(s.ValueSize)
	hexists := []byte("HEXISTS")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hexists, hashProbeKey, hashField(sel(seq))}
		},
	}
}

// HLen builds a hash of Members fields and probes it with HLEN, the O(1) field count.
// It reads the maintained header count with no scan, so it is the cheapest hash op and
// the one where a per-field storage model must not regress into a count-by-scan.
func HLen(s Spec) Plan {
	s = s.withDefaults()
	val := value(s.ValueSize)
	hlen := []byte("HLEN")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hlen, hashProbeKey}
		},
	}
}

// HStrlen builds a hash of Members fields and probes it with HSTRLEN on a random
// existing field, the field value length: a point probe that reads the value length
// without shipping the value.
func HStrlen(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	val := value(s.ValueSize)
	hstrlen := []byte("HSTRLEN")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hstrlen, hashProbeKey, hashField(sel(seq))}
		},
	}
}

// HKeys builds a hash of Members fields and probes it with HKEYS, the field-name
// enumeration. Like HGETALL it is a whole-collection read, but names only, so it
// measures the ordered-enumeration path without the value shipping HGETALL adds. Keep
// Members modest: a multi-million field HKEYS is a reply-size benchmark.
func HKeys(s Spec) Plan {
	s = s.withDefaults()
	val := value(s.ValueSize)
	hkeys := []byte("HKEYS")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hkeys, hashProbeKey}
		},
	}
}

// HVals builds a hash of Members fields and probes it with HVALS, the value
// enumeration: the same ordered walk as HKEYS but shipping each value, so it pairs with
// HKeys to separate the enumeration cost from the value-shipping cost.
func HVals(s Spec) Plan {
	s = s.withDefaults()
	val := value(s.ValueSize)
	hvals := []byte("HVALS")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hvals, hashProbeKey}
		},
	}
}

// HSetField builds a hash of Members fields and probes it with HSET overwriting a
// random existing field, the write hot path into one large hash. This is distinct from
// the flat hset workload, which spreads single-field writes across the whole key space
// of many small hashes; here every write lands in the same large collection, which is
// the larger-than-memory write path (locate the field row in a big sub-tree and update
// it in place).
func HSetField(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	val := value(s.ValueSize)
	hset := []byte("HSET")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hset, hashProbeKey, hashField(sel(seq)), val}
		},
	}
}

// HSetNX builds a hash of Members fields and probes it with HSETNX on a random existing
// field, the create-if-absent reject path: every probed field already exists, so the
// command resolves to a fast no-write after the presence check, which is the branch a
// write model must keep cheap (it must not pay a full write to discover the field is
// there).
func HSetNX(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	val := value(s.ValueSize)
	hsetnx := []byte("HSETNX")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hsetnx, hashProbeKey, hashField(sel(seq)), val}
		},
	}
}

// HDel builds a hash of Members fields and probes it with HDEL on a random field, the
// destructive field removal. Like LPOP it drains the collection over a sustained run:
// once a field is gone, re-deleting it returns 0, the same cheap path on aki, Redis,
// and Valkey, so a drained tail does not bias the ratio even though it stops measuring
// the populated-delete cost. Size Members at or above the op budget for a run that
// deletes a live field throughout.
func HDel(s Spec) Plan {
	s = s.withDefaults()
	sel := s.memberSelector()
	val := value(s.ValueSize)
	hdel := []byte("HDEL")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload:    hashPreload(val),
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{hdel, hashProbeKey, hashField(sel(seq))}
		},
	}
}

// hashPlans returns the hash operator plans this file adds, keyed by name. They merge
// into PlanRegistry so main dispatches them the same way as the other collection plans.
func hashPlans() map[string]func(Spec) Plan {
	return map[string]func(Spec) Plan{
		"hmget":     HMGet,
		"hexists":   HExists,
		"hlen":      HLen,
		"hstrlen":   HStrlen,
		"hkeys":     HKeys,
		"hvals":     HVals,
		"hsetfield": HSetField,
		"hsetnx":    HSetNX,
		"hdel":      HDel,
	}
}

// hashPlanNames lists the hash operator workloads in a stable order.
func hashPlanNames() []string {
	return []string{"hmget", "hexists", "hlen", "hstrlen", "hkeys", "hvals", "hsetfield", "hsetnx", "hdel"}
}
