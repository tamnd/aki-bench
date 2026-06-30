#!/bin/bash
# In-memory-fit collection matrix: aki vs Redis 8.8 vs Valkey 9.1.
#
# The other half of the bench (spec 2064/ltm/06 and 07). The larger-than-memory
# scenario (../ltm-collections) caps RAM below the dataset and measures the serve-
# time win aki gets by holding the collection in a single file. This scenario does
# the opposite: the dataset fits in RAM for every engine, there is no cap, the run
# is warm, and the race is pure CPU and wire path. This is the regime Redis and
# Valkey were built for and the one where aki's coll-form sub-tree is a structural
# disadvantage on a point read (a btree descent against one hash probe), so it is
# the harder of the two regimes and the one the audit flags DESCENT-RISK.
#
# Unlike the LTM scenario this needs no root, no cgroup scope, and no drop_caches.
# It drives the aki-bench binary, which launches all three engines, runs the
# collection point-read plans (with their preload phase), and prints the 2x gate.
# Each command is run at two pipeline depths (P1 for the per-op latency floor, P16
# for throughput) and under two access patterns (uniform, which is the hot tier's
# worst case, and zipfian, which a read cache is built to exploit). Both are
# reported; neither is silently dropped.
set -u

BENCH=${BENCH:-aki-bench}                 # the aki-bench binary
MEMBERS=${MEMBERS:-2000000}               # elements in the one probed collection
VALUE=${VALUE:-64}                        # hash value size, bytes
CONNS=${CONNS:-50}
DURATION=${DURATION:-10s}
GATE=${GATE:-2.0}
AKI_BIN=${AKI_BIN:-aki}
REDIS_BIN=${REDIS_BIN:-redis-server}
VALKEY_BIN=${VALKEY_BIN:-valkey-server}

CMDS=(sismember hget zscore zrank)
PIPES=(${PIPES:-1 16})
DISTS=(${DISTS:-uniform zipfian})

echo "== IN-MEMORY-FIT collection matrix: $MEMBERS elements, no cap, warm =="
echo "== gate=${GATE}x over both Redis and Valkey; uniform is the hot tier's worst case, zipfian its best =="

fail=0
for cmd in "${CMDS[@]}"; do
  for dist in "${DISTS[@]}"; do
    for pipe in "${PIPES[@]}"; do
      echo "--- $cmd  dist=$dist  P$pipe ---"
      "$BENCH" \
        -workload "$cmd" \
        -members "$MEMBERS" \
        -value-size "$VALUE" \
        -dist "$dist" \
        -connections "$CONNS" \
        -pipeline "$pipe" \
        -duration "$DURATION" \
        -gate "$GATE" \
        -aki-bin "$AKI_BIN" \
        -redis-bin "$REDIS_BIN" \
        -valkey-bin "$VALKEY_BIN" || fail=1
    done
  done
done

if [ "$fail" != 0 ]; then
  echo "== at least one cell missed the ${GATE}x gate; see rows above =="
  exit 2
fi
echo "== every cell cleared the ${GATE}x gate =="
