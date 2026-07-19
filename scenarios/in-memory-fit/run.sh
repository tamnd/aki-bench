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
# Sizing is per op class, because a single size lies. Flat point ops (the string row
# and the point writes) key off the top-level key space, so their size knob is -keys;
# collection ops key off one collection's cardinality, so their knob is -members. This
# script passes both flags set to the same size, so every workload is sized on the axis
# it actually reads (a flat workload that only reads -keys, or a plan that only reads
# -members, both land at SIZE) rather than the old footgun where a size sweep moved
# -members while a flat set/get quietly stayed at the default key space. Collection and
# flat ops run at one coll-form size (FIT_SIZE): large enough that a set or hash is in
# the partitioned coll form the audit calls DESCENT-RISK, not a listpack shortcut.
#
# Set algebra is the exception: its verdict genuinely differs across the set encoding
# boundary (a destination that fits a listpack or intset versus one that spills to the
# coll form), so it runs at two bracketing sizes, a small one below the boundary and
# FIT_SIZE above it, and a single size would report only half the truth. The seven
# algebra forms share one preload (they build the same two source sets and differ only
# in the probe), so they run as one comma-joined suite: aki-bench builds the sources
# once per size and probes them with every form, instead of paying the multi-member
# source build once per command.
#
# Engine per type. f1raw is the product engine and the default, served by its own
# binary, f1srv. Every type now runs on it: strings, hash, set, and zset landed on
# f1raw in M4-M6, list in M7, and stream in M8, so the whole matrix races the product
# engine rather than the legacy btree. The engine each group runs on is still printed
# so the routing stays explicit.
#
# Unlike the LTM scenario this needs no root, no cgroup scope, and no drop_caches.
# It drives the aki-bench binary, which launches all three engines, runs the
# workload (with its preload phase for the read and collection plans), and prints
# the 2x gate. Each command is run at two pipeline depths (P1 for the per-op latency
# floor, P16 for throughput) and under two access patterns (uniform, the hot tier's
# worst case, and zipfian, which a read cache is built to exploit). Both are
# reported; neither is silently dropped.
#
# The canonical gate wants the reactor net path and a cpu-split so the server and the
# load generator never share a core. Both are off by default so the script also runs
# on a laptop, and both turn on from the environment: set AKI_NET=reactor on Linux,
# and set CPU_SERVER and CPU_CLIENT to disjoint taskset lists (for example
# CPU_SERVER=4-17 CPU_CLIENT=18-31) to pin the halves. Off Linux the reactor and the
# split are unavailable, so the numbers are a same-box A/B, not the saturation gate.
set -u

BENCH=${BENCH:-aki-bench}                 # the aki-bench binary
FIT_SIZE=${FIT_SIZE:-1000000}             # key space and collection cardinality for the flat and collection rows
ALGEBRA_SMALL=${ALGEBRA_SMALL:-100}       # per-source-set size below the set encoding boundary
ALGEBRA_LARGE=${ALGEBRA_LARGE:-$FIT_SIZE} # per-source-set size above the boundary, in the coll form
VALUE=${VALUE:-64}                        # value size, bytes
CONNS=${CONNS:-50}
DURATION=${DURATION:-10s}
GATE=${GATE:-2.0}
AKI_BIN=${AKI_BIN:-aki}                   # serves any legacy engine if a row asks for one
F1SRV_BIN=${F1SRV_BIN:-f1srv}             # serves the f1raw product engine (every type)
REDIS_BIN=${REDIS_BIN:-redis-server}
VALKEY_BIN=${VALKEY_BIN:-valkey-server}
AKI_NET=${AKI_NET:-}                      # empty for the goroutine loop, reactor on Linux
CPU_SERVER=${CPU_SERVER:-}               # taskset list for the server half, empty to skip
CPU_CLIENT=${CPU_CLIENT:-}               # taskset list for the client half, empty to skip

# Optional cpu-split passthrough, engaged only when both halves are named.
SPLIT=()
if [ -n "$CPU_SERVER" ] && [ -n "$CPU_CLIENT" ]; then
  SPLIT=(-cpu-split -cpu-server "$CPU_SERVER" -cpu-client "$CPU_CLIENT")
