#!/bin/bash
# The sqlo1 core suite (spec 2064/sqlo1 doc 13): mixed workloads at 90/10,
# 50/50, and 10/90 read ratios, value sizes 16/128/512/4096 B, uniform and
# zipfian (theta 0.99) access, datasets at 1x, 4x, and 16x of the memory cap,
# two arms per cell, alternating measurement order across reps, a per-rep
# timeout, VmHWM and disk footprint per server per cell, and a manifest that
# pins the rival versions the run claims to have measured.
#
# The two arms are the doc 13 fairness protocol:
#
#   cap:  every rival runs under --maxmemory CAP with allkeys-lfu, the honest
#         larger-than-memory posture for an in-RAM engine (an uncapped rival
#         just swaps). Rivals evict, sqlo1 is supposed to tier; the coverage
#         probe and the hit ratio then say who still has the data.
#   data: nobody is capped, everyone holds the full dataset, and the measured
#         outcome is the memory column: VmHWM for the same data is the G2 gate
#         (at or below 1.0x of the best rival passes, 0.5x is the headline).
#
# The AKISLOT variable picks which server rides aki-bench's aki slot: sqlo1
# (default, the spec 2064/sqlo1 driver) or f3 (the current product engine).
# The S0 exit-gate baseline table is an AKISLOT=f3 pass, since the placeholder
# sqlo1 store has no numbers worth recording yet; sqlo1 passes exist first to
# prove the plumbing (the slice 6 self-proof) and later to gate G1.
#
# Every server is relaunched per cell, because VmHWM is a per-process high
# water mark: a peak inherited from an earlier, larger cell would poison every
# later memory reading. Preload is inside the measured invocation on purpose;
# the peak that counts includes building the dataset.
#
# This script needs Linux for /proc VmHWM and cgroup-free honesty about memory,
# so it refuses to run elsewhere. The gate box is the WSL2 GamingPC; the
# standing per-rep timeout discipline there is 240 s, which is the REP_TIMEOUT
# default, scaled by the dataset multiple since a 16x preload is 16x the work.
set -u

AKISLOT=${AKISLOT:-sqlo1}          # sqlo1 or f3: who rides the aki slot
ARMS=${ARMS:-"cap data"}
MIXES=${MIXES:-"90 50 10"}          # read percent of the mixed workload
SIZES=${SIZES:-"16 128 512 4096"}   # value sizes in bytes
DISTS=${DISTS:-"uniform zipfian"}
SCALES=${SCALES:-"1 4 16"}          # dataset as a multiple of the cap
REPS=${REPS:-2}                     # even numbers alternate the order evenly
CAP_MIB=${CAP_MIB:-512}             # the memory cap the scales multiply
KEY_OVERHEAD=${KEY_OVERHEAD:-100}   # assumed per-key metadata bytes; the scale
                                    # arithmetic sizes the dataset against the
                                    # cap by value+overhead, not value alone,
                                    # or a 16 B cell would need half a billion
                                    # keys to overflow a 512 MiB cap
MAXKEYS=${MAXKEYS:-30000000}        # hard keyspace ceiling, truncation is loud
DURATION=${DURATION:-30s}
WARM=${WARM:-10s}
CONNS=${CONNS:-64}
PIPE=${PIPE:-16}
IO_THREADS=${IO_THREADS:-4}
COVERAGE_PROBE=${COVERAGE_PROBE:-2000}
REP_TIMEOUT=${REP_TIMEOUT:-240}     # seconds, multiplied by the cell's scale
REDIS=${REDIS:-redis-server}
VALKEY=${VALKEY:-valkey-server}
REDIS_WANT=${REDIS_WANT:-8.8}       # pinned rival versions; a drifted binary
VALKEY_WANT=${VALKEY_WANT:-9.1}     # fails the run unless ALLOW_VERSION_DRIFT=1
ALLOW_VERSION_DRIFT=${ALLOW_VERSION_DRIFT:-0}
AKI_DIR=${AKI_DIR:-../aki}          # tamnd/aki checkout to build sqlo1srv/f3srv
BENCH=${BENCH:-}                    # prebuilt aki-bench; empty builds one
SQLO1SRV=${SQLO1SRV:-}              # prebuilt sqlo1srv; empty builds from AKI_DIR
F3SRV=${F3SRV:-}                    # prebuilt f3srv; empty builds from AKI_DIR
SHARDS=${SHARDS:-4}                 # f3srv shards when AKISLOT=f3
ARENA_MIB=${ARENA_MIB:-256}         # f3srv per-shard arena when AKISLOT=f3
PORT_AKI=${PORT_AKI:-7321}
PORT_REDIS=${PORT_REDIS:-7322}
PORT_VALKEY=${PORT_VALKEY:-7323}
OUTDIR=${OUTDIR:-$PWD/sqlo1-core.$(date +%Y%m%d-%H%M%S)}
# Server working dirs must sit on a real disk: a tmpfs data dir is RAM
# pretending to be disk, and both the disk column and the tiering regime lie.
WORKDIR=${WORKDIR:-$HOME}

