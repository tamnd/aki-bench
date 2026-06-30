# aki-bench

Benchmarks for [aki](https://github.com/tamnd/aki) against Redis and Valkey.

aki is a Redis-wire-compatible single-file database.
Its stated goal is to be at least twice as fast as Redis 7.4 and Valkey on equivalent hardware.
This repository is the harness that proves or disproves that claim, and it ships the exact pass/fail gate the claim is measured against.

It is a tooling repo, so it uses third-party Go deps where they earn their place, but in practice the load generator, the RESP client, and the latency histogram are all native and zero-dependency.

## What it measures

For a named workload against each target (aki, Redis, Valkey) it reports:

- throughput in operations per second
- latency at p50, p99, and p999
- the speedup of aki over each competitor
- a single gate verdict: did aki hit the 2x bar with no tail regression

Two load disciplines are available:

- closed loop: issue the next command as soon as the previous reply lands. This measures service time, the number redis-benchmark prints.
- open loop: issue commands on a fixed schedule at a target rate, with coordinated-omission correction. When the server stalls, the latency a queued request would have seen is reconstructed instead of silently dropped. This measures response time under load and is the honest tail-latency number.

## Workloads

The suite covers every type along three shapes: a point op, a bounded range or scan, and an algebra or aggregate.
That is the mapping the spec methodology requires, so a type's 2x verdict is read across all of its shapes rather than one lucky command.

- String: `get`, `getrange`, `set`, `incr`.
- Hash: `hset`, `hget`, `hscan`, `hgetall`.
- Set: `sadd`, `sismember`, `sscan`, `smembers`, `sinter`, `sunion`.
- Sorted set: `zadd`, `zscore`, `zrange`, `zrangebyscore`, `zrank`, `zunion`.
- List: `lpush`, `rpush`, `lrange`, `lpop`, `lindex`.
- Plus `mset` and a `mixed` read/write workload at a configurable read ratio.

The point-read, range, scan, and algebra workloads run a single-connection preload that fully populates the probed collection before the timed probe.
`getrange` reads a window of a large value, so run it with a large `-value-size` to exercise the larger-than-memory windowed-read path.
`lpop` is destructive, so for a clean populated-pop number size `-members` at or above the op budget; once a list drains, LPOP returns nil on all three servers alike, so the ratio stays fair.

Each workload is parameterized by value size and key count.
The key count can be set large enough that the dataset exceeds RAM, which is the case that exercises aki's larger-than-memory design rather than just its in-memory hot path.
See `workload.ValueSizeSweep` and `workload.KeySweep` for the default sweep points.

## Install and build

```
go build ./...
go build -o aki-bench ./cmd/aki-bench
```

Go 1.26 is required.

## Run

Launch all three servers and run the SET workload for five seconds:

```
aki-bench -workload set -connections 50 -pipeline 1 -duration 5s
```

This needs `aki`, `redis-server`, and `valkey-server` on your PATH.
Any target whose binary is missing is skipped, and the run continues with the rest.

Point at servers you already have running instead of launching them:

```
aki-bench -workload mixed -read-ratio 80 \
  -aki-addr 127.0.0.1:6400 \
  -redis-addr 127.0.0.1:6379 \
  -valkey-addr 127.0.0.1:6380
```

Open-loop run at a fixed rate, emitting JSON for CI:

```
aki-bench -workload get -open-loop -rate 200000 -duration 10s -json results.json
```

The compatibility smoke check instead of a load run:

```
aki-bench -smoke
```

The process exits non-zero when the gate fails or a smoke check fails, so it drops straight into a CI step.

## The 2x gate

The gate is one function, `report.EvaluateGate`, and it is deliberately strict.

It passes only when all of the following hold:

1. aki, Redis, and Valkey were all measured. If any target was skipped the gate fails, because a number that "passes" because a competitor was missing means nothing.
2. aki's throughput is at least the required multiple (2.0 by default, set with `-gate`) of Redis throughput.
3. aki's throughput is at least the same multiple of Valkey throughput.
4. aki's p99 latency is not worse than either Redis or Valkey on the same workload.

The required multiplier, the achieved speedups, and the reason for the verdict are all written into the JSON artifact, so a failing run says exactly why it failed.

## Fairness

A 2x claim only means something if the comparison is honest, so the harness pins each server into a matched configuration.

- In-memory vs in-memory. With `-durable` off (the default) every server runs with persistence disabled: no save points and no append-only file for Redis and Valkey, no fsync-on-commit for aki. This isolates command execution.
- Durable vs durable. With `-durable` on, every server runs an fsync-per-write configuration: `appendonly yes` with `appendfsync always` for Redis and Valkey, durable commits for aki. This is the configuration that proves a fair durable number.

The harness never compares an in-memory aki against a durable Redis or the reverse.
Both sides always run the same persistence posture.
It also pins each launched server to a private port and a fresh data directory so runs do not contaminate each other.

Hardware, kernel, and NUMA effects are out of scope for the harness itself.
Run it on the machine you care about and keep the targets on the same host so the network path is identical for all three.

### Co-located client and CPU partitioning

When the harness launches a server and drives it from the same box, the Go load generator and the server threads fight for the same cores.
On a busy multi-core machine that fight starves the server and understates the ratio: a GET workload that clears 2x when the load runs on separate cores can read as 1.79x co-located, purely because the client stole the server's cores.
The cleanest fix is to run the load generator on a different box through connect mode (`-aki-addr`, `-redis-addr`, `-valkey-addr`).

When only one box is available, `-cpu-split` partitions the cores so the launched server and the load generator never share one.
On Linux it re-execs the harness under `taskset` pinned to a client core set and launches every server pinned to the disjoint server set, the way `redis-benchmark` keeps its load threads off the server.
The split is applied identically to aki, Redis, and Valkey, so the comparison stays fair.
By default the client takes half the machine and the server takes the other half; `-cpu-server` and `-cpu-client` override the two `taskset -c` lists.
Half, not a quarter: the load generator is a Go client that encodes, decodes, and records a histogram per reply, so it is much heavier than redis-benchmark's C threads, and under-provisioning it makes the client the bottleneck and collapses the measured ratio.
On a quiet 6-core box the default 3-core server and 3-core client let aki saturate near 0.9M ops/s, where a 2-core client strangled the same run below 0.65M and dragged a real 2x down to 1.3x.

## Compatibility

This repo ships a small smoke check only: PING, SET, GET, INCR, and EXPIRE round-trips compared across targets.
It is enough to catch a target that is plainly broken before you trust its throughput number.
The deep behavioral compatibility suite lives in a separate repo, tamnd/aki-compat, and is not duplicated here.

## Layout

- `load` native RESP client, closed-loop and open-loop load generator, and the HdrHistogram
- `workload` the standard command mixes and the value-size and key-count sweeps
- `target` launch or connect to aki, Redis, and Valkey, with graceful skip when a binary is absent
- `cpuset` partition the cores between the load generator and the launched server so neither starves the other
- `report` the side-by-side table, the JSON artifact, and the 2x gate
- `smoke` the compatibility smoke check
- `cmd/aki-bench` the CLI that ties it together

## License

BSD 3-Clause. See LICENSE.
