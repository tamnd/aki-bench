#!/bin/bash
# Saturation pipeline-depth sweep: does aki's lead grow as the socket syscall
# amortizes across a deeper pipeline? At P16 the box is ~69% syscall (note 299).
# Each engine runs in its own clean --dir so no stray dump.rdb breaks startup.
#
# The load generator is memtier_benchmark: it drives a flat random keyspace with
# a fixed set:get ratio and reports one Totals ops/sec, which is the number this
# sweep tracks across pipeline depths. memtier is pinned to the client cores so
# its threads never share a core with the server.
set -u
AKI=${AKI:-$HOME/akiperf/aki-uring}
MT=${MT:-memtier_benchmark}
PORT=7030
NKEYS=100000
CLIENTS=50
THREADS=2                 # memtier client threads, pinned to CLI_CORES
CLI_CORES=4-5             # load generator cores, disjoint from the server's 0-3
TIME=12                   # seconds per measured depth
VALSIZE=16
DEPTHS="16 64 256 512"
TMP=$(mktemp -d)

stop() { redis-cli -p $PORT shutdown nosave >/dev/null 2>&1; sleep 1; }
wait_up() { local i; for i in $(seq 1 100); do if redis-cli -p $PORT ping >/dev/null 2>&1; then return 0; fi; sleep 0.1; done; return 1; }

# One measured run: $1=ratio (set:get) $2=pipeline. Prints Totals ops/sec.
bench() {
  taskset -c $CLI_CORES $MT -s 127.0.0.1 -p $PORT --threads $THREADS --clients $CLIENTS \
    --ratio "$1" --data-size $VALSIZE --pipeline "$2" --key-maximum $NKEYS --key-pattern R:R \
    --test-time $TIME --hide-histogram --distinct-client-seed 2>/dev/null \
    | awk '/Totals/{printf "%.0f",$2;exit}'
}
# Preload the keyspace so a get-only run measures value fetches, not misses.
preload() {
  taskset -c $CLI_CORES $MT -s 127.0.0.1 -p $PORT --threads $THREADS --clients $CLIENTS \
    --ratio 1:0 --data-size $VALSIZE --key-maximum $NKEYS --key-pattern P:P \
    --requests $((NKEYS/THREADS/CLIENTS+1)) --pipeline 32 --hide-histogram >/dev/null 2>&1
}

start_aki()   { rm -rf $TMP/d; mkdir -p $TMP/d; GOMAXPROCS=4 taskset -c 0-3 $AKI server --port $PORT --admin-port 0 --aki-engine hot --aki-net goroutine --dir $TMP/d >$TMP/aki.log 2>&1 & wait_up; }
start_redis() { rm -rf $TMP/d; mkdir -p $TMP/d; taskset -c 0-3 redis-server --port $PORT --save '' --appendonly no --dir $TMP/d --io-threads 4 --io-threads-do-reads yes >$TMP/r.log 2>&1 & wait_up; }
start_valkey(){ rm -rf $TMP/d; mkdir -p $TMP/d; taskset -c 0-3 valkey-server --port $PORT --save '' --appendonly no --dir $TMP/d --io-threads 4 >$TMP/v.log 2>&1 & wait_up; }

echo "== pipeline-depth saturation sweep: GET & SET, $(nproc) cores, server 0-3, client $CLI_CORES, load $(cut -d' ' -f1 /proc/loadavg) =="
echo "rivals: redis/valkey io-threads=4 (their best); aki GOMAXPROCS=4 hot engine goroutine net; memtier ${THREADS}x${CLIENTS} conns, ${TIME}s/depth"
for op in GET SET; do
  echo "--- $op ---"
  printf "%-7s %-12s %-12s %-12s %-10s %-10s\n" depth aki redis valkey aki/redis aki/valkey
  [ "$op" = GET ] && RATIO="0:1" || RATIO="1:0"
  for d in $DEPTHS; do
    stop; start_redis  >/dev/null 2>&1 || { echo "redis start fail"; continue; }; [ "$op" = GET ] && preload; R=$(bench $RATIO $d); stop
    start_valkey >/dev/null 2>&1 || { echo "valkey start fail"; continue; }; [ "$op" = GET ] && preload; V=$(bench $RATIO $d); stop
    start_aki    >/dev/null 2>&1 || { echo "aki start fail"; continue; }; [ "$op" = GET ] && preload; A=$(bench $RATIO $d); stop
    ar=$(awk "BEGIN{if($R>0)printf \"%.2f\",$A/$R; else print \"na\"}")
    av=$(awk "BEGIN{if($V>0)printf \"%.2f\",$A/$V; else print \"na\"}")
    printf "%-7s %-12s %-12s %-12s %-10s %-10s\n" "P$d" "$A" "$R" "$V" "$ar" "$av"
  done
done
rm -rf $TMP
echo "== done, load $(cut -d' ' -f1 /proc/loadavg) =="
