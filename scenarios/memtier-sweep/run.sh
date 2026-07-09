#!/bin/bash
# Single-box memtier saturation sweep: f2srv vs Redis 8.8 vs Valkey 9.1.
#
# This is the canonical in-memory-fit 2x gate for one machine. It drives every
# target with memtier_benchmark, the same C-threaded load generator the rest of
# the world benches Redis with, so the number is comparable to published Redis
# and Valkey figures rather than to our own Go client. memtier is a strict
# superset of redis-benchmark for this job: it does mixed set:get ratios, key
# distributions, arbitrary value sizes, and pipeline depth in one tool, and it
# prints a single Totals ops/sec that this script parses.
#
# Pinning is the whole game on a co-located box. The server takes the low cores,
# the load generator takes the rest, and the two never share a core. The client
# gets the larger share on purpose: memtier's threads have to saturate the
# server, and a starved client caps all three targets at the client ceiling,
# which reads a real win as a tie. Set SRV_CORES and CLI_CORES to disjoint lists
# for your box.
#
# Each cell brings its server fully down and waits for the port to free before
# the next one starts. A stale server bleeding into the next measurement is a
# real failure mode: a prior run of this sweep left a dying server on two of its
# tail cells and memtier timed a mix of the old and new server, printing an
# impossible number. One clean server per cell removes that.
set -u

BIN=${BIN:-/root/bin}                 # dir holding redis-server, valkey-server, f2srv
MT=${MT:-memtier_benchmark}           # memtier_benchmark on PATH or an absolute path
PORT=${PORT:-6399}
DIR=${DIR:-/root/mtbenchdir}          # scratch data dir, wiped per cell
SRV_CORES=${SRV_CORES:-0-7}           # cores the server is pinned to
CLI_CORES=${CLI_CORES:-8-31}          # cores the load generator is pinned to
MT_TIME=${MT_TIME:-10}                # seconds per measured cell
MT_CONN=${MT_CONN:---threads 8 --clients 25}  # 200 connections by default
KEYMAX=${KEYMAX:-1000000}             # key id space
GOGC=${GOGC:-400}                     # f2srv GOGC (memory for fewer GC cycles)
PIDF=/tmp/mtsweep.pid

# Targets to run. Override to a subset, e.g. TARGETS="f2srv" for a quick check.
TARGETS=${TARGETS:-redis valkey f2srv}

stop() { [ -f "$PIDF" ] && kill "$(cat $PIDF)" 2>/dev/null; fuser -k $PORT/tcp 2>/dev/null; sleep 1; rm -f "$PIDF"; }
start() {
  stop; rm -rf "$DIR"; mkdir -p "$DIR"
  case "$1" in
    redis)  taskset -c $SRV_CORES $BIN/redis-server  --bind 0.0.0.0 --port $PORT --dir $DIR --save '' --appendonly no --protected-mode no >/tmp/mtsweep.srv.log 2>&1 & echo $! >$PIDF;;
    valkey) taskset -c $SRV_CORES $BIN/valkey-server --bind 0.0.0.0 --port $PORT --dir $DIR --save '' --appendonly no --protected-mode no >/tmp/mtsweep.srv.log 2>&1 & echo $! >$PIDF;;
    f2srv)  taskset -c $SRV_CORES $BIN/f2srv -addr 0.0.0.0:$PORT -net go -gogc $GOGC >/tmp/mtsweep.srv.log 2>&1 & echo $! >$PIDF;;
    *) echo "unknown target $1" >&2; return 1;;
  esac
  sleep 2
}

# Preload the keyspace so a get-only cell measures value fetches, not misses.
# $1 = data size in bytes.
preload() {
  taskset -c $CLI_CORES $MT -s 127.0.0.1 -p $PORT $MT_CONN --ratio 1:0 --data-size "$1" \
    --key-maximum $KEYMAX --key-pattern P:P --requests $((KEYMAX/200)) --pipeline 16 \
    --hide-histogram >/dev/null 2>&1
}

# One measured cell: $1=ratio (set:get) $2=data size $3=pipeline. Prints Totals ops/sec.
mt() {
  taskset -c $CLI_CORES $MT -s 127.0.0.1 -p $PORT $MT_CONN --ratio "$1" --data-size "$2" \
    --pipeline "$3" --key-maximum $KEYMAX --key-pattern R:R --test-time $MT_TIME \
    --hide-histogram --distinct-client-seed 2>/dev/null | awk '/Totals/{printf "%.0f",$2;exit}'
}

# The workload matrix: label | ratio | data-size | pipeline. A 0:1 (get-only)
# row is preloaded at its data size first so the reads hit stored values.
WL=(
  "set-only-p16   1:0    64   16"
  "set-only-p1    1:0    64    1"
  "get-only-p16   0:1    64   16"
  "get-only-p1    0:1    64    1"
  "mixed-1:1-p16  1:1    64   16"
  "mixed-1:10-p16 1:10   64   16"
  "mixed-1:1-p1   1:1    64    1"
  "get-512b-p16   0:1   512   16"
  "set-512b-p16   1:0   512   16"
  "get-4k-p16     0:1  4096   16"
  "set-4k-p16     1:0  4096   16"
)

echo "=== single-box memtier sweep: srv=$SRV_CORES cli=$CLI_CORES conns=($MT_CONN) t=${MT_TIME}s ==="
printf '%-15s' workload; for t in $TARGETS; do printf ' %14s' "$t"; done; printf ' %10s\n' 'gate'
for row in "${WL[@]}"; do
  set -- $row; label=$1; ratio=$2; ds=$3; pipe=$4
  printf '%-15s' "$label"
  declare -A r=()
  for tgt in $TARGETS; do
    start $tgt || { r[$tgt]=0; printf ' %14s' ERR; continue; }
    [ "$ratio" = "0:1" ] && preload $ds
    v=$(mt $ratio $ds $pipe); [ -z "$v" ] && v=0
    r[$tgt]=$v; printf ' %14s' "$v"; stop
  done
  # Gate: f2srv over the faster rival, if all three ran.
  if [ -n "${r[f2srv]:-}" ] && [ -n "${r[redis]:-}" ] && [ -n "${r[valkey]:-}" ]; then
    printf ' %10s' "$(awk -v a="${r[f2srv]}" -v x="${r[redis]}" -v y="${r[valkey]}" \
      'BEGIN{m=(x>y?x:y); if(m>0)printf "%.2fx",a/m; else print "na"}')"
  fi
  printf '\n'; unset r
done
echo "=== done ==="
