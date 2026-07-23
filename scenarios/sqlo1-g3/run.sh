#!/bin/bash
# The sqlo1 G3 disk cell (spec 2064/sqlo1 doc 13): on-disk bytes per entry,
# per type-suite corpus, sqlo1 vs the Redis RDB snapshot, the Valkey RDB
# snapshot, and the f3 engine's persisted footprint.
#
# Protocol per cell (one suite = one corpus):
#
#   1. Load the corpus, then churn it: overwrite or replace about half of
#      the entries with fresh values of the same shape. Every server gets
#      byte-identical traffic from the deterministic generator. Churn is
#      part of the protocol because a disk engine compacts (and compresses)
#      only where dead records exist, and because a snapshot format that
#      never sees an overwrite is not being measured either.
#   2. Rivals: SAVE, SIGTERM, du of their data dir. That is the RDB file,
#      rdbcompression yes (the default), the strongest honest form of
#      their persisted footprint.
#   3. f3: SIGTERM, du of its working dir (vlog plus whatever it persists).
#   4. sqlo1: a cap ladder. The sqlo1b data file only ever grows, so du at
#      the end reads the high-water mark, and the free-extent gauge that
#      drives compaction only turns on under a -max-bytes budget. The
#      honest number is therefore the smallest budget under which the full
#      load and churn complete without a single shed error: the engine
#      held the dataset and served every write inside a file of that size.
#      The ladder runs tightest first, fractions of the measured Redis RDB
#      size, and the table quotes the tightest clean rung; an unbounded
#      rung always closes the ladder so the raw high-water goes on record
#      even if every capped rung sheds.
#
# Every row lands in results.csv; the manifest pins versions and revisions.
# Linux only: GNU du -sb, /proc conventions, and the gate-box discipline.
set -u

SUITES=${SUITES:-"str-json str-ts str-uuid str-const hash set zset list stream"}
ENTRIES=${ENTRIES:-1000000}
CAP_FRACS=${CAP_FRACS:-"0.40 0.55 0.70 1.00 1.50"}
CAP_FLOOR_MIB=${CAP_FLOOR_MIB:-64}  # a budget below the ladder's reserve just wedges
SEED=${SEED:-42}
PIPE_TIMEOUT=${PIPE_TIMEOUT:-1200}  # seconds per load or churn pass
REDIS=${REDIS:-redis-server}
REDIS_CLI=${REDIS_CLI:-redis-cli}
VALKEY=${VALKEY:-valkey-server}
REDIS_WANT=${REDIS_WANT:-8.8}
VALKEY_WANT=${VALKEY_WANT:-9.1}
ALLOW_VERSION_DRIFT=${ALLOW_VERSION_DRIFT:-0}
AKI_DIR=${AKI_DIR:-../aki}
SQLO1SRV=${SQLO1SRV:-}              # prebuilt sqlo1srv; empty builds from AKI_DIR
F3SRV=${F3SRV:-}                    # prebuilt f3srv; empty builds from AKI_DIR
SHARDS=${SHARDS:-4}
ARENA_MIB=${ARENA_MIB:-256}
F3_RES_MIB=${F3_RES_MIB:-128}       # per-shard resident cap so f3 actually spills
PORT=${PORT:-7331}
OUTDIR=${OUTDIR:-$PWD/sqlo1-g3.$(date +%Y%m%d-%H%M%S)}
# Server working dirs must sit on a real disk: a tmpfs data dir is RAM
# pretending to be disk, and the whole point of this cell is the disk.
WORKDIR=${WORKDIR:-$HOME}

if [ "$(uname -s)" != Linux ]; then
  echo "sqlo1-g3: needs Linux (GNU du, gate-box discipline); run it on the gate box" >&2
  exit 1
fi
if [ "$(stat -f -c %T "$WORKDIR" 2>/dev/null)" = tmpfs ]; then
  echo "sqlo1-g3: WORKDIR=$WORKDIR is tmpfs, use a real disk" >&2
  exit 1
fi

WORK=$(mktemp -d "$WORKDIR/sqlo1g3.XXXXXX")
pids=()
cleanup() {
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT
mkdir -p "$OUTDIR"

HERE=$(cd "$(dirname "$0")/../.." && pwd)
GEN="$(cd "$(dirname "$0")" && pwd)/gen.py"
if [ -z "$SQLO1SRV" ]; then
  echo "== building sqlo1srv from $AKI_DIR =="
  (cd "$AKI_DIR" && go build -o "$WORK/sqlo1srv" ./cmd/sqlo1srv) || exit 1
  SQLO1SRV="$WORK/sqlo1srv"
fi
if [ -z "$F3SRV" ]; then
  echo "== building f3srv from $AKI_DIR =="
  (cd "$AKI_DIR" && go build -o "$WORK/f3srv" ./cmd/f3srv) || exit 1
  F3SRV="$WORK/f3srv"
fi

redis_ver=$("$REDIS" --version 2>/dev/null | grep -o 'v=[0-9.]*' | head -1 | cut -d= -f2)
valkey_ver=$("$VALKEY" --version 2>/dev/null | grep -o 'v=[0-9.]*' | head -1 | cut -d= -f2)
pin_fail=0
case "$redis_ver" in "$REDIS_WANT"*) ;; *)
  echo "sqlo1-g3: redis is ${redis_ver:-absent}, pinned $REDIS_WANT" >&2; pin_fail=1 ;;
