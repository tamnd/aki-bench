#!/bin/bash
# Fair larger-than-memory string matrix: f1srv vs Redis 8.8 vs Valkey 9.1.
#
# This is the string sibling of ltm-collections/run.sh. Where that scenario loads one
# collection larger than the RAM cap and reads random elements, this one loads many
# string keys whose values together exceed the cap and reads random values. It is the
# regime f1raw's milestone-M1 cold value tier is built for (spec 2064/f1_rewrite_ltm,
# WiscKey key-value separation): f1srv keeps its lock-free index and the record keys
# resident in an anonymous arena and writes any value past the separation threshold to
# an append-only cold log on disk, so under a cap smaller than the value bytes its
# resident footprint stays near index-plus-keys while Redis and Valkey hold every value
# in the heap and fault the swapped overflow on each read.
#
# The fairness rule is the same as the collection scenario (spec 2064/ltm/05 section 1):
# every engine LOADS with full RAM, THEN the cap is tightened below the value bytes and
# the caches are dropped before reads are served. That isolates the serve-time LTM
# effect and removes the load-time swap-thrash artifact. f1srv does not reload from a
# file (the durable single-file format is milestone M2); it stays up under the tightened
# cap and serves values from the cold log through the OS page cache, which the cap
# bounds. Redis and Valkey overflow their heap to swap under the same cap.
#
# Keys are 12-digit zero-padded ids, which is exactly what redis-benchmark's
# __rand_int__ substitutes, so every GET probe hits a stored key and measures the value
# fetch rather than a miss. Values are VALSIZE bytes (default 1024), above the default
# 512-byte separation threshold, so every value is separated on f1srv. N keys x VALSIZE
# is the raw value volume; pick CAP well under it so the rivals must swap.
#
# This run is uniform-random. Skew (zipfian) is where a read-cache tier would separate
# further; it is a later milestone and is logged as a gap here, not silently dropped.
set -u

CAP=${CAP:-256M}        # RAM cap the read phase serves under
SWAP=${SWAP:-4096M}     # swap room the rivals fault into
N=${N:-1000000}         # string keys (N x VALSIZE is the raw value volume)
VALSIZE=${VALSIZE:-1024} # value bytes; must exceed the f1srv separation threshold
PORT=${PORT:-7048}
GETN=${GETN:-100000}    # random probes per measurement
CLIENTS=${CLIENTS:-50}
PIPE=${PIPE:-16}
REPS=${REPS:-2}
SEP=${SEP:-512}         # f1srv separation threshold in bytes
IBUCKETS=${IBUCKETS:-1048576} # f1srv index buckets (~7M slots at 1<<20)
ARENA=${ARENA:-268435456}     # f1srv arena bytes (index+keys+pointers only; values go cold)
F1=${F1:-$HOME/akiperf/f1srv}
REDIS=${REDIS:-redis-server}
VALKEY=${VALKEY:-$HOME/akiperf/valkey-9.1.0/src/valkey-server}
# The f1srv cold value log MUST live on a real disk. If the work dir is tmpfs (as /tmp
# often is), the "on-disk" cold log is RAM-backed: drop_caches cannot reclaim it, the
# resident footprint never drops below the value bytes, and the LTM effect being measured
# vanishes (f1srv then holds every value in RAM like the rivals and OOMs under the cap).
# Default to $HOME, which is on a real filesystem, and refuse to run from tmpfs.
WORKDIR=${WORKDIR:-$HOME}

if [ "$(id -u)" != 0 ]; then echo "needs root for cgroup scopes and drop_caches" >&2; exit 1; fi
if [ "$(stat -f -c %T "$WORKDIR" 2>/dev/null)" = tmpfs ]; then
  echo "WORKDIR=$WORKDIR is tmpfs (RAM-backed); the cold log would defeat the LTM test. Set WORKDIR to a real-disk path." >&2
  exit 1
fi
VAL=$(perl -e "print 'v' x $VALSIZE")

reset_scope() { systemctl stop ltms.scope 2>/dev/null; systemctl reset-failed ltms.scope 2>/dev/null; sleep 1; }
drop_caches() { sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null; }
wait_up() { local i; for i in $(seq 1 600); do redis-cli -p $PORT ping >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }
# Refuse to start a new engine until the previous one has released the port. A back-to-back
# rep that races an old server still bound to $PORT sends its load to two servers at once,
# which double-writes the cold log and halves the measured rate. Block until the port is
# free so each rep measures exactly one clean engine.
wait_port_free() { local i; for i in $(seq 1 300); do redis-cli -p $PORT ping >/dev/null 2>&1 || return 0; sleep 0.2; done; return 1; }
swap_mb() { free -m | awk '/Swap/{print $3}'; }
rss_of() { local pid=$1; [ -n "$pid" ] && awk '/VmRSS/{printf "%d",$2/1024}' /proc/$pid/status 2>/dev/null; }
# Pull the rate token right before "requests" in redis-benchmark -q output, so the
# probe command's own arguments never shift a fixed column.
rate() { tr '\r' '\n' | awk '{for(i=1;i<=NF;i++)if($i=="requests")v=$(i-1)} END{print v+0}'; }

load_pipe() { perl -e "$1" | redis-cli -p $PORT --pipe >/dev/null 2>&1; }
probe() { redis-benchmark -p $PORT -r $N -n $GETN -c $CLIENTS -P $PIPE -q $1 2>/dev/null | rate; }

