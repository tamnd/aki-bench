# In-memory-fit baseline: all-type, vs Redis 8.8 and Valkey 9.1

This is the recorded in-memory-fit half of the M0 baseline for the f1_rewrite_ltm milestone (spec 2064/f1_rewrite_ltm/07, the dual-regime harness).
The larger-than-memory half is measured separately (it needs root on Linux) and is not in this file yet.
It captures where the f1raw product engine stands today against Redis 8.8.0 and Valkey 9.1.0, per type and per command shape, so later milestones have a fixed line to move.
The raw run logs live under `results/` (gitignored); the distilled numbers below are the committed record.
It is a baseline, not a victory lap: it records the starting numbers honestly, including the cells that are below the 2x bar and the two cells that are outright slower.

The gate is 2.0x over both competitors at once.
A cell only passes if aki beats Redis by 2x and Valkey by 2x in the same run.
Beating one but not the other is a fail, and the harness prints which competitor pinned the verdict.

## What ran

Two boxes, because neither alone gives a clean read of both regimes.

The Mac (Apple Silicon, quiet, no other load) is the cleaner box for the in-memory-fit regime.
Redis and Valkey run native on it, f1raw runs through its own server binary f1srv, and nothing else competes for cores.
This is the fair CPU-and-wire read.

Server2 (Linux x86, 6 cores) is the native platform for the competitors and is where the larger-than-memory regime will be measured, since LTM needs cgroup scopes and a page-cache drop that only exist under root on Linux.
The catch is that server2 was under real load during this run (load average 5 to 9 from other jobs), so its absolute numbers are depressed and noisy.
It is included for coverage and cross-check, not as the clean in-memory-fit read.

Both runs use the same shapes per type: a point op, a bounded range or scan, and an algebra or aggregate, plus the type's point write.
Each cell runs at pipeline depth 1 (the per-op latency floor) and 16 (throughput), under uniform and zipfian key distributions.

## In-memory-fit, Mac (quiet, the fair CPU/wire read)

Point ops clear the bar at P16 and miss it at P1.
That split is the whole story of this regime: at depth 16 the throughput win is real and repeatable, at depth 1 the Go netpoller per-syscall floor caps aki around 1.1 to 1.7x (task #182, the reactor/io_uring net path is the fix).

Strings, f1raw:

| cmd | P1 uniform | P16 uniform | P1 zipfian | P16 zipfian |
|-----|-----------|-------------|-----------|-------------|
| SET | 1.37x fail | 2.65x PASS | 1.48x fail | 2.44x PASS |
| GET | 1.32x fail | 2.19x PASS | 1.18x fail | 2.12x PASS |
| INCR | 1.27x fail | 1.90x fail | 1.16x fail | 2.08x PASS |
| GETRANGE | 1.09x fail | 1.30x fail | 1.18x fail | 2.14x (valkey) fail |

Hash, f1raw:

| cmd | P1 uniform | P16 uniform | P1 zipfian | P16 zipfian |
|-----|-----------|-------------|-----------|-------------|
| HSET | 1.49x fail | 3.85x PASS | 1.65x fail | 3.13x PASS |
| HGET | 1.52x fail | 2.21x PASS | 1.49x fail | 2.29x PASS |
| HSCAN | 1.97x fail | 2.06x (valkey) fail | 2.17x fail | 1.94x fail |
| HGETALL | 1.04x fail | 0.47x fail | 1.00x fail | (huge-reply, see below) |

Read: the core point path (SET, GET, HSET, HGET, and INCR under zipfian) clears 2x over both competitors at P16 on a quiet box.
That is the headline the port set out to reach for point reads and writes, and it holds on the fair box.

Two known misses, both tracked:
- HGETALL is slow and below 1x. This is the value-carrying enumeration gap (task #190). Note the workload is a 2M-field HGETALL, which returns four million bulk strings per call, so it is a stress shape, not the common case, but the enumeration path still needs the value-carrying cursor before it competes.
- Every P1 cell fails. This is the netpoller latency floor (#182), not a data-model problem, and it is the same floor the string work already named.

Set and zset rows on the Mac were still filling in when this baseline was recorded and will be appended to `results/in-memory-fit-mac-2026-07-01.log`.
The server2 numbers below cover those types in the meantime.

## In-memory-fit, server2 (Linux, loaded box, coverage cross-check)

Full hash, set, and zset collection gate. Speedups over Redis and Valkey, P16 unless noted.
Absolute throughput here is depressed by the box load, so treat these as relative coverage, not clean maxima.

Clears 2x over both:
- ZSCORE uniform P16: 2.53x Redis, 2.01x Valkey. The one clean pass in this run.

Close, pinned by one competitor (1.9x band):
- HGET uniform P16 1.94x Redis; SISMEMBER uniform P16 2.06x Redis but 1.67x Valkey; SADD zipfian P16 1.95x Redis; SMISMEMBER zipfian P1 1.98x Valkey; ZMSCORE zipfian P16 1.99x Redis; ZSCORE zipfian P16 1.83x Valkey.

Broad middle (1.2 to 1.8x): HSET, HMGET, ZADD, ZRANK, ZMSCORE, most of set point ops.

Below 1x, tracked regressions:
- SPOP P16 collapses to 0.15x Redis / 0.59x Valkey (task #200, pipeline serialization).
- ZRANGE and ZRANK dip under 1x in a few zipfian cells, range-read path.

## Honest verdict

In-memory-fit is the regime Redis and Valkey were built for and it is the hard one for aki.
The baseline says: the core point path (GET/SET/HGET/HSET and INCR) does reach 2x at throughput depth on a quiet box, which is the design target landing for those commands.
It does not yet hold universally.
Three named gaps keep the rest of the matrix under the bar, and each has a task: the P1 latency floor (#182), value-carrying enumeration for HGETALL/HVALS (#190), and the SPOP pipeline collapse (#200).
On a loaded box the whole matrix compresses toward 1x, which is expected and is why the clean read is the quiet Mac.

The larger-than-memory regime is aki's actual selling point and is not in this file.
It requires a quiet Linux box with root for cgroup memory scopes and a page-cache drop between runs, and it must be reproduced against both Redis 8.8 and Valkey 9.1 before any 2x claim is made for it.
That run is the next half of M0 and is pending a clean server window.
No LTM 2x is claimed here.

## Reproduce

In-memory-fit, from the aki-bench repo:

```
scenarios/in-memory-fit/run.sh
```

with `AKI_BIN` pointing at the legacy aki binary (list/stream, btree), `F1SRV_BIN` at the f1raw server (string/hash/set/zset), and `REDIS_BIN` / `VALKEY_BIN` at Redis 8.8 and Valkey 9.1.
Single cells:

```
aki-bench -workload hget -aki-engine f1raw -members 2000000 -pipeline 16 -dist uniform \
  -duration 10s -gate 2.0 -f1srv-bin f1srv -redis-bin redis-server -valkey-bin valkey-server
```
