#!/bin/bash
# In-memory-fit matrix across every type: aki vs Redis 8.8 vs Valkey 9.1.
#
# The in-memory half of the bench (spec 2064/ltm/06 and 07, and the per-type
# methodology mapping in f1_rewrite_ltm/13 section 6). The larger-than-memory
# scenario (../ltm-collections) caps RAM below the dataset and measures the serve-
# time win aki gets by holding the data in a single file. This scenario does the
# opposite: the dataset fits in RAM for every engine, there is no cap, the run is
# warm, and the race is pure CPU and wire path. This is the regime Redis and Valkey
# were built for and the harder of the two for aki: strings race a hand-tuned C
# event loop, and a coll-form point read is a btree descent against one hash probe,
# the case the audit flags DESCENT-RISK. Most cells legitimately miss the 2x bar
# today; the gate is the end-state target the type milestones (M1-M8) drive toward,
# so a non-zero exit during the build-out is expected, not a harness bug.
#
# Coverage is the full per-type mapping: every type along three shapes, a point op,
# a bounded range or scan, and an algebra or aggregate, plus the type's point write.
# A type's 2x verdict is read across all of its shapes, not one lucky command.
#
# Engine per type. f1raw is the product engine and the default; it is strings-only
# in this slice (served by its own binary, f1srv), so the string rows run on f1raw
# and every collection row runs on the btree legacy engine until collections land
# on f1raw (M4-M8). The engine each group runs on is printed, so the legacy-vs-
# product split is explicit rather than hidden.
#
# Unlike the LTM scenario this needs no root, no cgroup scope, and no drop_caches.
# It drives the aki-bench binary, which launches all three engines, runs the
# workload (with its preload phase for the read and collection plans), and prints
# the 2x gate. Each command is run at two pipeline depths (P1 for the per-op latency
# floor, P16 for throughput) and under two access patterns (uniform, the hot tier's
# worst case, and zipfian, which a read cache is built to exploit). Both are
# reported; neither is silently dropped.
set -u

BENCH=${BENCH:-aki-bench}                 # the aki-bench binary
MEMBERS=${MEMBERS:-2000000}               # elements in the one probed collection
VALUE=${VALUE:-64}                        # value size, bytes
CONNS=${CONNS:-50}
DURATION=${DURATION:-10s}
GATE=${GATE:-2.0}
AKI_BIN=${AKI_BIN:-aki}                   # serves the legacy btree engine (collections)
F1SRV_BIN=${F1SRV_BIN:-f1srv}             # serves the f1raw product engine (strings)
REDIS_BIN=${REDIS_BIN:-redis-server}
VALKEY_BIN=${VALKEY_BIN:-valkey-server}

# Each row is "engine|type|workload workload ...". String, hash, set, and zset now
# run on the f1raw product engine: their point, enumeration, and algebra paths are
# built on f1raw (milestones M4-M6, served by f1srv). List and stream are not yet on
# f1raw, so they run on the btree legacy engine until M7-M8 land; the engine per row
# is printed so the split stays explicit. The name is ROWS, not GROUPS: GROUPS is a
# special bash array holding the caller's group ids, and assigning to it is silently
# ignored, so the matrix would expand to the user's gid list.
ROWS=(
  "f1raw|string|set get incr getrange"
  "f1raw|hash|hset hget hscan hgetall"
  "f1raw|set|sadd sismember sscan sinter"
  "f1raw|zset|zadd zscore zrange zrank zunion"
  "btree|list|lpush lrange lpop lindex"
  "btree|stream|xadd xrange xread xreadgroup"
)

PIPES=(${PIPES:-1 16})
DISTS=(${DISTS:-uniform zipfian})

echo "== IN-MEMORY-FIT full-type matrix: $MEMBERS elements, no cap, warm =="
echo "== gate=${GATE}x over both Redis and Valkey; string/hash/set/zset on f1raw (product), list/stream on btree (legacy) until M7-M8 =="
echo "== uniform is the hot tier's worst case, zipfian its best; most cells miss the bar today and that is expected =="

fail=0
for row in "${ROWS[@]}"; do
  engine=${row%%|*}         # text before the first |
  rest=${row#*|}            # everything after the first |
  typ=${rest%%|*}           # text before the next |
  cmds=${rest#*|}           # the space-separated workload list
  echo "=== $typ on the $engine engine ==="
  for cmd in $cmds; do
    for dist in "${DISTS[@]}"; do
      for pipe in "${PIPES[@]}"; do
        echo "--- $cmd  ($typ/$engine)  dist=$dist  P$pipe ---"
        "$BENCH" \
          -workload "$cmd" \
          -aki-engine "$engine" \
          -members "$MEMBERS" \
          -value-size "$VALUE" \
          -dist "$dist" \
          -connections "$CONNS" \
          -pipeline "$pipe" \
          -duration "$DURATION" \
          -gate "$GATE" \
          -aki-bin "$AKI_BIN" \
          -f1srv-bin "$F1SRV_BIN" \
          -redis-bin "$REDIS_BIN" \
          -valkey-bin "$VALKEY_BIN" || fail=1
      done
    done
  done
done

if [ "$fail" != 0 ]; then
  echo "== at least one cell missed the ${GATE}x gate; see rows above (expected until the type milestones land) =="
  exit 2
fi
echo "== every cell cleared the ${GATE}x gate =="
