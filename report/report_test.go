package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/aki-bench/load"
)

func entry(name string, ops, p99 float64) Entry {
	return Entry{Target: name, Workload: "set", OpsPerSec: ops, P99us: p99, P50us: p99 / 2}
}

func TestGatePassesAtTwoX(t *testing.T) {
	aki := entry("aki", 200000, 50)
	redis := entry("redis", 100000, 60)
	valkey := entry("valkey", 95000, 55)
	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	if !g.Pass {
		t.Fatalf("expected pass, got fail: %s", g.Reason)
	}
	if g.SpeedupVsRedis < 2.0 || g.SpeedupVsValkey < 2.0 {
		t.Fatalf("speedups wrong: redis %.2f valkey %.2f", g.SpeedupVsRedis, g.SpeedupVsValkey)
	}
}

func TestGateFailsBelowTwoX(t *testing.T) {
	aki := entry("aki", 150000, 50)
	redis := entry("redis", 100000, 60)
	valkey := entry("valkey", 95000, 55)
	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	if g.Pass {
		t.Fatal("expected fail, aki is only 1.5x Redis")
	}
	if !strings.Contains(g.Reason, "Redis") {
		t.Fatalf("reason should call out Redis: %s", g.Reason)
	}
}

func TestGateFailsOnTailRegression(t *testing.T) {
	aki := entry("aki", 300000, 200) // fast but worse tail than redis
	redis := entry("redis", 100000, 60)
	valkey := entry("valkey", 95000, 55)
	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	if g.Pass {
		t.Fatal("expected fail on p99 regression")
	}
}

func TestGateFailsWhenTargetSkipped(t *testing.T) {
	aki := entry("aki", 300000, 50)
	redis := Skipped("redis", "set")
	valkey := entry("valkey", 95000, 55)
	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	if g.Pass {
		t.Fatal("gate must not pass when a target was skipped")
	}
}

func TestWriteTableAndJSON(t *testing.T) {
	cmp := NewComparison("set",
		entry("aki", 200000, 50),
		entry("redis", 100000, 60),
		entry("valkey", 95000, 55),
		DefaultRequiredSpeedup,
	)
	var tbuf bytes.Buffer
	cmp.WriteTable(&tbuf)
	if !strings.Contains(tbuf.String(), "speedup vs redis") {
		t.Fatalf("table missing speedup line:\n%s", tbuf.String())
	}
	if !strings.Contains(tbuf.String(), "PASS") {
		t.Fatalf("table missing verdict:\n%s", tbuf.String())
	}

	var jbuf bytes.Buffer
	if err := cmp.WriteJSON(&jbuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jbuf.String(), "\"gate\"") {
		t.Fatalf("json missing gate field:\n%s", jbuf.String())
	}
}

func TestEntryCarriesBandwidthAndProtocol(t *testing.T) {
	res := load.Result{Ops: 100, Elapsed: time.Second, Hist: load.NewLatencyHistogram(),
		BytesRead: 5000, BytesWritten: 7000}
	e := FromResult("aki", "set", res)
	if e.BytesPerSec != 12000 {
		t.Fatalf("bytes_per_sec = %.0f, want 12000", e.BytesPerSec)
	}
	if e.RespVer != 2 {
		t.Fatalf("resp_ver = %d, want 2 (the client never negotiates RESP3)", e.RespVer)
	}
}

func TestWithMemoryDerivesBytesPerKey(t *testing.T) {
	e := entry("aki", 100000, 50).WithMemory(1<<28, 2048000, 1000)
	if e.RSSBytes != 1<<28 || e.UsedMemory != 2048000 {
		t.Fatalf("memory columns lost: rss %d used %d", e.RSSBytes, e.UsedMemory)
	}
	if e.BytesPerKey != 2048 {
		t.Fatalf("bytes_per_key = %.1f, want 2048", e.BytesPerKey)
	}

	// Unmeasured accounting must leave the derived figure empty, never derive
	// it from RSS, whose allocator slack would inflate it.
	e = entry("aki", 100000, 50).WithMemory(1<<28, 0, 1000)
	if e.BytesPerKey != 0 {
		t.Fatalf("bytes_per_key must stay empty without used_memory, got %.1f", e.BytesPerKey)
	}
}

func TestComparisonCarriesCell(t *testing.T) {
	cmp := NewComparison("set",
		entry("aki", 200000, 50),
		entry("redis", 100000, 60),
		entry("valkey", 95000, 55),
		DefaultRequiredSpeedup,
	)
	cmp.Cell = Cell{CardBand: "10k", Keyspace: 10000, ValueSize: 1024,
		Dist: "zipfian", ZipfS: 0.99, Pipeline: 16, Connections: 512}

	var tbuf bytes.Buffer
	cmp.WriteTable(&tbuf)
	if !strings.Contains(tbuf.String(), "card=10k") || !strings.Contains(tbuf.String(), "value=1024B") {
		t.Fatalf("table missing the cell line:\n%s", tbuf.String())
	}

	var jbuf bytes.Buffer
	if err := cmp.WriteJSON(&jbuf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"card_band\": \"10k\"", "\"value_size\": 1024", "\"zipf_s\": 0.99"} {
		if !strings.Contains(jbuf.String(), want) {
			t.Fatalf("json missing %s:\n%s", want, jbuf.String())
		}
	}
}
