package workload

import "strconv"

// streamSpreadN is the number of distinct streams the multi-stream consumer-group
// probe spreads across, so the load lands on many keys (and thus many shards on a
// sharded engine) instead of concentrating on one hot stream. It is the realistic
// consumer-group shape: production stream fleets run many groups over many streams,
// not one shared stream every client hammers.
const streamSpreadN = 256

// streamSpreadDepth is the per-stream preloaded undelivered depth, enough that the
// sustained one-add-one-deliver balance never drains a stream to empty.
const streamSpreadDepth = 64

// spreadStreamKey names the j-th spread stream.
func spreadStreamKey(j int64) []byte {
	return []byte("stream:sp:" + strconv.FormatInt(j, 10))
}

// XReadGroupN is the multi-stream twin of XReadGroup: the same sustained
// one-add-one-deliver consumer-group probe, but spread across streamSpreadN
// distinct streams instead of one shared key. Each connection reads its own stream
// (conn modulo N), so on a sharded engine the load fans across shards the way a
// real multi-group deployment does. The single-stream XReadGroup measures one
// shard against a single-threaded rival's whole thread; this measures the engine's
// aggregate consumer-group throughput, the figure the 2x gate is about.
func XReadGroupN(s Spec) Plan {
	s = s.withDefaults()
	xadd := []byte("XADD")
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
	one := intArg(1)
	streams := []byte("STREAMS")
	gt := []byte(">")
	// Preload lays out each stream in turn: its one XGROUP CREATE then depth XADDs,
	// a single sequential pass on one connection so every stream ends populated.
	perStream := int64(1 + streamSpreadDepth)
	return Plan{
		PreloadOps: int64(streamSpreadN) * perStream,
		Preload: func(conn int, seq int64) [][]byte {
			j := seq / perStream
			local := seq % perStream
			sk := spreadStreamKey(j)
			if local == 0 {
				return [][]byte{xgroup, create, sk, grp, zero, mkstream}
			}
			return [][]byte{xadd, sk, star, field, val}
		},
		Probe: sustained(
			func(conn int, seq int64) [][]byte {
				sk := spreadStreamKey(int64(conn % streamSpreadN))
				return [][]byte{xadd, sk, star, field, val}
			},
			func(conn int, seq int64) [][]byte {
				sk := spreadStreamKey(int64(conn % streamSpreadN))
				return [][]byte{xreadgroup, group, grp, consumer, count, one, streams, sk, gt}
			},
		),
	}
}
