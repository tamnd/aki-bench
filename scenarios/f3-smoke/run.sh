#!/bin/bash
# f3srv launch-target smoke: build f3srv from a sibling tamnd/aki checkout and
# prove the harness can launch it, talk to it, and tear it down.
#
# This is the M0 P13 harness slice 1 check (spec 2064/f3/milestones/M0.md), and it
# is non-evidential by design: no number it prints is a gate number, the windows
# are seconds long, and no rival needs to be present. What it proves is plumbing:
# aki-bench's -aki-engine f3 path builds against the real cmd/f3srv flag surface,
# the launched server answers the string smoke with correct replies, and a short
# load run completes against it. CI runs this on every PR so a drift in f3srv's
# launch line or its M0 string surface shows up here, not in a gate session.
#
# The smoke stays inside the surface f3srv serves in M0: SET, GET, the INCR
# family, APPEND, SETRANGE/GETRANGE, DEL, EXISTS, STRLEN, TYPE, PING, ECHO.
# MGET joins once the multi-key fan-out slice lands.
#
# The load rows tolerate a failed gate (exit 2): with no Redis or Valkey present
# the gate cannot pass and is not supposed to. Any other exit is a real failure.
set -u

AKI_DIR=${AKI_DIR:-../aki}        # sibling tamnd/aki checkout to build f3srv from
BENCH=${BENCH:-}                  # prebuilt aki-bench binary; empty builds one
DURATION=${DURATION:-2s}
CONNS=${CONNS:-8}
PIPE=${PIPE:-4}

if [ ! -d "$AKI_DIR/cmd/f3srv" ]; then
  echo "f3-smoke: no cmd/f3srv under AKI_DIR=$AKI_DIR; point AKI_DIR at a tamnd/aki checkout" >&2
  exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "== building f3srv from $AKI_DIR =="
(cd "$AKI_DIR" && go build -o "$WORK/f3srv" ./cmd/f3srv) || exit 1

if [ -z "$BENCH" ]; then
  echo "== building aki-bench =="
  HERE=$(cd "$(dirname "$0")/../.." && pwd)
  (cd "$HERE" && go build -o "$WORK/aki-bench" ./cmd/aki-bench) || exit 1
  BENCH="$WORK/aki-bench"
fi

echo "== string smoke against a launched f3srv =="
"$BENCH" -smoke -aki-engine f3 -f3srv-bin "$WORK/f3srv" || exit 1

# A short load mix inside the M0 string surface, proving launch, preload, the
# measured window, and teardown end to end. Exit 2 (gate not met) is expected
# when no rival is installed; anything else is a harness or server failure.
fail=0
for wl in set get incr append; do
  echo "--- $wl  (f3, non-evidential smoke load) ---"
  "$BENCH" \
    -workload "$wl" \
    -aki-engine f3 \
    -f3srv-bin "$WORK/f3srv" \
    -connections "$CONNS" \
    -pipeline "$PIPE" \
    -duration "$DURATION"
  rc=$?
  if [ "$rc" != 0 ] && [ "$rc" != 2 ]; then
    echo "f3-smoke: workload $wl failed with exit $rc" >&2
    fail=1
  fi
done

if [ "$fail" != 0 ]; then
  exit 1
fi
echo "== f3srv launch target ok =="
