#!/bin/bash
# Fair larger-than-memory collection matrix: aki vs Redis 8.8 vs Valkey 9.1.
#
# This is the table-driven generalization of ltm_set_fair.sh and ltm_zset_fair.sh
# (spec 2064/ltm/05). One row per gated collection read. Every row loads ONE
# collection larger than the RAM cap, then measures random point reads against it
# under a hard cgroup cap. A row is "command + the load that builds its collection +
# the redis-benchmark probe". Adding a command is a row, not a new script.
#
# The fairness rule that matters (spec 2064/ltm/05 section 1): every engine LOADS
# with full RAM, THEN the cap is tightened below the dataset and reads are served.
# This isolates the serve-time larger-than-memory effect, which is aki's selling
# point, and removes the load-time swap-thrash artifact that made a capped Valkey
# load take ~100 minutes. aki serves from its single file through the bounded buffer
# pool; Redis and Valkey hold the whole collection in heap and fault the swapped
# overflow on each read.
#
# Members are 255B: a 12-digit id at the FRONT then 243 bytes of pad. The id-first
# shape is what redis-benchmark's __rand_int__ (a 12-digit zero-padded random in
# [0,r)) substitutes, so a probe hits a stored element. The pad keeps leaf
# separators short so aki's interior index stays small.
#
# This run is uniform-random. Zipfian (skew) is a separate row set the spec calls
# for to show the hot-tier win; it is NOT measured here and is logged as a gap, not
# silently dropped.
set -u

CAP=${CAP:-300M}        # RAM cap the read phase serves under
SWAP=${SWAP:-4096M}     # swap room the rivals fault into
N=${N:-3000000}         # elements in the one big collection (~765MB raw at 255B)
PORT=${PORT:-7047}
GETN=${GETN:-100000}    # random probes per measurement
CLIENTS=${CLIENTS:-50}
PIPE=${PIPE:-16}
REPS=${REPS:-2}
BP=${BP:-128mb}         # aki buffer-pool budget: how much of the .aki file stays resident
AKI=${AKI:-$HOME/akiperf/aki-ltm}
REDIS=${REDIS:-redis-server}
VALKEY=${VALKEY:-$HOME/akiperf/valkey-9.1.0/src/valkey-server}

if [ "$(id -u)" != 0 ]; then echo "needs root for cgroup scopes and drop_caches" >&2; exit 1; fi
PAD=$(perl -e "print 'x' x 243")
VAL=$(perl -e "print 'v' x 255")

reset_scope() { systemctl stop ltm.scope 2>/dev/null; systemctl reset-failed ltm.scope 2>/dev/null; sleep 1; }
drop_caches() { sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null; }
wait_up() { local i; for i in $(seq 1 600); do redis-cli -p $PORT ping >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }
swap_mb() { free -m | awk '/Swap/{print $3}'; }
rss_mb() { local pid; pid=$(redis-cli -p $PORT info server 2>/dev/null | tr -d '\r' | awk -F: '/process_id/{print $2}'); [ -n "$pid" ] && awk '/VmRSS/{printf "%d",$2/1024}' /proc/$pid/status 2>/dev/null; }
# Pull the rate token that sits right before "requests" in redis-benchmark -q output,
# so the probe command's own arguments never shift a fixed column.
rate() { tr '\r' '\n' | awk '{for(i=1;i<=NF;i++)if($i=="requests")v=$(i-1)} END{print v+0}'; }

# --- the row table -----------------------------------------------------------
# Each row is: name | type | load-perl | card-cmd | probe redis-benchmark args.
# load-perl prints inline commands to stream through redis-cli --pipe; it expands
# $N and $PAD and $VAL at eval time.
ROWS=(
  "sismember|set|for(0..$N-1){printf \"SADD s:0 %012d${PAD}\n\",\$_}|scard s:0|SISMEMBER s:0 __rand_int__${PAD}"
  "zscore|zset|for(0..$N-1){printf \"ZADD z:0 %d %012d${PAD}\n\",\$_,\$_}|zcard z:0|ZSCORE z:0 __rand_int__${PAD}"
  "zrank|zset|for(0..$N-1){printf \"ZADD z:0 %d %012d${PAD}\n\",\$_,\$_}|zcard z:0|ZRANK z:0 __rand_int__${PAD}"
  "hget|hash|for(0..$N-1){printf \"HSET h:0 %012d${PAD} ${VAL}\n\",\$_}|hlen h:0|HGET h:0 __rand_int__${PAD}"
)

load_pipe() { perl -e "$1" | redis-cli -p $PORT --pipe >/dev/null 2>&1; }
probe() { redis-benchmark -p $PORT -r $N -n $GETN -c $CLIENTS -P $PIPE -q $1 2>/dev/null | rate; }

