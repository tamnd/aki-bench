#!/bin/bash
# Larger-than-memory string pair for the f3 engine: f3srv vs Redis vs Valkey,
# every engine holding a dataset bigger than the memory budget, with the
# eviction posture stated instead of implied.
#
# What the M0 gate LTM cell got wrong (tamnd/aki#542): the rivals ran with
# --maxmemory 512mb --maxmemory-policy allkeys-lfu, evicted roughly two thirds
# of the keyspace during the load, and then answered GETs for the evicted keys
# with a nil at RAM speed, zero disk reads for the whole window. The harness
# counted every reply as an op, so a rival serving mostly nothing posted ~4.85M
# ops/s against aki's 40k value-bearing reads from the vlog, and the row was
# recorded as 0.01x. The bytes/s column had the truth all along: redis moved
# ~352 B per "op" on a 1 KiB-value workload.
#
# This scenario keeps the same posture, capped rivals with allkeys-lfu, because
# that IS the honest larger-than-memory stance for an in-RAM engine (doc 18
# section 6.3: unbounded rivals just swap). What changes is the accounting and
# the visibility:
#
#   1. The harness now splits nil and refused replies out of the op count and
#      gates on value-bearing ops/s (vops/sec in the table, value_ops_per_sec
#      in the JSON, hit_ratio next to it).
#   2. The harness reads keyspace_hits/keyspace_misses/evicted_keys and the
#      cap/policy back from each rival's INFO around the measured window and
#      prints them under the table, so an eviction-heavy row explains itself.
#   3. The rival launch line below is explicit and printed, never buried in a
#      wrapper: anyone reading the output sees the cap and the policy the
#      rivals ran under.
#
# Remember what a "miss" means on each side: aki keeps every value (spilled to
# the vlog past the resident cap) and can return all of them; the rivals
# permanently dropped the evicted values, and outside a benchmark that is data
# loss by policy (doc 06). A rival's nil is not a fast answer to the question
# the workload asked.
#
# The old scenarios/ltm-strings/run.sh is the f1-era K7 protocol (load
# uncapped, then tighten the cgroup and let the rivals swap, probe guaranteed
# hits). It stays as the historical swap-posture protocol; this one is the f3
# eviction-posture protocol.
set -u

AKI_DIR=${AKI_DIR:-../aki}          # sibling tamnd/aki checkout to build f3srv from
BENCH=${BENCH:-}                    # prebuilt aki-bench binary; empty builds one
PORT_AKI=${PORT_AKI:-7311}
PORT_REDIS=${PORT_REDIS:-7312}
PORT_VALKEY=${PORT_VALKEY:-7313}
CAP=${CAP:-512mb}                   # rival maxmemory; must be well under N*VALSIZE
N=${N:-2000000}                     # string keys
VALSIZE=${VALSIZE:-1032}            # 1032 > the 1024 embedded-band ceiling, so every
                                    # value takes the spill path once the resident cap fills
DURATION=${DURATION:-60s}
WARM=${WARM:-10s}
CONNS=${CONNS:-64}
PIPE=${PIPE:-16}
IO_THREADS=${IO_THREADS:-4}         # rival io-threads; match the box's gate config
SHARDS=${SHARDS:-4}                 # f3srv shards
ARENA_MIB=${ARENA_MIB:-256}         # f3srv per-shard arena
RESIDENT_CAP_MIB=${RESIDENT_CAP_MIB:-128} # f3srv per-shard resident value budget
REDIS=${REDIS:-redis-server}
VALKEY=${VALKEY:-valkey-server}
OUTDIR=${OUTDIR:-}                  # where the per-row JSON goes; empty uses the work dir
# The vlog must live on a real disk, same reason as the f1 scenario: a
# tmpfs-backed vlog is RAM pretending to be disk and the LTM regime vanishes.
WORKDIR=${WORKDIR:-$HOME}

if [ ! -d "$AKI_DIR/cmd/f3srv" ]; then
  echo "f3-ltm-strings: no cmd/f3srv under AKI_DIR=$AKI_DIR; point AKI_DIR at a tamnd/aki checkout" >&2
  exit 1
fi
if [ "$(stat -f -c %T "$WORKDIR" 2>/dev/null)" = tmpfs ]; then
  echo "WORKDIR=$WORKDIR is tmpfs; the vlog would be RAM-backed and the LTM regime vanishes. Use a real disk." >&2
  exit 1