if [ "$(uname -s)" != Linux ]; then
  echo "sqlo1-core: needs Linux for /proc VmHWM; run it on the gate box" >&2
  exit 1
fi
if [ "$(stat -f -c %T "$WORKDIR" 2>/dev/null)" = tmpfs ]; then
  echo "sqlo1-core: WORKDIR=$WORKDIR is tmpfs, use a real disk" >&2
  exit 1
fi
case "$AKISLOT" in sqlo1|f3) ;; *)
  echo "sqlo1-core: AKISLOT=$AKISLOT, want sqlo1 or f3" >&2; exit 1 ;;
esac

WORK=$(mktemp -d "$WORKDIR/sqlo1core.XXXXXX")
pids=()
cleanup() {
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT
mkdir -p "$OUTDIR"

HERE=$(cd "$(dirname "$0")/../.." && pwd)
if [ -z "$BENCH" ]; then
  echo "== building aki-bench =="
  (cd "$HERE" && go build -o "$WORK/aki-bench" ./cmd/aki-bench) || exit 1
  BENCH="$WORK/aki-bench"
fi
if [ "$AKISLOT" = sqlo1 ] && [ -z "$SQLO1SRV" ]; then
  echo "== building sqlo1srv from $AKI_DIR =="
  (cd "$AKI_DIR" && go build -o "$WORK/sqlo1srv" ./cmd/sqlo1srv) || exit 1
  SQLO1SRV="$WORK/sqlo1srv"
fi
if [ "$AKISLOT" = f3 ] && [ -z "$F3SRV" ]; then
  echo "== building f3srv from $AKI_DIR =="
  (cd "$AKI_DIR" && go build -o "$WORK/f3srv" ./cmd/f3srv) || exit 1
  F3SRV="$WORK/f3srv"
fi

# The version pin. The whole point of a baseline table is that it names the
# rival builds it beat or lost to; a run against whatever happened to be on
# PATH is not quotable. Drift fails the run unless explicitly waived.
redis_ver=$("$REDIS" --version 2>/dev/null | grep -o 'v=[0-9.]*' | head -1 | cut -d= -f2)
valkey_ver=$("$VALKEY" --version 2>/dev/null | grep -o 'v=[0-9.]*' | head -1 | cut -d= -f2)
pin_fail=0
case "$redis_ver" in "$REDIS_WANT"*) ;; *)
  echo "sqlo1-core: redis is ${redis_ver:-absent}, pinned $REDIS_WANT" >&2; pin_fail=1 ;;
esac
case "$valkey_ver" in "$VALKEY_WANT"*) ;; *)
  echo "sqlo1-core: valkey is ${valkey_ver:-absent}, pinned $VALKEY_WANT" >&2; pin_fail=1 ;;
esac
if [ "$pin_fail" = 1 ] && [ "$ALLOW_VERSION_DRIFT" != 1 ]; then
  echo "sqlo1-core: version pin failed; install the pinned rivals or set ALLOW_VERSION_DRIFT=1" >&2
  exit 1
fi

