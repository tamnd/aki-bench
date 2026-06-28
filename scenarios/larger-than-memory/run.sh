#!/bin/bash
# Larger-than-memory contest: aki vs Redis vs Valkey under a hard RAM cap with a
# dataset several times larger than the cap. This is the workload aki's single-file
# design is built for, and it is a different test from the in-memory saturation the
# Go harness runs: here the bottleneck is the storage path under memory pressure,
# not the loopback socket.
#
# Linux only, needs root for cgroup scopes and drop_caches. Each engine runs inside
# its own `systemd-run --scope` with MemoryMax and MemorySwapMax, so the host and
# anything else on the box are never touched, and the overflow goes to swap exactly
# as a small-RAM box would force.
#
# The methodology has one asymmetry, and it is deliberate, because it reflects what
# each engine is. aki persists the whole dataset to one file, so it is loaded
# UNCAPPED, the page cache is dropped, and it serves read-only UNDER the cap: every
# file page it then touches is clean and reclaimable, so the cap throttles its
# resident set with no OOM. Redis and Valkey live in the heap, so they are loaded
# and served INSIDE the cap and their overflow swaps. Capping aki during its own
# write-load instead would fill the cap with dirty pages and the kernel would
# OOM-kill it; that is a property of file-backed writes under cgroup accounting, not
# a fair write comparison, so the load numbers below are reported but not treated as
# a like-for-like write contest (aki's is uncapped, the others' are capped).
#
# Override any parameter from the environment, for example:
#   CAP=192M KEYS=3000000 VAL=128 ./run.sh
set -u
CAP=${CAP:-256M}          # RAM ceiling per engine
SWAP=${SWAP:-2048M}       # swap ceiling per engine
KEYS=${KEYS:-800000}      # key space size
VAL=${VAL:-1024}          # value size in bytes
GETN=${GETN:-200000}      # number of random GETs in the read measurement
BP=${BP:-128mb}           # aki buffer pool size
AKI=${AKI:-aki}           # aki binary
PORT=${PORT:-7020}

if [ "$(id -u)" != 0 ]; then echo "needs root for cgroup scopes and drop_caches" >&2; exit 1; fi

reset_scope() { systemctl stop ltm.scope 2>/dev/null; systemctl reset-failed ltm.scope 2>/dev/null; sleep 1; }
drop_caches() { sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null; }
wait_up() { local i; for i in $(seq 1 300); do redis-cli -p $PORT ping >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }
rss_mb() { local pid; pid=$(redis-cli -p $PORT info server 2>/dev/null | tr -d '\r' | awk -F: '/process_id/{print $2}'); [ -n "$pid" ] && awk '/VmRSS/{printf "%d",$2/1024}' /proc/$pid/status 2>/dev/null; }
getrps() { redis-benchmark -p $PORT -t get -r $KEYS -n $GETN -d $VAL -c 50 -P 16 -q 2>/dev/null | tr '\r' '\n' | awk '/requests per second/{v=$2} END{print v}'; }

measure_aki() {
  reset_scope
  local D; D=$(mktemp -d ${TMPDIR:-/tmp}/ltm.XXXXXX)
  # Phase 1: load uncapped, persisted to the .aki file.
  $AKI server --port $PORT --admin-port 0 --buffer-pool-size $BP --dir $D >$D/load.log 2>&1 &
  local lpid=$!
  wait_up || { echo "aki: load start FAILED"; tail -3 $D/load.log; kill $lpid 2>/dev/null; rm -rf $D; return; }
  local t0=$SECONDS
  redis-benchmark -p $PORT -t set -r $KEYS -n $KEYS -d $VAL -c 50 -P 16 -q >/dev/null 2>&1
  redis-cli -p $PORT save >/dev/null 2>&1
  local load=$((SECONDS-t0))
  redis-cli -p $PORT shutdown nosave >/dev/null 2>&1; sleep 1; kill $lpid 2>/dev/null; wait $lpid 2>/dev/null
  drop_caches
  # Phase 2: serve read-only under the RAM cap, clean reclaimable file pages.
  systemd-run --quiet --unit=ltm --scope -p MemoryMax=$CAP -p MemorySwapMax=$SWAP \
    $AKI server --port $PORT --admin-port 0 --buffer-pool-size $BP --dir $D >$D/serve.log 2>&1 &
  wait_up || { echo "aki: serve start FAILED"; tail -3 $D/serve.log; reset_scope; rm -rf $D; return; }
  local rss; rss=$(rss_mb); local g; g=$(getrps)
  echo "aki:    get_rps=$g  rss_MB=$rss  load_s=$load(uncapped)"
  reset_scope; rm -rf $D
}

measure_mem() { # redis or valkey: load and serve inside the cap, overflow swaps
  local name=$1 bin=$2
  reset_scope; drop_caches
  local D; D=$(mktemp -d ${TMPDIR:-/tmp}/ltm.XXXXXX)
  systemd-run --quiet --unit=ltm --scope -p MemoryMax=$CAP -p MemorySwapMax=$SWAP \
    $bin --port $PORT --dir $D --save "" --appendonly no >$D/run.log 2>&1 &
  wait_up || { echo "$name: start FAILED"; tail -3 $D/run.log; reset_scope; rm -rf $D; return; }
  local t0=$SECONDS
  redis-benchmark -p $PORT -t set -r $KEYS -n $KEYS -d $VAL -c 50 -P 16 -q >/dev/null 2>&1
  if ! redis-cli -p $PORT ping >/dev/null 2>&1; then echo "$name: DIED during load (OOM?)"; tail -3 $D/run.log; reset_scope; rm -rf $D; return; fi
  local load=$((SECONDS-t0))
  local rss; rss=$(rss_mb); local g; g=$(getrps)
  echo "$name:  get_rps=$g  rss_MB=$rss  load_s=$load(capped)"
  reset_scope; rm -rf $D
}

ds=$(awk "BEGIN{printf \"%.0f\", $KEYS*$VAL/1048576}")
echo "== larger-than-memory: RAM cap=$CAP swap=$SWAP raw~${ds}MB (${KEYS}x${VAL}B), $(nproc) cores, load $(cut -d' ' -f1 /proc/loadavg) =="
measure_aki
measure_mem redis  redis-server
measure_mem valkey valkey-server
echo "== done, load $(cut -d' ' -f1 /proc/loadavg) =="