fi

WORK=$(mktemp -d "$WORKDIR/f3ltm.XXXXXX")
OUTDIR=${OUTDIR:-$WORK}
pids=()
cleanup() {
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== building f3srv from $AKI_DIR =="
(cd "$AKI_DIR" && go build -o "$WORK/f3srv" ./cmd/f3srv) || exit 1
if [ -z "$BENCH" ]; then
  echo "== building aki-bench =="
  HERE=$(cd "$(dirname "$0")/../.." && pwd)
  (cd "$HERE" && go build -o "$WORK/aki-bench" ./cmd/aki-bench) || exit 1
  BENCH="$WORK/aki-bench"
fi

wait_up() { # port
  for _ in $(seq 1 300); do
    if printf 'PING\r\n' | timeout 1 sh -c "exec 3<>/dev/tcp/127.0.0.1/$1 && cat >&3 && head -c 7 <&3" 2>/dev/null | grep -q PONG; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

raw_mb=$(awk "BEGIN{printf \"%.0f\", $N*$VALSIZE/1048576}")
echo "== f3 LTM strings: $N keys x ${VALSIZE}B (~${raw_mb}MB raw values) =="
echo "== rival posture: --maxmemory $CAP --maxmemory-policy allkeys-lfu (eviction, doc 18 s6.3);"
echo "   evicted keys are gone, their GETs answer nil, and the harness counts those apart =="
echo "== f3srv posture: $SHARDS shards, --arena-mib $ARENA_MIB --resident-cap-mib $RESIDENT_CAP_MIB per shard,"
echo "   values past the resident cap spill to the vlog on disk and stay readable =="

echo "== launching f3srv =="
mkdir -p "$WORK/vlog"
"$WORK/f3srv" --addr 127.0.0.1:$PORT_AKI --shards $SHARDS \
  --arena-mib $ARENA_MIB --resident-cap-mib $RESIDENT_CAP_MIB \
  --vlog-dir "$WORK/vlog" >"$WORK/f3srv.log" 2>&1 &
pids+=($!)

# The rival launch lines, printed in full so the posture is in the transcript.
rival_flags=(--maxmemory "$CAP" --maxmemory-policy allkeys-lfu --save "" --appendonly no --io-threads "$IO_THREADS")
echo "== launching redis: $REDIS --port $PORT_REDIS ${rival_flags[*]} =="
"$REDIS" --port $PORT_REDIS "${rival_flags[@]}" >"$WORK/redis.log" 2>&1 &
pids+=($!)
echo "== launching valkey: $VALKEY --port $PORT_VALKEY ${rival_flags[*]} =="
"$VALKEY" --port $PORT_VALKEY "${rival_flags[@]}" >"$WORK/valkey.log" 2>&1 &
pids+=($!)

for port in $PORT_AKI $PORT_REDIS $PORT_VALKEY; do
  wait_up $port || { echo "f3-ltm-strings: server on port $port did not come up" >&2; exit 1; }
done

# Connect mode: aki-bench flushes, preloads the full N-key space (this is where
# the rivals evict), warms, measures, and prints per-target vops/sec, hit%, and
# the server window line (evicted/hits/misses read back from INFO). GET first,
# then SET; the SET row on a full store exercises aki's spill write path against
# rivals evicting to make room.
fail=0
for wl in get set; do
  echo "--- $wl (f3 LTM, eviction posture) ---"
  "$BENCH" \
    -workload "$wl" \
    -aki-addr 127.0.0.1:$PORT_AKI \
    -redis-addr 127.0.0.1:$PORT_REDIS \
    -valkey-addr 127.0.0.1:$PORT_VALKEY \
    -keys "$N" \
    -value-size "$VALSIZE" \
    -connections "$CONNS" \
    -pipeline "$PIPE" \
    -warm "$WARM" \
    -duration "$DURATION" \
    -json "$OUTDIR/f3-ltm-$wl.json"
  rc=$?
  # Exit 2 is the gate saying aki did not clear 2x, which is a result, not a
  # harness failure. Anything else is a real failure.
  if [ "$rc" != 0 ] && [ "$rc" != 2 ]; then
    echo "f3-ltm-strings: workload $wl failed with exit $rc" >&2
    fail=1
  fi
done

echo "== row JSON in $OUTDIR/f3-ltm-{get,set}.json; check hit_ratio and the server window lines =="
[ "$fail" = 0 ] || exit 1
