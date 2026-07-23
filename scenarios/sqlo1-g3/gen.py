#!/usr/bin/env python3
"""Corpus generator for the sqlo1-g3 disk scenario.

Emits RESP arrays on stdout for redis-cli --pipe, deterministically from
(suite, entries, phase, seed), so every server in a cell receives byte
identical traffic without any intermediate corpus file.

The load phase builds the dataset. The churn phase overwrites or
replaces about half of the entries with fresh values of the same shape,
which is what puts dead records into the store: a disk engine that
compresses only on compaction shows nothing on a pure load, and a churn
free run would flatter the snapshot formats too. Live entry count after
both phases is exactly --entries for every suite.

Value shapes are the cascade lab's (2064/sqlo1 labs/b4/01_cascade):
json bodies (the zstd fall-through shape), timestamps (forpack),
uuid-class high-entropy cores (the worst real shape), and constants
(dictionary shapes). Collections carry the same shapes in their
members and fields.
"""

import argparse
import hashlib
import sys

OUT = sys.stdout.buffer


def resp(*args):
    parts = [b"*%d\r\n" % len(args)]
    for a in args:
        if isinstance(a, str):
            a = a.encode()
        parts.append(b"$%d\r\n%s\r\n" % (len(a), a))
    OUT.write(b"".join(parts))


def json_value(i, size, salt):
    v = b'{"id":"user-%08d","status":"active","note":"' % (i + salt)
    unit = b"event %d ok;" % ((i + salt) % 7)
    while len(v) < size - 2:
        v += unit
    return v[: size - 2] + b'"}'


def ts_value(i, salt):
    return b"%d" % (1700000000000 + i * 137 + salt)


def uuid_value(i, salt, seed):
    h = hashlib.md5(b"%d:%d:%d" % (seed, i, salt)).hexdigest()
    return ("%s-%s-%s-%s-%s" % (h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])).encode()


def const_value(size):
    return b"v" * size


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--suite", required=True)
    p.add_argument("--entries", type=int, required=True)
    p.add_argument("--phase", choices=("load", "churn"), required=True)
    p.add_argument("--seed", type=int, default=42)
    a = p.parse_args()

    n, seed = a.entries, a.seed
    load, churn = a.phase == "load", a.phase == "churn"
    salt = 0 if load else 1

    if a.suite.startswith("str-"):
        shape = a.suite[4:]

        def value(i, s):
            if shape == "json":
                return json_value(i, 950, s)
            if shape == "ts":
                return ts_value(i, s)
            if shape == "uuid":
                return uuid_value(i, s, seed)
            if shape == "const":
                return const_value(950)
            raise SystemExit("unknown string shape %s" % shape)

        for i in range(n):
            if churn and i % 2:
                continue
            resp("SET", b"k:%08d" % i, value(i, salt))

    elif a.suite == "hash":
        # Session-store shape: 16 fields per root, short json values.
        for i in range(n):
            if churn and i % 2:
                continue
            resp("HSET", b"h:%06d" % (i // 16), b"f%02d" % (i % 16), json_value(i, 60, salt))

    elif a.suite == "set":
        # Tag membership, uuid-class members: churn replaces half of them.
        for i in range(n):
            if churn and i % 2:
                continue
            root = b"s:%06d" % (i // 100)
            if churn:
                resp("SREM", root, uuid_value(i, 0, seed))
            resp("SADD", root, uuid_value(i, salt, seed))

    elif a.suite == "zset":
        # Leaderboard: member set is stable, churn moves half the scores.
        for i in range(n):
            if churn and i % 2:
                continue
            score = b"%d" % ((i * 2654435761 + salt * 977) % 1000000)
            resp("ZADD", b"z:%06d" % (i // 100), score, b"m:%08d" % i)

    elif a.suite == "list":
        # Feed entries in 100-element lists; churn rewrites half in place.
        for i in range(n):
            if churn and i % 2:
                continue
            root = b"l:%06d" % (i // 100)
            if load:
                resp("RPUSH", root, json_value(i, 128, salt))
            else:
                resp("LSET", root, b"%d" % (i % 100), json_value(i, 128, salt))

    elif a.suite == "stream":
        # A firehose over 100 streams with explicit deterministic ids;
        # churn appends half as much again and trims back, so the head
        # of every stream dies.
        streams = min(100, max(1, n // 100))
        per = n // streams
        if load:
            for i in range(streams * per):
                st, j = i // per, i % per
                resp(
                    "XADD", b"st:%04d" % st, b"%d-1" % (j + 1),
                    b"u", uuid_value(i, salt, seed),
                    b"t", ts_value(i, salt),
                    b"n", json_value(i, 60, salt),
                )
        else:
            extra = per // 2
            for st in range(streams):
                for j in range(per, per + extra):
                    i = st * per + j
                    resp(
                        "XADD", b"st:%04d" % st, b"%d-1" % (j + 1),
                        b"u", uuid_value(i, salt, seed),
                        b"t", ts_value(i, salt),
                        b"n", json_value(i, 60, salt),
                    )
                resp("XTRIM", b"st:%04d" % st, "MAXLEN", b"%d" % per)

    else:
        raise SystemExit("unknown suite %s" % a.suite)

    OUT.flush()


if __name__ == "__main__":
    main()