# The row table. Each row is: name | load-perl | probe redis-benchmark args.
# load-perl prints inline commands to stream through redis-cli --pipe. $N and $VALSIZE
# expand at array-build time; the value bytes are built inside perl with ("v" x VALSIZE)
# so the giant string never rides in the command and $VAL stays out of perl scope. GET
# is the primary LTM read; SET measures the write path where f1srv appends to the cold
# log while the rivals rewrite heap under the cap.
ROWS=(
  "get|for(0..$N-1){printf \"SET %012d %s\n\",\$_,('v' x $VALSIZE)}|GET __rand_int__"
  "set|for(0..$N-1){printf \"SET %012d %s\n\",\$_,('v' x $VALSIZE)}|SET __rand_int__ $VAL"
)

# f1srv: start in an 8G scope, load all (index+keys resident, values to the cold log),
# tighten to CAP in place, drop caches, then serve. No reload: f1srv has no on-disk
# snapshot yet (M2), it keeps the arena resident and reads values from the cold log.
meas_f1() {
  local load=$1 pr=$2
  reset_scope; wait_port_free; drop_caches; local D; D=$(mktemp -d "$WORKDIR/ltms.XXXXXX")
  systemd-run --quiet --unit=ltms --scope -p MemoryMax=8G -p MemorySwapMax=8G \
    $F1 server --addr 127.0.0.1:$PORT --dir $D --ltm-cold --sep-threshold $SEP \
    --index-buckets $IBUCKETS --arena-bytes $ARENA >$D/f 2>&1 & local p=$!
  wait_up || { echo "  f1srv:  start-FAIL" >&2; tail -3 $D/f >&2; reset_scope; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  load_pipe "$load"
  redis-cli -p $PORT ping >/dev/null 2>&1 || { echo "  f1srv:  DIED-on-load" >&2; tail -3 $D/f >&2; reset_scope; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  local lrss; lrss=$(rss_of $p); local sc; sc=$(redis-cli -p $PORT dbsize | tr -d '\r')
  local vmb; vmb=$(du -sm $D 2>/dev/null | cut -f1)
  systemctl set-property --runtime ltms.scope MemoryMax=$CAP MemorySwapMax=$SWAP 2>/dev/null
  sleep 8; drop_caches; sleep 2
  redis-cli -p $PORT ping >/dev/null 2>&1 || { echo "  f1srv:  DIED-on-cap (loaded_rss=${lrss}MB)" >&2; reset_scope; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  local g; g=$(probe "$pr")
  echo "  f1srv:  rps=$g  rss_MB=$(rss_of $p)  loaded_rss=${lrss}MB  cold_MB=$vmb  swap=$(swap_mb)MB  keys=$sc" >&2
  reset_scope; kill $p 2>/dev/null; wait $p 2>/dev/null; rm -rf $D; echo "$g"
}

# redis/valkey: start in an 8G scope, load all in RAM, THEN tighten to CAP so the value
# overflow swaps, drop the file cache, and serve.
meas_mem() {
  local name=$1 bin=$2 load=$3 pr=$4
  reset_scope; wait_port_free; drop_caches; local D; D=$(mktemp -d "$WORKDIR/ltms.XXXXXX")
  systemd-run --quiet --unit=ltms --scope -p MemoryMax=8G -p MemorySwapMax=8G \
    $bin --port $PORT --dir $D --save "" --appendonly no >$D/r 2>&1 & local p=$!
  wait_up || { echo "  $name: start-FAIL" >&2; tail -2 $D/r >&2; reset_scope; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  load_pipe "$load"
  redis-cli -p $PORT ping >/dev/null 2>&1 || { echo "  $name: DIED-on-load" >&2; reset_scope; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  local lrss; lrss=$(rss_of $p); local sc; sc=$(redis-cli -p $PORT dbsize | tr -d '\r')
  systemctl set-property --runtime ltms.scope MemoryMax=$CAP MemorySwapMax=$SWAP 2>/dev/null
  sleep 8; drop_caches; sleep 2
  redis-cli -p $PORT ping >/dev/null 2>&1 || { echo "  $name: DIED-on-cap (loaded_rss=${lrss}MB)" >&2; reset_scope; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  local g; g=$(probe "$pr")
  echo "  $name: rps=$g  rss_MB=$(rss_of $p)  loaded_rss=${lrss}MB  swap=$(swap_mb)MB  keys=$sc" >&2
  reset_scope; kill $p 2>/dev/null; wait $p 2>/dev/null; rm -rf $D; echo "$g"
}

raw=$(awk "BEGIN{printf \"%.0f\", $N*$VALSIZE/1048576}")
echo "== FAIR LTM string matrix: $N keys x ${VALSIZE}B (~${raw}MB raw values), load-uncapped then cap=$CAP, $(nproc) cores =="
echo "== f1srv sep-threshold=$SEP; redis $($REDIS --version | grep -o 'v=[0-9.]*') | valkey $($VALKEY --version | grep -o 'v=[0-9.]*') =="
echo "== uniform-random probes; zipfian/skew rows are a separate follow-up, not measured here =="

for row in "${ROWS[@]}"; do
  IFS='|' read -r name load pr <<<"$row"
  echo "--- $name (string) ---"
  for rep in $(seq 1 $REPS); do
    echo ">>> rep=$rep"
    A=$(meas_f1  "$load" "$pr")
    R=$(meas_mem redis  "$REDIS"  "$load" "$pr")
    V=$(meas_mem valkey "$VALKEY" "$load" "$pr")
    ar=$(awk "BEGIN{if($R>0)printf \"%.2f\",$A/$R; else print \"na\"}")
    av=$(awk "BEGIN{if($V>0)printf \"%.2f\",$A/$V; else print \"na\"}")
    printf "  RATIO %-6s f1srv=%s redis=%s valkey=%s  f1srv/redis=%s f1srv/valkey=%s\n" "$name" "$A" "$R" "$V" "$ar" "$av"
  done
done
echo "== done, load $(cut -d' ' -f1 /proc/loadavg) =="
