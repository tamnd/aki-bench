# Larger-than-memory collection matrix

This scenario measures aki's sharpest structural claim: one collection larger than
RAM, served under a hard cgroup memory cap, against Redis 8.8 and Valkey 9.1. It is
the table-driven generalization of the per-type fair harnesses (`ltm_set_fair.sh`,
`ltm_zset_fair.sh`) the earlier set and zset numbers came from, written up in spec
2064/ltm/05. Each gated collection read is one row in the table; adding a command is
a row, not a new script.

## What it measures

One collection of `N` elements at about 248 bytes each (a one-char prefix, a decimal
id, then 243 bytes of pad). At the default `N=3000000` that is about 710 MB of raw
element data, served under a 300 MB cap, so the working set is more than twice the cap.
The rows:

| row         | type | builds        | probe                       |
|-------------|------|---------------|-----------------------------|
| sismember   | set  | `SADD s:0 ...`| `SISMEMBER s:0 <rand>`      |
| srandmember | set  | `SADD s:0 ...`| `SRANDMEMBER s:0`           |
| zscore      | zset | `ZADD z:0 ...`| `ZSCORE z:0 <rand>`         |
| zrank       | zset | `ZADD z:0 ...`| `ZRANK z:0 <rand>`          |
| hget        | hash | `HSET h:0 ...`| `HGET h:0 <rand>`           |

memtier draws a random id in `[0, N)` and substitutes it as `__key__` = `k` + id over
the fixed `--key-prefix=k`, so the probe element is that keyed id plus the same pad
literal the load used, and it lands on a stored element every time. The prefix is a
real character because memtier refuses an empty prefix (it exits with a usage error),
and the loader writes the same `k`-prefixed members so load and probe agree.

The list point read (LINDEX by position) is not a row here for a mechanical reason:
memtier substitutes `__key__` only as prefix + id and refuses an empty prefix, so it
cannot emit the bare integer a LINDEX index needs. LINDEX random access is covered in
the in-memory-fit scenario, whose aki-bench plan generator produces the numeric index
directly.

Each row is one point read, one per type. That is the shape memtier's `__key__`
substitution fits: a single random id dropped into a fixed command. A bounded range or
scan needs a computed window (`start, start+window`) and a single `__key__` can only
drop one random, so `ZRANGE z:0 r1 r2` lands empty
whenever `r1 > r2`; an algebra like `SINTER` needs a second loaded collection,
which doubles the dataset and breaks the single-collection cap math the fairness
rule depends on. Those shapes are exercised in the in-memory-fit scenario, which
drives aki-bench's plan probes; carrying them into the capped LTM regime needs a
probe generator that can compute a window, which is a logged follow-up.

## The fairness rule

The result hinges on how the cap is applied, and the spec is strict about it
(2064/ltm/05 section 1). Every engine **loads with full RAM first**, then the cap
is tightened below the dataset, caches are dropped, and reads are served. This
isolates the serve-time larger-than-memory effect and removes the load-time
swap-thrash artifact that once made a capped Valkey load take about 100 minutes.

- aki loads uncapped to its single `.aki` file, saves, then restarts serving from
  that file under the cap. Its real LTM mechanism is the bounded buffer pool over
  file-backed pages: a point read is an O(log n) descent of clean, reclaimable,
  page-local reads, so resident memory stays near the buffer-pool budget.
- Redis and Valkey load the whole collection into the heap inside an 8 GB scope,
  then the scope is tightened to the cap with `systemctl set-property`. The overflow
  swaps out, and each random read faults scattered heap pages back from swap.

Both rivals report `loaded_rss` (heap size before the cap) and `swap` (how much was
pushed out), so the matrix carries the evidence that the larger-than-memory regime
is real and not a benchmark tilt.

## Running it

Needs root for the cgroup scopes and `drop_caches`, and a quiet Linux box with swap.

```
sudo AKI=/path/to/aki \
     REDIS=redis-server \
     VALKEY=/path/to/valkey-9.1.0/src/valkey-server \
     ./run.sh
```

Knobs (environment variables): `N`, `CAP`, `SWAP`, `PTIME` (seconds per memtier
probe), `CLIENTS`, `THREADS` (memtier client threads), `PIPE`, `REPS`, `BP` (aki
buffer-pool budget), `PORT`. The probe is `memtier_benchmark`, so it must be on
`PATH`. The defaults are the baseline configuration: `N=3000000`, `CAP=300M`,
`SWAP=4096M`, `PIPE=16`, `REPS=2`.

A fits-in-RAM control is just the same script with a small `N` and a large `CAP`
(for example `N=20000 CAP=2G`); there aki loses to both rivals, which proves the
LTM win is the structural effect and not a tilt in aki's favor.

## Baseline result (server2, 6 cores, N=3000000, cap=300M, two reps)

This table predates the memtier switch and was measured with the old redis-benchmark
probe over 255-byte members; `run.sh` now drives the same point reads with memtier over
~247-byte members. The serve-time effect being measured is unchanged, so the ratios
stand as the reference until the next sweep re-records them memtier-native.

aki dev (order-statistic tree merged) against Redis 8.8.0 and Valkey 9.1.0. Every
engine loaded the 3M-element collection with full RAM, then served random point
reads under the 300 MB cap. aki held ~283 MB resident throughout; Redis and Valkey
loaded 1.0 to 1.9 GB and pushed 2.3 to 3.3 GB out to swap.

| command   | aki rps (r1 / r2)   | aki/redis (r1 / r2) | aki/valkey (r1 / r2) | 2x both? |
|-----------|---------------------|---------------------|----------------------|----------|
| SISMEMBER | 14041 / 22589       | 12.8 / 22.2         | 4.3 / 7.8            | yes      |
| ZSCORE    | 20717 / 12736       | 20.8 / 18.5         | 8.2 / 6.0            | yes      |
| HGET      | 13468 / 11647       | 18.7 / 19.1         | 6.7 / 6.7            | yes      |
| ZRANK     | 1879 / 1777         | 2.9 / 3.1           | 1.2 / 1.4           | **no**   |

The three point reads clear the 2x bar against both engines by a wide margin and
reproduce across reps: under a cap the rivals fault scattered heap pages from swap on
every read, while aki does one clean file-backed descent. ZRANK does not clear 2x
against Valkey 9.1. It is two descents, not one (a member-index point lookup to read
the score, then the order-statistic Rank descent on the score row), and under the cap
those two descents touch both halves of the dual-index tree, so the resident working
set and the fault rate are roughly double a single-descent read. Valkey's skiplist
rank stays a single logarithmic walk even when swapped, so it holds up. ZRANK is
therefore a still-slow site under the standing rule and is tracked as the next LTM
optimization; the goal stays open until it clears 2x against both engines.

The `srandmember` set row runs alongside the point reads; the list type has no memtier
row here because LINDEX needs a bare numeric index memtier cannot emit, so its LTM
random-access coverage lives in the in-memory-fit scenario instead.

## Scope

This run is uniform-random. The skew (zipfian) rows the spec also calls for, to show
the hot-tier win, are a separate follow-up and are not measured here; the script
prints that so the coverage gap is explicit rather than silent. The ~247-byte-element
shape is the one where every type keeps its element in a small btree key or in the
value behind a small key; a pathological all-key set of very large members is a
different boundary, recorded in the set LTM notes.
