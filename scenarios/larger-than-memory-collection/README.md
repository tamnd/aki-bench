# Larger-than-memory collection scenario

The sibling `larger-than-memory` scenario measures many small string keys under a
hard RAM cap. This one measures the regime that is aki's sharpest structural claim: a
SINGLE collection larger than RAM with random element access. It is the workload that
separates a file-backed store from a heap store the most, and it is where aki's
element-per-row collection storage pays off the most.

`run.sh` builds one hash `h:0` with `F` fields of `V` bytes each, sized several times
larger than a hard cgroup RAM cap, then measures random `HGET` throughput and
resident memory for aki, Redis, and Valkey under the identical cap. Linux only, needs
root (cgroup v2 scopes and `drop_caches`). Same deliberate asymmetry as the string
scenario: aki loads uncapped to its single file, drops the page cache, then serves
read-only under the cap (clean reclaimable pages, no OOM); Redis and Valkey load and
serve inside the cap and overflow to swap, their real and only larger-than-memory
behavior. Fields are named `field:%012d` so redis-benchmark's 12-digit `__rand_int__`
reconstructs a stored field.

## What it found

server2, 6 cores, kernel 6.8, June 2026. Rivals Redis 8.8.0 (latest, a tougher bar
than the 7.4 the compat suite targets) and Valkey 7.2.12. 300 MB cap, 4096 MB swap.

Random HGET throughput, hash several times larger than the cap:

| config            | over cap | aki HGET     | redis HGET  | aki/redis        | aki/valkey       |
| ----------------- | -------- | ------------ | ----------- | ---------------- | ---------------- |
| 3M x 256 B (732MB)| ~2.4x    | 11.9k-15.1k  | 1.8k-2.0k   | 6.6x-7.9x (med 7.5x) | 9.3x-12.2x   |
| 1M x 1 KB (1.0GB) | ~3.3x    | 13.7k-17.0k  | 1.9k-2.0k   | 7.1x-8.6x        | 8.5x-10.4x       |

Five independent over-cap runs across two value sizes, every one at least 6.6x Redis
and 8.5x Valkey, clustered tightly (this is not the swap-noise wash the string
large-value case was). Resident memory: aki 11 MB every run; Redis and Valkey pinned
at the 300 MB cap with the rest in swap, the same ~27x resident win.

The control that bounds the claim, hash that FITS in RAM:

| config            | fits     | aki HGET     | redis HGET  | aki/redis |
| ----------------- | -------- | ------------ | ----------- | --------- |
| 200k x 256 B (50MB)| under cap| 88k-153k    | 136k-255k   | 0.60-0.64x|

When the hash fits, aki is slower than Redis (0.6x). So aki is not generically fast at
hashes; the win is purely the larger-than-memory effect and vanishes the moment the
data fits. That bounds the result precisely and honestly.

## Why

A Redis hash with millions of fields is a dict of sds objects scattered across the
heap. Under a cap that holds a fraction of it, a uniformly random HGET faults in the
bucket, the entry and two sds objects, mostly from swap, and swap-in latency floors
the rate near 1,900 HGET/s. aki stores the hash element-per-row in a btree sub-tree
whose KEY is the small field name and whose VALUE carries the bytes, so separators are
tiny, fanout is high, the tree is shallow, its upper levels stay resident in the
buffer pool, and a random HGET costs about one clean reclaimable leaf-page read. One
cheap page read against several scattered swap-ins is the whole 7x.

## Boundary: large-key sets

This does not generalize to every collection type. The same test with a 3M-member SET
of ~256 B members (random SISMEMBER) OOM-kills aki's serve-under-cap process, at both
a 128 MB and a 64 MB pool, because a set stores each member AS the btree key, so
255-byte members collapse the fanout and the index pages alone overflow the cap. The
hash escapes this only because its key is small and the bytes ride in the value. So
the proven result is for large hashes (and any collection whose elements live in the
value behind a small key); large-key sets under a tight cap are an open boundary.

## Running it

```
sudo CAP=300M SWAP=4096M F=3000000 V=256  ./run.sh    # 732 MB hash, ~2.4x over cap
sudo CAP=300M SWAP=4096M F=1000000 V=1024 ./run.sh    # 1.0 GB hash, ~3.3x over cap
sudo CAP=300M SWAP=4096M F=200000  V=256  ./run.sh    # 50 MB control, fits in RAM
```

Parameters are environment overrides: `CAP`, `SWAP`, `F` (fields), `V` (value bytes),
`GETN`, `BP` (aki buffer pool), `AKI` (binary), `PORT`. Needs `redis-server`,
`valkey-server`, `redis-cli`, `redis-benchmark`, and an `aki` binary on PATH or via
`AKI=`. Throughput under a hard cap with the dataset many times over it is set by the
disk and swap subsystem, so run several reps on a quiet box and read the cluster, not
a single number.
