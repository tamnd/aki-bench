#!/bin/bash
# Size sweep: how the aki-vs-Redis-vs-Valkey ratio moves as the working set grows,
# swept on the axis each op class actually reads. This is the reproducible, in-repo
# answer to "what size keeps the 2x verdict faithful, and where does it drift", the
# question the ad-hoc box scripts used to answer by hand.
#
# The axis matters and is easy to get wrong. Flat ops (the string row and the point
# writes) key off the top-level key space, so their size is -keys; a set/get ignores
# -members entirely. Collection and algebra ops key off one collection's cardinality,
# so their size is -members; a flat -keys does nothing for them. A sweep that moves the
# wrong knob measures the same run at every "size" and reports a flat ratio that is an
# artifact, not a finding. This script sets -keys and -members both to the swept size
# via aki-bench, so every workload lands at SIZE on the axis it reads, and the ratio
# that comes back is real for that class.
#
# Two classes are swept:
#   flat    - a string point op, size = key space (-keys). O(1) per op, so this asks
#             whether the ratio holds as the resident index and cold log grow.
#   algebra - the seven set-algebra forms as one preload-shared suite, size = per-source
#             cardinality (-members). This is the size-sensitive class: the read forms
#             drift as the reply grows and the STORE forms cross the set encoding
#             boundary, so its verdict genuinely changes across the ladder.
#
# It drives the aki-bench binary, which launches all three servers, and prints the
# native per-workload comparison (ratio vs both plus the gate) for every size. The gate
# is informational here: a size sweep is a measurement, not a pass/fail, so a cell under
# the bar is expected and does not stop the sweep.
set -u

BENCH=${BENCH:-aki-bench}
VALUE=${VALUE:-64}
CONNS=${CONNS:-50}
DURATION=${DURATION:-6s}                   # long enough that algebra is not 4s-jittery
GATE=${GATE:-2.0}
F1SRV_BIN=${F1SRV_BIN:-f1srv}
REDIS_BIN=${REDIS_BIN:-redis-server}
VALKEY_BIN=${VALKEY_BIN:-valkey-server}
AKI_NET=${AKI_NET:-}
CPU_SERVER=${CPU_SERVER:-}
CPU_CLIENT=${CPU_CLIENT:-}

# The ladder spans the tiny end (where the STORE forms still fit a small set) through
# the coll-form regime. Override SIZES to zoom in on a band.
SIZES=(${SIZES:-1 10 100 1000 10000 100000 1000000 2000000})
# The flat op swept over the key space, and the algebra suite swept over the member
# space. Override to sweep other flat workloads (for example "get").
FLAT_WL=${FLAT_WL:-set}
ALGEBRA_SUITE=${ALGEBRA_SUITE:-sinter,sunion,sdiff,sintercard,sinterstore,sunionstore,sdiffstore}
DISTS=(${DISTS:-uniform})
PIPES=(${PIPES:-1})

SPLIT=()
if [ -n "$CPU_SERVER" ] && [ -n "$CPU_CLIENT" ]; then
  SPLIT=(-cpu-split -cpu-server "$CPU_SERVER" -cpu-client "$CPU_CLIENT")
fi

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
    -f1srv-bin "$F1SRV_BIN" \
    -redis-bin "$REDIS_BIN" \
    -valkey-bin "$VALKEY_BIN" \
    -aki-net "$AKI_NET" \
    ${SPLIT[@]+"${SPLIT[@]}"} || true
}

echo "== SIZE SWEEP: flat=$FLAT_WL (key-space axis), algebra suite (member axis) =="
echo "== sizes: ${SIZES[*]}; net=${AKI_NET:-goroutine-loop} split=${CPU_SERVER:-none}/${CPU_CLIENT:-none}; gate ${GATE}x informational =="

for dist in "${DISTS[@]}"; do
  for pipe in "${PIPES[@]}"; do
    echo "############ flat $FLAT_WL  dist=$dist  P$pipe ############"
    for size in "${SIZES[@]}"; do
      echo "--- $FLAT_WL keys=$size dist=$dist P$pipe ---"
      runcell "$FLAT_WL" "$size" "$dist" "$pipe"
    done
    echo "############ algebra suite  dist=$dist  P$pipe ############"
    for size in "${SIZES[@]}"; do
      echo "--- algebra suite members=$size dist=$dist P$pipe ---"
      runcell "$ALGEBRA_SUITE" "$size" "$dist" "$pipe"
    done
  done
done
echo "== SIZE SWEEP DONE =="