esac
case "$valkey_ver" in "$VALKEY_WANT"*) ;; *)
  echo "sqlo1-g3: valkey is ${valkey_ver:-absent}, pinned $VALKEY_WANT" >&2; pin_fail=1 ;;
esac
if [ "$pin_fail" = 1 ] && [ "$ALLOW_VERSION_DRIFT" != 1 ]; then
  echo "sqlo1-g3: version pin failed; install the pinned rivals or set ALLOW_VERSION_DRIFT=1" >&2
  exit 1
fi

akibench_rev=$(git -C "$HERE" rev-parse HEAD 2>/dev/null || echo unknown)
aki_rev=$(git -C "$AKI_DIR" rev-parse HEAD 2>/dev/null || echo unknown)
{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "box: $(hostname) / $(uname -srm)"
  echo "cpus: $(nproc)"
  echo "aki-bench: $akibench_rev"
  echo "aki checkout: $aki_rev"
  echo "redis: $(command -v "$REDIS") v$redis_ver (pinned $REDIS_WANT, rdbcompression default yes, one SAVE after churn)"
  echo "valkey: $(command -v "$VALKEY") v$valkey_ver (pinned $VALKEY_WANT)"
  echo "f3srv: shards $SHARDS arena_mib $ARENA_MIB resident_cap_mib $F3_RES_MIB"
  echo "suites: $SUITES  entries: $ENTRIES  seed: $SEED"
  echo "cap ladder: fractions [$CAP_FRACS] of the cell's redis RDB bytes, floor ${CAP_FLOOR_MIB} MiB, then unbounded"
} > "$OUTDIR/manifest.txt"

CSV="$OUTDIR/results.csv"
echo "suite,entries,server,version,cap_bytes,clean,pipe_errors,disk_bytes,bytes_per_entry" > "$CSV"
FAILLOG="$OUTDIR/failures.txt"
: > "$FAILLOG"

wait_up() { # port
  for _ in $(seq 1 300); do
    if "$REDIS_CLI" -p "$1" ping 2>/dev/null | grep -q PONG; then return 0; fi
    sleep 0.2
  done
  return 1
}

# feed <suite> <phase> <port> -> echoes the pipe's error count, 999999 on
# a transport-level failure (generator died, server gone, timeout). Note
# redis-cli --pipe exits nonzero when it counted errors, so the exit code
# alone cannot distinguish shed writes from a dead generator; a parsed
# error count wins, and a nonzero exit with a clean count means the
# generator half of the pipe failed.
feed() {
  local out rc errs
  out=$(set -o pipefail; timeout "$PIPE_TIMEOUT" python3 "$GEN" \
    --suite "$1" --entries "$ENTRIES" --phase "$2" --seed "$SEED" \
    | "$REDIS_CLI" -p "$3" --pipe 2>&1)
  rc=$?
  errs=$(echo "$out" | grep -o 'errors: [0-9]*' | head -1 | grep -o '[0-9]*')
  if [ -z "$errs" ] || { [ "$rc" != 0 ] && [ "$errs" = 0 ]; }; then
    echo 999999
  else
    echo "$errs"
  fi
}

row() { # suite server version cap clean errors disk
  python3 -c "
disk = int('$7')
print(','.join(['$1', str($ENTRIES), '$2', '$3', '$4', '$5', '$6', str(disk), str(round(disk / $ENTRIES, 2))]))" >> "$CSV"
}

