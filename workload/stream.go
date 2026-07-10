package workload

import "github.com/tamnd/aki-bench/load"

// This file adds the stream workloads that complete the per-type coverage the
// methodology requires. XADD is the stream point write, a flat generator like
// the other write workloads. XRANGE, XREAD, and XREADGROUP are the bounded read
// shapes, built as Plans so a single-connection preload fills one stream before
// the timed probe, the same way the collection range plans work.

// XAdd appends one entry to streams across the key space with a server-assigned
// ID. It is the stream point write, the flat write analogue of LPUSH and SADD,
// so it lives in the flat Registry rather than as a Plan: every op creates a new
// entry and the stream grows, no preload needed.
func XAdd(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("XADD")
	star := []byte("*")
	field := []byte("f")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, keyAt("stream:", sel(conn, seq)), star, field, val}
	}
}

// XRange builds a stream of Members entries and probes it with a bounded XRANGE
// over the whole id range capped at COUNT, the stream range read. The - + COUNT
// form is the bound-not-materialize shape: it returns at most rangeWindow entries
// regardless of how long the stream is, so the cost tracks the window, not the
// stream length.
func XRange(s Spec) Plan {
	s = s.withDefaults()
	xadd := []byte("XADD")
	sk := []byte("stream:" + collKey)
	star := []byte("*")
	field := []byte("f")
	val := value(s.ValueSize)
	xrange := []byte("XRANGE")
	dash := []byte("-")
	plus := []byte("+")
	count := []byte("COUNT")
	cnt := intArg(rangeWindow)
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{xadd, sk, star, field, val}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{xrange, sk, dash, plus, count, cnt}
		},
	}
}

// XRead builds a stream of Members entries and probes it with XREAD from id 0
// capped at COUNT, the stream tail-read. Reading from 0 returns the head of the
// stream every time, a bounded non-destructive read, so the probe measures the
// same populated work on every call rather than draining the stream.
func XRead(s Spec) Plan {
	s = s.withDefaults()
	xadd := []byte("XADD")
	sk := []byte("stream:" + collKey)
	star := []byte("*")
	field := []byte("f")
	val := value(s.ValueSize)
	xread := []byte("XREAD")
	count := []byte("COUNT")
	cnt := intArg(rangeWindow)
	streams := []byte("STREAMS")
	zero := []byte("0")
	return Plan{
		PreloadOps: int64(s.Members),
		Preload: func(conn int, seq int64) [][]byte {
			return [][]byte{xadd, sk, star, field, val}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{xread, count, cnt, streams, sk, zero}
		},
	}
}

// XReadGroup builds a stream of Members entries under a consumer group and probes
// it with XREADGROUP of new messages, the consumer-group read. The group has to
// exist before the stream is read, and a Plan preload is a single sequential pass
// on one connection, so seq 0 issues the one-time XGROUP CREATE ... MKSTREAM and
// every later seq issues an XADD. The create uses a start id of 0 so all the
// added entries count as undelivered and the probe's `>` selector delivers them.
//
// XREADGROUP with `>` is destructive in the same sense LPOP is: each delivered
// entry moves into the consumer's pending list and `>` will not return it again,
// so a sustained run drains the undelivered entries and then returns nil. That nil
// path is the same cheap reply on aki, Redis, and Valkey alike, so a drained tail
// keeps the ratio fair the way the LPOP drain does; size -members at or above the
// op budget for a run that delivers a populated batch throughout.
func XReadGroup(s Spec) Plan {
	s = s.withDefaults()
	xadd := []byte("XADD")
	sk := []byte("stream:" + collKey)
	star := []byte("*")
	field := []byte("f")
	val := value(s.ValueSize)
	xgroup := []byte("XGROUP")
	create := []byte("CREATE")
	grp := []byte("g")
	zero := []byte("0")
	mkstream := []byte("MKSTREAM")
	xreadgroup := []byte("XREADGROUP")
	group := []byte("GROUP")
	consumer := []byte("consumer")
	count := []byte("COUNT")
	cnt := intArg(rangeWindow)
	streams := []byte("STREAMS")
	gt := []byte(">")
	return Plan{
		// One extra op for the XGROUP CREATE at seq 0, then Members XADDs.
		PreloadOps: int64(s.Members) + 1,
		Preload: func(conn int, seq int64) [][]byte {
			if seq == 0 {
				return [][]byte{xgroup, create, sk, grp, zero, mkstream}
			}
			return [][]byte{xadd, sk, star, field, val}
		},
		Probe: func(conn int, seq int64) [][]byte {
			return [][]byte{xreadgroup, group, grp, consumer, count, cnt, streams, sk, gt}
		},
	}
}

// streamPlans returns the stream range, tail, and consumer-group plans keyed by
// name. They are merged into PlanRegistry so main dispatches them the same way as
// the other collection plans. XADD is not here: it is a flat write generator in
// the Registry, not a Plan.
func streamPlans() map[string]func(Spec) Plan {
	return map[string]func(Spec) Plan{
		"xrange":     XRange,
		"xread":      XRead,
		"xreadgroup": XReadGroup,
	}
}

// streamPlanNames lists the stream plan workloads in a stable order.
func streamPlanNames() []string {
	return []string{"xrange", "xread", "xreadgroup"}
}
