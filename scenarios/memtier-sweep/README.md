# memtier-sweep

The canonical in-memory-fit 2x gate for a single machine, driven by
`memtier_benchmark`.

`redis-benchmark` used to be the external cross-check here. memtier replaces it
outright: it does everything redis-benchmark does for this job and adds mixed
`set:get` ratios, key distributions, and arbitrary value sizes in one tool, and
it reports a single `Totals` ops/sec line that the script parses. There is no
reason to keep two load generators, so the suite standardizes on memtier.

## What it runs

One server at a time on `PORT` (default 6399), each in a freshly wiped data dir,
pinned to `SRV_CORES`. memtier drives it from `CLI_CORES`, a disjoint set. The
matrix sweeps:

- set-only, get-only, and mixed `1:1` and `1:10` ratios
- pipeline depth 1 and 16
- value sizes 64 B, 512 B, and 4 KiB

get-only cells preload the keyspace at their value size first, so a read
measures a value fetch and not a miss. Every cell brings its server fully down
and frees the port before the next one starts, so a dying server never bleeds
into the next measurement.

The last column is the gate: f2srv over the faster of Redis and Valkey. A cell
passes only when that ratio is at least 2.0.

## Run it

Needs `redis-server`, `valkey-server`, and `f2srv` in `BIN`, plus
`memtier_benchmark` on `PATH` (or set `MT` to its absolute path). Pick core sets
that fit your box; the defaults assume a 32-core machine.

```
SRV_CORES=0-7 CLI_CORES=8-31 BIN=/root/bin ./run.sh
```

Quick single-target check:

```
TARGETS=f2srv MT_TIME=5 ./run.sh
```

## Knobs

- `SRV_CORES`, `CLI_CORES`: disjoint `taskset -c` lists. The client gets the
  larger share on purpose; a starved client caps every target at the client
  ceiling and reads a real win as a tie.
- `MT_CONN`: memtier connection shape, default `--threads 8 --clients 25` (200
  connections).
- `MT_TIME`: seconds per cell. `KEYMAX`: key id space. `GOGC`: f2srv GOGC.
- `TARGETS`: subset of `redis valkey f2srv` to run.

## Result

See `results/gamingpc-2026-07-09/` for a full run: the gate clears 6-11x on every
cell, across all ratios, both pipeline depths, and all three value sizes.
