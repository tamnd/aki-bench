package report

import (
	"bytes"
	"strings"
	"testing"
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