fi

# runcell drives one aki-bench invocation at a named size. -keys and -members are
# both set to the size so the workload lands there whichever axis it reads (see the
# sizing note in the header). $1 is the workload argument (one name, or a comma-joined
# suite for set algebra), the rest come from the environment.
runcell() {
  local wl=$1 size=$2 dist=$3 pipe=$4
  "$BENCH" \
    -workload "$wl" \
    -aki-engine f1raw \
    -keys "$size" \
    -members "$size" \
    -value-size "$VALUE" \
    -dist "$dist" \
    -connections "$CONNS" \
    -pipeline "$pipe" \
    -duration "$DURATION" \
    -gate "$GATE" \
    -aki-bin "$AKI_BIN" \
    -f1srv-bin "$F1SRV_BIN" \
    -redis-bin "$REDIS_BIN" \
    -valkey-bin "$VALKEY_BIN" \
    -aki-net "$AKI_NET" \
    ${SPLIT[@]+"${SPLIT[@]}"}
}

# Each row is "type|workload workload ...". Every type runs on the f1raw product
# engine now. Coverage per type reads across shapes: a point read, a bounded range or
# scan, an algebra or aggregate, the point write, and the destructive op (delete or
# pop), since the delete family collapsed at pipeline depth until the coalesced-delete
# change and has to be watched alongside the reads. The setalgebra row is handled
# specially: it runs as one preload-shared suite at two bracketing sizes.
ROWS=(
  "string|set get incr getrange"
  "hash|hset hget hscan hgetall hdel"
  "set|sadd sismember smismember scard sscan smembers srem spop srandmember srandmembercount smove"
  "setalgebra|sinter sunion sdiff sintercard sinterstore sunionstore sdiffstore"
  "zset|zadd zscore zrange zrank zunion zrem"
  "list|lpush lrange lpop lindex"
  "stream|xadd xrange xread xreadgroup"
)

PIPES=(${PIPES:-1 16})
DISTS=(${DISTS:-uniform zipfian})

echo "== IN-MEMORY-FIT full-type matrix: fit_size=$FIT_SIZE, algebra sizes ${ALGEBRA_SMALL}/${ALGEBRA_LARGE}, no cap, warm =="
echo "== gate=${GATE}x over both Redis and Valkey; every type on the f1raw product engine (served by f1srv) =="
echo "== net=${AKI_NET:-goroutine-loop} split=${CPU_SERVER:-none}/${CPU_CLIENT:-none}; reactor+split give the saturation gate, off-Linux is a same-box A/B =="
echo "== uniform is the hot tier's worst case, zipfian its best; most cells miss the bar today and that is expected =="

fail=0
for row in "${ROWS[@]}"; do
  typ=${row%%|*}            # text before the first |
  cmds=${row#*|}            # the space-separated workload list
  echo "=== $typ on the f1raw engine ==="
  if [ "$typ" = "setalgebra" ]; then
    # Join the seven forms into one suite so the two source sets are built once per
    # size, then run the suite at both bracketing sizes. The suite exits non-zero if
    # any form inside it misses the gate.
    suite=$(echo "$cmds" | tr ' ' ',')
    for size in "$ALGEBRA_SMALL" "$ALGEBRA_LARGE"; do
      for dist in "${DISTS[@]}"; do
        for pipe in "${PIPES[@]}"; do
          echo "--- setalgebra suite  members=$size  dist=$dist  P$pipe ---"
          runcell "$suite" "$size" "$dist" "$pipe" || fail=1
        done
      done
    done
    continue
  fi
  for cmd in $cmds; do
    for dist in "${DISTS[@]}"; do
      for pipe in "${PIPES[@]}"; do
        echo "--- $cmd  ($typ)  size=$FIT_SIZE  dist=$dist  P$pipe ---"
        runcell "$cmd" "$FIT_SIZE" "$dist" "$pipe" || fail=1
      done
    done
  done
done

if [ "$fail" != 0 ]; then
  echo "== at least one cell missed the ${GATE}x gate; see rows above (expected until the type milestones land) =="
  exit 2
fi
echo "== every cell cleared the ${GATE}x gate =="