fail=0
for suite in $SUITES; do
  echo "=== suite $suite ($ENTRIES entries) ==="

  # --- rivals: load, churn, SAVE, kill, du ---
  rdb_redis=0
  for rival in redis valkey; do
    dir="$WORK/$suite-$rival"; mkdir -p "$dir"
    if [ "$rival" = redis ]; then bin="$REDIS"; ver="$redis_ver"; else bin="$VALKEY"; ver="$valkey_ver"; fi
    "$bin" --port "$PORT" --dir "$dir" --bind 127.0.0.1 --save "" --appendonly no >"$dir.log" 2>&1 &
    pid=$!; pids=("$pid")
    if ! wait_up "$PORT"; then
      echo "!! $suite/$rival: did not come up" | tee -a "$FAILLOG"; fail=1
      kill "$pid" 2>/dev/null; wait 2>/dev/null; pids=(); continue
    fi
    errs=$(( $(feed "$suite" load "$PORT") + $(feed "$suite" churn "$PORT") ))
    "$REDIS_CLI" -p "$PORT" save >/dev/null
    kill "$pid" 2>/dev/null; wait 2>/dev/null; pids=()
    disk=$(du -sb "$dir" | cut -f1)
    [ "$rival" = redis ] && rdb_redis=$disk
    clean=1; [ "$errs" = 0 ] || { clean=0; echo "!! $suite/$rival: $errs pipe errors" | tee -a "$FAILLOG"; fail=1; }
    row "$suite" "$rival" "$ver" "" "$clean" "$errs" "$disk"
    rm -rf "$dir"
  done

  # --- f3 ---
  dir="$WORK/$suite-f3"; mkdir -p "$dir/vlog"
  (cd "$dir" && exec "$F3SRV" --addr 127.0.0.1:$PORT --shards "$SHARDS" \
    --arena-mib "$ARENA_MIB" --resident-cap-mib "$F3_RES_MIB" --vlog-dir "$dir/vlog") >"$dir.log" 2>&1 &
  pid=$!; pids=("$pid")
  if wait_up "$PORT"; then
    errs=$(( $(feed "$suite" load "$PORT") + $(feed "$suite" churn "$PORT") ))
    kill "$pid" 2>/dev/null; wait 2>/dev/null; pids=()
    disk=$(du -sb "$dir" | cut -f1)
    clean=1; [ "$errs" = 0 ] || { clean=0; echo "!! $suite/f3: $errs pipe errors" | tee -a "$FAILLOG"; fail=1; }
    row "$suite" f3 "$aki_rev" "" "$clean" "$errs" "$disk"
  else
    echo "!! $suite/f3: did not come up" | tee -a "$FAILLOG"; fail=1
    kill "$pid" 2>/dev/null; wait 2>/dev/null; pids=()
  fi
  rm -rf "$dir"

  # --- sqlo1: the cap ladder, tightest first ---
  if [ "$rdb_redis" = 0 ]; then
    echo "!! $suite: no redis RDB size, skipping the sqlo1 ladder" | tee -a "$FAILLOG"; fail=1; continue
  fi
  caps=$(python3 -c "
fracs = '$CAP_FRACS'.split()
floor = $CAP_FLOOR_MIB * 1048576
caps = sorted(set(max(floor, int(float(f) * $rdb_redis)) for f in fracs))
print(' '.join(str(c) for c in caps), 0)")
  found=0
  for cap in $caps; do
    if [ "$found" = 1 ] && [ "$cap" = 0 ]; then break; fi
    dir="$WORK/$suite-sqlo1-$cap"; mkdir -p "$dir"
    capflag=()
    [ "$cap" != 0 ] && capflag=(-max-bytes "$cap")
    (cd "$dir" && exec "$SQLO1SRV" -addr 127.0.0.1:$PORT -store file -path "$dir/data.aki" "${capflag[@]}") >"$dir.log" 2>&1 &
    pid=$!; pids=("$pid")
    if ! wait_up "$PORT"; then
      echo "!! $suite/sqlo1 cap=$cap: did not come up" | tee -a "$FAILLOG"; fail=1
      kill "$pid" 2>/dev/null; wait 2>/dev/null; pids=(); rm -rf "$dir"; continue
    fi
    errs=$(( $(feed "$suite" load "$PORT") + $(feed "$suite" churn "$PORT") ))
    kill -TERM "$pid" 2>/dev/null
    wait "$pid"; rc_srv=$?
    pids=()
    disk=$(du -sb "$dir" | cut -f1)
    clean=1
    [ "$errs" = 0 ] || clean=0
    [ "$rc_srv" = 0 ] || { clean=0; echo "!! $suite/sqlo1 cap=$cap: shutdown exit $rc_srv" | tee -a "$FAILLOG"; }
    row "$suite" sqlo1 "$aki_rev" "$cap" "$clean" "$errs" "$disk"
    echo "    sqlo1 cap=$cap clean=$clean errors=$errs disk=$disk (redis rdb $rdb_redis)"
    [ "$clean" = 1 ] && found=1
    rm -rf "$dir"
  done
  if [ "$found" = 0 ]; then
    echo "!! $suite/sqlo1: no rung of the ladder finished clean, not even unbounded" | tee -a "$FAILLOG"; fail=1
  fi
done

echo "== done: $CSV, manifest beside it =="
if [ -s "$FAILLOG" ]; then
  echo "== failures were logged: =="
  cat "$FAILLOG"
fi
[ "$fail" = 0 ] || exit 1
