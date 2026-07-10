#!/bin/bash
# Band-matrix axes for the f3 gate matrix (spec 2064/f3/18 section 2.1): the
# cardinality ladder, the value ladder to 1MiB, and the band-transition ramps.
#
# Three sweeps, all on the f3 engine by default since this scenario exists for
# the f3 rewrite's string surface (set/get/incr/append/getrange in M0):
#
#   1. Cardinality ladder: keyspace 1 / 10 / 10k / 1M at 64B values. For strings
#      the doc 18 R1 ladder is a keyspace ladder, and the 1-key row doubles as
#      the single-hot-key axis every f1 failure lived on.
#   2. Value ladder: 16B to 1MiB at the default keyspace. 64KiB and 1MiB are the
#      giant-value rows where bytes/s (CF20) is the column to watch: when every
#      server sits near the box's copy ceiling the ops/s ratio is manufactured.
#   3. Band-transition ramps: value sizes at half, at, and at double each string
#      placement threshold (1KiB inline-to-separated, 64KiB separated-to-chunked,
#      doc 09), so a cliff at a promotion boundary shows up as a step between
#      adjacent rows instead of hiding between two distant gate points.
#
# Rows land as JSON next to the run under OUT (one file per cell, the cell tuple
# inside each row), so a sweep session leaves quotable rows, not scrollback.
# Off the gate box these are sweep-class numbers, never gate numbers.
set -u

BENCH=${BENCH:-aki-bench}
ENGINE=${ENGINE:-f3}
CONNS=${CONNS:-50}
PIPE=${PIPE:-16}
DURATION=${DURATION:-8s}
GATE=${GATE:-2.0}
DIST=${DIST:-uniform}
OUT=${OUT:-}                       # directory for per-cell JSON rows, empty to skip
F3SRV_BIN=${F3SRV_BIN:-f3srv}
F1SRV_BIN=${F1SRV_BIN:-f1srv}
AKI_BIN=${AKI_BIN:-aki}
REDIS_BIN=${REDIS_BIN:-redis-server}
VALKEY_BIN=${VALKEY_BIN:-valkey-server}
CPU_SERVER=${CPU_SERVER:-}
CPU_CLIENT=${CPU_CLIENT:-}

SPLIT=()
if [ -n "$CPU_SERVER" ] && [ -n "$CPU_CLIENT" ]; then
  SPLIT=(-cpu-split -cpu-server "$CPU_SERVER" -cpu-client "$CPU_CLIENT")
fi

CARDS=(${CARDS:-1 10 10k 1M})                       # doc 18 gate cardinality bands
VALUES=(${VALUES:-16 64 256 1k 4k 64k 1m})          # value ladder to 1MiB
TRANSITIONS=(${TRANSITIONS:-512 1k 2k 32k 64k 128k}) # half/at/double each threshold
CARD_CMDS=${CARD_CMDS:-"set get incr"}
VALUE_CMDS=${VALUE_CMDS:-"set get"}
TRANSITION_CMDS=${TRANSITION_CMDS:-"set get append"}

[ -n "$OUT" ] && mkdir -p "$OUT"

run_cell() { # workload card value label
  local wl=$1 c=$2 v=$3 label=$4
  local json=()
  if [ -n "$OUT" ]; then
    json=(-json "$OUT/$label-$wl-card$c-val$v-$DIST.json")
  fi
  echo "--- $label  $wl  card=$c value=$v dist=$DIST P$PIPE ---"
  "$BENCH" \
    -workload "$wl" \
    -aki-engine "$ENGINE" \
    -card "$c" \
    -value-size "$v" \
    -dist "$DIST" \
    -connections "$CONNS" \
    -pipeline "$PIPE" \
    -duration "$DURATION" \
    -gate "$GATE" \
    -aki-bin "$AKI_BIN" \
    -f1srv-bin "$F1SRV_BIN" \
    -f3srv-bin "$F3SRV_BIN" \
    -redis-bin "$REDIS_BIN" \
    -valkey-bin "$VALKEY_BIN" \
    ${json[@]+"${json[@]}"} \
    ${SPLIT[@]+"${SPLIT[@]}"} || fail=1
}

fail=0

echo "== cardinality ladder: ${CARDS[*]} at 64B (engine=$ENGINE) =="
for wl in $CARD_CMDS; do
  for c in "${CARDS[@]}"; do
    run_cell "$wl" "$c" 64 card-ladder
  done
done

echo "== value ladder: ${VALUES[*]} at the default keyspace =="
for wl in $VALUE_CMDS; do
  for v in "${VALUES[@]}"; do
    run_cell "$wl" 100k "$v" value-ladder
  done
done

echo "== band transitions: ${TRANSITIONS[*]} across the 1KiB and 64KiB thresholds =="
for wl in $TRANSITION_CMDS; do
  for v in "${TRANSITIONS[@]}"; do
    run_cell "$wl" 10k "$v" transition
  done
done

if [ "$fail" != 0 ]; then
  echo "== at least one cell missed the ${GATE}x gate; see rows above (expected until the M0 value-band slices land) =="
  exit 2
fi
echo "== every cell cleared the ${GATE}x gate =="