# aki: load uncapped to its file, save, restart serving from the file under the cap.
meas_aki() {
  local load=$1 card=$2 pr=$3
  reset_scope; local D; D=$(mktemp -d /tmp/ltm.XXXXXX)
  $AKI server --port $PORT --admin-port 0 --buffer-pool-size $BP --dir $D >$D/l 2>&1 & local p=$!
  wait_up || { echo "  aki:    load-start-FAIL" >&2; tail -2 $D/l >&2; kill $p 2>/dev/null; rm -rf $D; echo 0; return; }
  load_pipe "$load"; redis-cli -p $PORT save >/dev/null 2>&1
  local sc; sc=$(redis-cli -p $PORT $card | tr -d '\r')
  redis-cli -p $PORT shutdown nosave >/dev/null 2>&1; sleep 1; kill $p 2>/dev/null; wait $p 2>/dev/null
  local fmb; fmb=$(du -sm $D 2>/dev/null | cut -f1)
  drop_caches
  systemd-run --quiet --unit=ltm --scope -p MemoryMax=$CAP -p MemorySwapMax=$SWAP \
    $AKI server --port $PORT --admin-port 0 --buffer-pool-size $BP --dir $D >$D/s 2>&1 &
  wait_up || { echo "  aki:    serve-FAIL-or-OOM" >&2; tail -2 $D/s >&2; reset_scope; rm -rf $D; echo 0; return; }
  local g; g=$(probe "$pr")
  echo "  aki:    rps=$g  rss_MB=$(rss_mb)  card=$sc  file_MB=$fmb" >&2
  reset_scope; rm -rf $D; echo "$g"
}

# redis/valkey: start in an 8G scope, load all in RAM, THEN tighten to CAP so the
# overflow swaps, drop the file cache, and serve.
meas_mem() {
  local name=$1 bin=$2 load=$3 card=$4 pr=$5
  reset_scope; drop_caches; local D; D=$(mktemp -d /tmp/ltm.XXXXXX)
  systemd-run --quiet --unit=ltm --scope -p MemoryMax=8G -p MemorySwapMax=8G \
    $bin --port $PORT --dir $D --save "" --appendonly no >$D/r 2>&1 &
  wait_up || { echo "  $name: start-FAIL" >&2; tail -2 $D/r >&2; reset_scope; rm -rf $D; echo 0; return; }
  load_pipe "$load"
  redis-cli -p $PORT ping >/dev/null 2>&1 || { echo "  $name: DIED-on-load" >&2; reset_scope; rm -rf $D; echo 0; return; }
  local lrss; lrss=$(rss_mb); local sc; sc=$(redis-cli -p $PORT $card | tr -d '\r')
  systemctl set-property --runtime ltm.scope MemoryMax=$CAP MemorySwapMax=$SWAP 2>/dev/null
  sleep 8; drop_caches; sleep 2
  redis-cli -p $PORT ping >/dev/null 2>&1 || { echo "  $name: DIED-on-cap (loaded_rss=${lrss}MB)" >&2; reset_scope; rm -rf $D; echo 0; return; }
  local g; g=$(probe "$pr")
  echo "  $name: rps=$g  rss_MB=$(rss_mb)  loaded_rss=${lrss}MB  swap=$(swap_mb)MB  card=$sc" >&2
  reset_scope; rm -rf $D; echo "$g"
}

raw=$(awk "BEGIN{printf \"%.0f\", $N*255/1048576}")
echo "== FAIR LTM collection matrix: $N elements x 255B (~${raw}MB raw), load-uncapped then cap=$CAP, $(nproc) cores =="
echo "== aki=$($AKI --version 2>/dev/null | head -1) | redis $($REDIS --version | grep -o 'v=[0-9.]*') | valkey $($VALKEY --version | grep -o 'v=[0-9.]*') =="
echo "== uniform-random probes; zipfian/skew rows are a separate follow-up, not measured here =="

for row in "${ROWS[@]}"; do
  IFS='|' read -r name typ load card pr <<<"$row"
  echo "--- $name ($typ) ---"
  for rep in $(seq 1 $REPS); do
    echo ">>> rep=$rep"
    A=$(meas_aki "$load" "$card" "$pr")
    R=$(meas_mem redis  "$REDIS"  "$load" "$card" "$pr")
    V=$(meas_mem valkey "$VALKEY" "$load" "$card" "$pr")
    ar=$(awk "BEGIN{if($R>0)printf \"%.2f\",$A/$R; else print \"na\"}")
    av=$(awk "BEGIN{if($V>0)printf \"%.2f\",$A/$V; else print \"na\"}")
    printf "  RATIO %-10s aki=%s redis=%s valkey=%s  aki/redis=%s aki/valkey=%s\n" "$name" "$A" "$R" "$V" "$ar" "$av"
  done
done
echo "== done, load $(cut -d' ' -f1 /proc/loadavg) =="