# The manifest is the provenance the results directory travels with.
akibench_rev=$(git -C "$HERE" rev-parse HEAD 2>/dev/null || echo unknown)
aki_rev=$(git -C "$AKI_DIR" rev-parse HEAD 2>/dev/null || echo unknown)
{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "box: $(hostname) / $(uname -srm)"
  echo "cpus: $(nproc)"
  echo "aki-bench: $akibench_rev"
  echo "aki checkout: $aki_rev"
  echo "akislot: $AKISLOT"
  echo "redis: $(command -v "$REDIS") v$redis_ver (pinned $REDIS_WANT)"
  echo "valkey: $(command -v "$VALKEY") v$valkey_ver (pinned $VALKEY_WANT)"
  echo "cap_mib: $CAP_MIB  key_overhead: $KEY_OVERHEAD  maxkeys: $MAXKEYS"
  echo "arms: $ARMS  mixes: $MIXES  sizes: $SIZES  dists: $DISTS  scales: $SCALES  reps: $REPS"
  echo "duration: $DURATION  warm: $WARM  conns: $CONNS  pipe: $PIPE  io_threads: $IO_THREADS"
  echo "rep_timeout_base_s: $REP_TIMEOUT (times the cell's scale)"
  [ "$AKISLOT" = f3 ] && echo "f3srv: shards $SHARDS arena_mib $ARENA_MIB"
} > "$OUTDIR/manifest.txt"

CSV="$OUTDIR/results.csv"
echo "arm,mix,value_size,dist,scale,rep,order,keys,server,version,ops_per_sec,value_ops_per_sec,hit_ratio,p50_us,p99_us,used_memory,vmhwm_bytes,disk_bytes,coverage_fraction" > "$CSV"
FAILLOG="$OUTDIR/failures.txt"
: > "$FAILLOG"

