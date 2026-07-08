#!/bin/bash
# Saturation pipeline-depth sweep: does aki's lead grow as the socket syscall
# amortizes across a deeper pipeline? At P16 the box is ~69% syscall (note 299).
# Each engine runs in its own clean --dir so no stray dump.rdb breaks startup.
set -u
AKI=${AKI:-$HOME/akiperf/aki-uring}
PORT=7030
NKEYS=100000
N=3000000
CLIENTS=50
DEPTHS="16 64 256 512"
TMP=$(mktemp -d)

stop() { redis-cli -p $PORT shutdown nosave >/dev/null 2>&1; sleep 1; }
wait_up() { local i; for i in $(seq 1 100); do if redis-cli -p $PORT ping >/dev/null 2>&1; then return 0; fi; sleep 0.1; done; return 1; }
parse() { tr '\r' '\n' | awk '{for(i=1;i<=NF;i++)if($i=="requests")v=$(i-1)} END{print v+0}'; }

bench() { # $1=depth  rest=command -> summed rps across 2 pinned clients
  local d=$1; shift
  taskset -c 4 redis-benchmark -p $PORT -n $N -r $NKEYS -c $CLIENTS -P $d -q "$@" 2>/dev/null | parse > $TMP/a &
  local pa=$!
  taskset -c 5 redis-benchmark -p $PORT -n $N -r $NKEYS -c $CLIENTS -P $d -q "$@" 2>/dev/null | parse > $TMP/b
  wait $pa
  awk '{s+=$1} END{print s}' $TMP/a $TMP/b
}

start_aki()   { rm -rf $TMP/d; mkdir -p $TMP/d; GOMAXPROCS=4 taskset -c 0-3 $AKI server --port $PORT --admin-port 0 --aki-engine hot --aki-net goroutine --dir $TMP/d >$TMP/aki.log 2>&1 & wait_up; }
start_redis() { rm -rf $TMP/d; mkdir -p $TMP/d; taskset -c 0-3 redis-server --port $PORT --save '' --appendonly no --dir $TMP/d --io-threads 4 --io-threads-do-reads yes >$TMP/r.log 2>&1 & wait_up; }
start_valkey(){ rm -rf $TMP/d; mkdir -p $TMP/d; taskset -c 0-3 valkey-server --port $PORT --save '' --appendonly no --dir $TMP/d --io-threads 4 >$TMP/v.log 2>&1 & wait_up; }
preload() { redis-benchmark -p $PORT -n 200000 -r $NKEYS -c 50 -P 32 -q SET key:__rand_int__ vvvvvvvvvvvvvvvv >/dev/null 2>&1; }

echo "== pipeline-depth saturation sweep: GET & SET, $(nproc) cores, server 0-3, clients 4&5, load $(cut -d' ' -f1 /proc/loadavg) =="
echo "rivals: redis/valkey io-threads=4 (their best); aki GOMAXPROCS=4 hot engine goroutine net; N=$N per client"
for op in GET SET; do
  echo "--- $op ---"
  printf "%-7s %-12s %-12s %-12s %-10s %-10s\n" depth aki redis valkey aki/redis aki/valkey
  for d in $DEPTHS; do
    [ "$op" = GET ] && CMD="GET key:__rand_int__" || CMD="SET key:__rand_int__ vvvvvvvvvvvvvvvv"
    stop; start_redis  >/dev/null 2>&1 || { echo "redis start fail"; continue; }; preload; R=$(bench $d $CMD); stop
    start_valkey >/dev/null 2>&1 || { echo "valkey start fail"; continue; }; preload; V=$(bench $d $CMD); stop
    start_aki    >/dev/null 2>&1 || { echo "aki start fail"; continue; }; preload; A=$(bench $d $CMD); stop
    ar=$(awk "BEGIN{if($R>0)printf \"%.2f\",$A/$R; else print \"na\"}")
    av=$(awk "BEGIN{if($V>0)printf \"%.2f\",$A/$V; else print \"na\"}")
    printf "%-7s %-12s %-12s %-12s %-10s %-10s\n" "P$d" "$A" "$R" "$V" "$ar" "$av"
  done
done
rm -rf $TMP
echo "== done, load $(cut -d' ' -f1 /proc/loadavg) =="
