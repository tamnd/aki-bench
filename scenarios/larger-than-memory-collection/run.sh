#!/bin/bash
# Larger-than-memory collection contest: aki vs Redis vs Valkey.
#
# The string LTM tests (notes 303-305) measured many small keys. This one measures
# the regime that is aki's purest structural claim: a SINGLE collection larger than
# RAM with random element access. Redis stores a hash as one heap object (a dict of
# sds entries scattered across the heap); under a hard RAM cap the dict and its
# entries swap, so each HGET faults in several scattered pages. aki stores the hash
# element-per-row in a btree sub-tree, so HGET is an O(log F) descent of clean,
# reclaimable, page-local file reads. The question: does that structural difference
# produce a throughput win, and how big, under the same cap.
#
# Methodology mirrors run.sh: aki loads uncapped to its single file, drops the page
# cache, then serves read-only UNDER the cap (clean reclaimable pages, no OOM). Redis
# and Valkey load and serve INSIDE the cap and overflow to swap, their real LTM
# behavior. One giant hash h:0 with F fields of V bytes each. Fields are named
# field:%012d so redis-benchmark's __rand_int__ (12-digit) matches a stored field.
set -u
CAP=${CAP:-300M}
SWAP=${SWAP:-4096M}
F=${F:-3000000}        # number of fields in the one big hash
V=${V:-256}            # field value size in bytes
GETN=${GETN:-100000}   # number of random HGETs in the read measurement
BP=${BP:-128mb}
AKI=${AKI:-$HOME/akiperf/aki-uring}
PORT=${PORT:-7026}

if [ "$(id -u)" != 0 ]; then echo "needs root for cgroup scopes and drop_caches" >&2; exit 1; fi
VAL=$(perl -e "print 'x' x $V")

reset_scope() { systemctl stop ltm.scope 2>/dev/null; systemctl reset-failed ltm.scope 2>/dev/null; sleep 1; }
drop_caches() { sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null; }
wait_up() { local i; for i in $(seq 1 600); do redis-cli -p $PORT ping >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }
rss_mb() { local pid; pid=$(redis-cli -p $PORT info server 2>/dev/null | tr -d '\r' | awk -F: '/process_id/{print $2}'); [ -n "$pid" ] && awk '/VmRSS/{printf "%d",$2/1024}' /proc/$pid/status 2>/dev/null; }
# Build the one big hash by streaming inline HSET commands through redis-cli --pipe.
load_hash() { perl -e "for(0..$F-1){printf \"HSET h:0 field:%012d $VAL\n\",\$_}" | redis-cli -p $PORT --pipe >/dev/null 2>&1; }
# redis-benchmark needs every flag BEFORE the command, else -c/-P land as HGET args.
# The summary line is "HGET h:0 field:__rand_int__: <rate> requests per second", so
# pull the token right before "requests" rather than a fixed column.
hgetrps() { redis-benchmark -p $PORT -r $F -n $GETN -c 50 -P 16 -q HGET h:0 field:__rand_int__ 2>/dev/null | tr '\r' '\n' | awk '{for(i=1;i<=NF;i++)if($i=="requests")v=$(i-1)} END{print v}'; }

measure_aki() {
  reset_scope
  local D; D=$(mktemp -d ${TMPDIR:-/tmp}/ltm.XXXXXX)
  $AKI server --port $PORT --admin-port 0 --buffer-pool-size $BP --dir $D >$D/load.log 2>&1 &
  local lpid=$!
  wait_up || { echo "aki: load start FAILED"; tail -3 $D/load.log; kill $lpid 2>/dev/null; rm -rf $D; return; }
  local t0=$SECONDS
  load_hash
  redis-cli -p $PORT save >/dev/null 2>&1
  local load=$((SECONDS-t0))
  local hlen; hlen=$(redis-cli -p $PORT hlen h:0 2>/dev/null | tr -d '\r')
  redis-cli -p $PORT shutdown nosave >/dev/null 2>&1; sleep 1; kill $lpid 2>/dev/null; wait $lpid 2>/dev/null
  local dsize; dsize=$(du -sm $D 2>/dev/null | cut -f1)
  drop_caches
  systemd-run --quiet --unit=ltm --scope -p MemoryMax=$CAP -p MemorySwapMax=$SWAP \
    $AKI server --port $PORT --admin-port 0 --buffer-pool-size $BP --dir $D >$D/serve.log 2>&1 &
  wait_up || { echo "aki: serve start FAILED"; tail -3 $D/serve.log; reset_scope; rm -rf $D; return; }
  local rss; rss=$(rss_mb); local g; g=$(hgetrps)
  echo "aki:    hget_rps=$g  rss_MB=$rss  hlen=$hlen  file_MB=$dsize  load_s=$load(uncapped)"
  reset_scope; rm -rf $D
}

measure_mem() {
  local name=$1 bin=$2
  reset_scope; drop_caches
  local D; D=$(mktemp -d ${TMPDIR:-/tmp}/ltm.XXXXXX)
  systemd-run --quiet --unit=ltm --scope -p MemoryMax=$CAP -p MemorySwapMax=$SWAP \
    $bin --port $PORT --dir $D --save "" --appendonly no >$D/run.log 2>&1 &
  wait_up || { echo "$name: start FAILED"; tail -3 $D/run.log; reset_scope; rm -rf $D; return; }
  local t0=$SECONDS
  load_hash
  if ! redis-cli -p $PORT ping >/dev/null 2>&1; then echo "$name: DIED during load (OOM?)"; tail -3 $D/run.log; reset_scope; rm -rf $D; return; fi
  local load=$((SECONDS-t0))
  local hlen; hlen=$(redis-cli -p $PORT hlen h:0 2>/dev/null | tr -d '\r')
  local rss; rss=$(rss_mb); local g; g=$(hgetrps)
  echo "$name:  hget_rps=$g  rss_MB=$rss  hlen=$hlen  load_s=$load(capped)"
  reset_scope; rm -rf $D
}

ds=$(awk "BEGIN{printf \"%.0f\", $F*$V/1048576}")
echo "== LTM collection: one hash, $F fields x ${V}B raw~${ds}MB, RAM cap=$CAP swap=$SWAP, $(nproc) cores, load $(cut -d' ' -f1 /proc/loadavg) =="
measure_aki
measure_mem redis  redis-server
measure_mem valkey valkey-server
echo "== done, load $(cut -d' ' -f1 /proc/loadavg) =="
