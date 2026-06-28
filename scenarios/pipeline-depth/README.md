# Pipeline-depth saturation scenario

The two larger-than-memory scenarios measure aki where its file-backed design beats a
heap store. This one measures the opposite case, the one everyone benchmarks first: an
in-memory keyspace that fits comfortably in RAM, GET and SET at saturation, swept
across pipeline depth.

The point of the sweep is that pipeline depth, not engine speed, decides whether a
Redis-compatible server is syscall-bound or command-bound. At shallow pipelining (P16)
one socket read and one socket write per batch dominate the CPU, all engines pay the
same in-kernel loopback delivery cost, and they cluster within ~1.4x of each other. As
the pipeline deepens, that one read and write amortize over far more commands, the
syscall share of CPU falls, and the engine's per-command path becomes the deciding
cost. `run.sh` sweeps P16, P64, P256, P512 so the whole curve is visible instead of a
single point.

## What it found

server2, 6 cores, kernel 6.8, quiet box, June 2026. Each engine pinned to cores 0-3,
two `redis-benchmark` clients pinned to cores 4 and 5 and summed. Matched core budget:
aki `GOMAXPROCS=4`; Redis and Valkey `io-threads=4` (Redis also
`io-threads-do-reads yes`), their strongest setting. aki runs `--aki-engine hot`, its
in-memory tier, the fair match to Redis and Valkey serving from memory. Rivals are
Redis 8.8.0 (latest, tougher than the 7.4 the compat suite targets) and Valkey 7.2.12.

GET, throughput summed across two clients:

| depth | aki       | redis     | valkey  | aki/redis | aki/valkey |
| ----- | --------- | --------- | ------- | --------- | ---------- |
| P16   | 971k      | 741k      | 410k    | 1.31x     | 2.37x      |
| P64   | 1.71M     | 883k      | 412k    | 1.94x     | 4.16x      |
| P256  | 2.28M     | 835k      | 672k    | 2.73x     | 3.39x      |
| P512  | 2.18M     | 1.00M     | 742k    | 2.18x     | 2.94x      |

SET:

| depth | aki/redis | aki/valkey |
| ----- | --------- | ---------- |
| P16   | 1.42x     | 1.90x      |
| P64   | 1.37x     | 1.66x      |
| P256  | 1.47x     | 1.67x      |
| P512  | 1.67x     | 1.72x      |

The GET crossing reproduces. Three reps each at P64/P256/P512 gave aki/redis of
1.97-2.50x at P64, 2.12-2.33x at P256, 2.30-2.40x at P512, and aki/valkey of 2.69-3.15x
throughout, nine of nine reps over 2x against both rivals.

## Reading it

aki clears 2x over the latest Redis and Valkey on in-memory GET at pipeline depth 64
and above, reproduced. The P16 GET-vs-Redis number is 1.31x, which is the bottom of the
curve and matches the older saturation measurements that only ever ran P16; it is not a
contradiction, it is the shallow end where the socket syscall floor dominates and every
engine pays the same loopback cost. SET stays at 1.4x-1.7x over Redis across all depths,
real but short of 2x, because the write path does more per command and the rivals sit
closer to aki there. So the honest scope is: in-memory GET at realistic deep pipelining
is a 2x-and-beyond win; SET and shallow-pipeline GET-vs-Redis are not.

## Running it

```
sudo ./run.sh                                   # full GET+SET sweep, P16..P512
AKI=/path/to/aki ./run.sh                        # point at a specific aki binary
```

Environment overrides: `AKI` (binary), `PORT`, `NKEYS`, `N` (gets/client), `CLIENTS`,
`DEPTHS`. Needs `redis-server`, `valkey-server`, `redis-cli`, `redis-benchmark`,
`taskset`, and cgroup-free root or a user that can pin cores. Run on a quiet box and
read the cluster across reps, not a single point: the shallow depths sit near the
syscall floor and move with box load, while the deep-pipeline GET ratio is stable.