wait_up() { # port
  for _ in $(seq 1 300); do
    if printf 'PING\r\n' | timeout 1 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1 && cat >&3 && head -c 7 <&3" 2>/dev/null | grep -q PONG; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

hwm() { # pid -> peak resident bytes, 0 when unreadable
  awk '/^VmHWM:/{print $2*1024; found=1} END{if(!found)print 0}' "/proc/$1/status" 2>/dev/null || echo 0
}

cells=0
for arm in $ARMS; do for mix in $MIXES; do for vs in $SIZES; do
  for dist in $DISTS; do for scale in $SCALES; do cells=$((cells+1)); done; done
done; done; done
echo "== sqlo1 core suite: $cells cells x $REPS reps, akislot=$AKISLOT, out $OUTDIR =="

fail=0
for arm in $ARMS; do
for mix in $MIXES; do
for vs in $SIZES; do
for dist in $DISTS; do
for scale in $SCALES; do
  keys=$(( scale * CAP_MIB * 1048576 / (vs + KEY_OVERHEAD) ))
  if [ "$keys" -gt "$MAXKEYS" ]; then
    echo "!! cell $arm/$mix/$vs/$dist/${scale}x: keyspace $keys truncated to $MAXKEYS; the cell no longer reaches ${scale}x of the cap" | tee -a "$FAILLOG"
    keys=$MAXKEYS
  fi
  for rep in $(seq 1 "$REPS"); do
    # Alternate who is preloaded and measured first, so no server always
    # enjoys the coldest caches or the freshest page cache.
    if [ $((rep % 2)) = 1 ]; then order="aki,redis,valkey"; else order="valkey,redis,aki"; fi
    cell="$arm-r$mix-v$vs-$dist-${scale}x-rep$rep"
    celldir="$WORK/$cell"
    mkdir -p "$celldir/aki" "$celldir/redis" "$celldir/valkey"

    rival_flags=(--bind 127.0.0.1 --save "" --appendonly no --io-threads "$IO_THREADS")
    if [ "$arm" = cap ]; then
      rival_flags+=(--maxmemory "${CAP_MIB}mb" --maxmemory-policy allkeys-lfu)
    fi

    if [ "$AKISLOT" = sqlo1 ]; then
      # S0 sqlo1srv has no memory cap flag yet; on the cap arm it simply runs,
      # and its VmHWM column reports what that costs. Once the real store
      # lands, its cache budget flag goes here.
      (cd "$celldir/aki" && exec "$SQLO1SRV" -addr 127.0.0.1:$PORT_AKI) >"$celldir/aki.log" 2>&1 &
    else
      if [ "$arm" = cap ]; then
        res_mib=$(( CAP_MIB / SHARDS ))
      else
        # Uncapped arm: size the resident budget to hold the full dataset so
        # nothing spills and VmHWM reports the true full-data footprint.
        res_mib=$(( keys * (vs + KEY_OVERHEAD) / 1048576 / SHARDS * 5 / 4 + 64 ))
      fi
      (cd "$celldir/aki" && exec "$F3SRV" --addr 127.0.0.1:$PORT_AKI --shards "$SHARDS" \
        --arena-mib "$ARENA_MIB" --resident-cap-mib "$res_mib" --vlog-dir "$celldir/aki/vlog") \
        >"$celldir/aki.log" 2>&1 &
    fi
    pid_aki=$!
    "$REDIS" --port $PORT_REDIS --dir "$celldir/redis" "${rival_flags[@]}" >"$celldir/redis.log" 2>&1 &
    pid_redis=$!
    "$VALKEY" --port $PORT_VALKEY --dir "$celldir/valkey" "${rival_flags[@]}" >"$celldir/valkey.log" 2>&1 &
    pid_valkey=$!
    pids=("$pid_aki" "$pid_redis" "$pid_valkey")

    up=1
    for port in $PORT_AKI $PORT_REDIS $PORT_VALKEY; do
      wait_up $port || { echo "!! $cell: server on port $port did not come up" | tee -a "$FAILLOG"; up=0; }
    done

    rc=1
    if [ "$up" = 1 ]; then
      echo "--- $cell (order $order, $keys keys) ---"
      timeout $(( REP_TIMEOUT * scale )) "$BENCH" \
        -workload mixed -read-ratio "$mix" \
        -aki-addr 127.0.0.1:$PORT_AKI \
        -redis-addr 127.0.0.1:$PORT_REDIS \
        -valkey-addr 127.0.0.1:$PORT_VALKEY \
        -keys "$keys" -value-size "$vs" \
        -dist "$dist" -zipf-s 0.99 \
        -connections "$CONNS" -pipeline "$PIPE" \
        -warm "$WARM" -duration "$DURATION" \
        -order "$order" \
        -coverage-probe "$COVERAGE_PROBE" \
        -json "$OUTDIR/$cell.json"
      rc=$?
      # Exit 2 is the gate verdict, a result rather than a failure. 124 is the
      # per-rep timeout tripping.
      if [ "$rc" != 0 ] && [ "$rc" != 2 ]; then
        echo "!! $cell: aki-bench exit $rc$([ "$rc" = 124 ] && echo ' (per-rep timeout)')" | tee -a "$FAILLOG"
        fail=1
      fi
    else
      fail=1
    fi

    # Peak memory before the kill, while /proc still has the processes, then
    # the on-disk footprint of what each server left behind.
    hwm_aki=$(hwm "$pid_aki"); hwm_redis=$(hwm "$pid_redis"); hwm_valkey=$(hwm "$pid_valkey")
    for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
    wait 2>/dev/null
    pids=()
    disk_aki=$(du -sb "$celldir/aki" 2>/dev/null | cut -f1)
    disk_redis=$(du -sb "$celldir/redis" 2>/dev/null | cut -f1)
    disk_valkey=$(du -sb "$celldir/valkey" 2>/dev/null | cut -f1)

    if [ "$rc" = 0 ] || [ "$rc" = 2 ]; then
      python3 - "$OUTDIR/$cell.json" <<PYEOF >> "$CSV"
import json, sys
cell = json.load(open(sys.argv[1]))
slot = {"aki": "$AKISLOT", "redis": "redis", "valkey": "valkey"}
extra = {
    "aki": ($hwm_aki, ${disk_aki:-0}),
    "redis": ($hwm_redis, ${disk_redis:-0}),
    "valkey": ($hwm_valkey, ${disk_valkey:-0}),
}
for name in ("aki", "redis", "valkey"):
    e = cell[name]
    if e.get("skipped"):
        continue
    hwm, disk = extra[name]
    print(",".join(str(x) for x in [
        "$arm", "$mix", "$vs", "$dist", "$scale", "$rep", "$order".replace(",", " "), "$keys",
        slot[name], e.get("version", ""),
        round(e.get("ops_per_sec", 0), 1), round(e.get("value_ops_per_sec", 0), 1),
        round(e.get("hit_ratio", 0), 4),
        e.get("p50_us", 0), e.get("p99_us", 0),
        e.get("used_memory", 0), hwm, disk,
        e.get("coverage_fraction", ""),
    ]))
PYEOF
    fi
    rm -rf "$celldir"
  done
done
done
done
done
done

echo "== done: $CSV, manifest and per-cell JSON beside it =="
if [ -s "$FAILLOG" ]; then
  echo "== failures were logged: =="
  cat "$FAILLOG"
fi
[ "$fail" = 0 ] || exit 1
