# Larger-than-memory scenario

aki's design goal is a single-file store that serves a dataset larger than RAM.
The Go harness in this repo measures the in-memory saturation case, where every
engine is bottlenecked on the loopback socket and the storage path never shows up.
This scenario measures the other case: a hard RAM cap with a dataset several times
larger than the cap, where the storage path is the whole story.

`run.sh` puts aki, Redis, and Valkey under the same RAM and swap ceiling, loads a
dataset that overflows it, and measures random GET throughput and resident memory.
It is Linux only and needs root, because it uses cgroup v2 scopes
(`systemd-run --scope -p MemoryMax -p MemorySwapMax`) and drops the page cache
between phases. Each engine runs in its own scope, so the host and anything else on
the box are untouched.

## The methodology, and the one deliberate asymmetry

cgroup v2 MemoryMax counts the file page cache, not just anonymous heap. So the
engines are loaded differently, on purpose, to reflect what each one is:

- aki persists the whole dataset to one file. It is loaded UNCAPPED, the page cache
  is dropped, then it serves read-only UNDER the cap. Every file page it then
  touches is clean and reclaimable, so the cap throttles its resident set exactly
  as a small-RAM box would, with no OOM. Capping aki during its own write-load
  instead fills the cap with dirty pages and the kernel OOM-kills it, which is a
  property of file-backed writes under cgroup accounting, not a fair test.
- Redis and Valkey live in the heap. They are loaded and served INSIDE the cap and
  their overflow swaps, which is their real larger-than-memory behavior.

Because of that asymmetry the load times are not a like-for-like write contest
(aki's load is uncapped, the others' are capped), so this scenario reports them but
does not draw a write ratio from them. The honest comparison here is the read
throughput and the resident memory, both measured with every engine serving under
the identical cap.

## What it found

server2, 6 cores, kernel 6.8, run June 2026. The result depends sharply on value
size, and that dependence is the point.

Resident memory, every configuration, no exceptions: aki holds about 11 MB
resident while serving a multiple-of-RAM dataset under the cap. Redis and Valkey
sit pinned at the cap (about 180-190 MB) with the rest in swap. That is a roughly
17-22x smaller resident set, and it is the robust, direction-stable win of the
design. It is a memory result, not a throughput result.

Read throughput, aki over Redis, as value size grows (192-256 MB cap, dataset
several times over). The small and mid-value rows are single runs; the large-value
rows are the median of three back-to-back reps, with the full spread shown because
the spread is the point:

| value | keys | aki/redis GET    | notes                                       |
| ----- | ---- | ---------------- | ------------------------------------------- |
| 128 B | 3 M  | 0.72x            | aki loses: btree descent dominates          |
| 1 KB  | 800 K| 1.25x            | single run, modest win                      |
| 4 KB  | 100 K| 1.26x            | single run, modest win                      |
| 32 KB | 80 K | 1.27x median     | three reps: 1.06x, 1.27x, 2.96x             |
| 64 KB | 40 K | 1.33x median     | two completed reps: 1.34x, 1.32x            |

The shape is real and has a mechanism. A btree GET costs O(log n) page reads
(root, interior levels, leaf); a hash GET costs O(1). At small values and high key
counts the descent dominates and aki loses, because under cache pressure each level
that misses is its own random read. As values grow and key counts fall the tree
gets shallow, its upper levels stay resident, and each GET is dominated by moving
the value bytes off disk, which aki does as one contiguous file read against
Redis's swapped-from-heap object. So the ratio climbs with value size: aki turns a
0.72x loss at 128 B into a roughly 1.3x win at 32-64 KB.

What this does NOT establish is a robust 2x, and the reps prove why. The 32 KB
configuration, byte for byte identical across three reps, produced 1.06x and 2.96x:
a factor-of-three spread on the same config. That is not aki speeding up or slowing
down, it is the shared swap and disk subsystem changing state between runs, which
under a hard cap with the dataset 12x over it sets every engine's read rate. Earlier
single runs that touched 2.0x were the right tail of that noise, not a reproducible
operating point; rerunning them gave the 1.3x medians above. Against Valkey the same
data sits near 1.0x: the two trade the lead. So the honest read result at large
values is a modest, noise-dominated ~1.3x over Redis and a wash against Valkey, not
a 2x. Rerun `run.sh` on a quiet box with several reps per config and quote the
median, never a single ratio.

## Running it

```
sudo CAP=256M SWAP=2048M KEYS=800000 VAL=1024 ./run.sh        # 1 KB baseline
sudo CAP=200M SWAP=6144M KEYS=80000  VAL=32768 ./run.sh       # large value
```

Parameters are environment overrides: `CAP`, `SWAP`, `KEYS`, `VAL`, `GETN`, `BP`
(aki buffer pool), `AKI` (binary), `PORT`. It prints one line per engine with GET
rps, resident MB, and load seconds, and reports whether each engine loaded under
the cap or uncapped. Needs `redis-server`, `valkey-server`, `redis-cli`,
`redis-benchmark`, and an `aki` binary on PATH or via `AKI=`.
```
