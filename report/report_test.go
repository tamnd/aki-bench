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

// genEntry builds a synthetic closed-loop row: a target at the given
// throughput with the given minimum latency in microseconds.
func genEntry(name string, ops, minUs float64) Entry {
	e := entry(name, ops, minUs*4)
	e.MinUs = minUs
	return e
}

func TestDetectGeneratorBound(t *testing.T) {
	// The f3 M0 gate shape: P16 c512, so 8192 outstanding requests. All three
	// targets tie at ~2.1M ops/s and every minimum latency is exactly what the
	// closed-loop identity predicts, outstanding/throughput. This is the
	// redis-benchmark row that was quoted as capacity while the same server
	// did 4.21M under a faster generator.
	outstanding := 512 * 16
	identityMin := func(ops float64) float64 { return float64(outstanding) / ops * 1e6 }

	aki := genEntry("aki", 2.13e6, identityMin(2.13e6))
	redis := genEntry("redis", 2.05e6, identityMin(2.05e6))
	valkey := genEntry("valkey", 2.10e6, identityMin(2.10e6))

	bound, note := DetectGeneratorBound(aki, redis, valkey, outstanding, DefaultGeneratorBoundEpsilon)
	if !bound {
		t.Fatal("a three-way tie satisfying the closed-loop identity must flag generator-bound")
	}
	if !strings.Contains(note, "8192") {
		t.Fatalf("note should carry the outstanding count: %s", note)
	}
}

func TestDetectGeneratorBoundServerBoundSpread(t *testing.T) {
	// A genuine capacity row: aki at 4.21M while Redis and Valkey sit near
	// 350k. The spread alone clears it, whatever the latencies say.
	outstanding := 512 * 16
	aki := genEntry("aki", 4.21e6, 100)
	redis := genEntry("redis", 3.5e5, 120)
	valkey := genEntry("valkey", 3.6e5, 115)
	if bound, _ := DetectGeneratorBound(aki, redis, valkey, outstanding, DefaultGeneratorBoundEpsilon); bound {
		t.Fatal("a 12x spread is a server-bound row, not a generator-bound one")
	}
}

func TestDetectGeneratorBoundIdentityBroken(t *testing.T) {
	// Three targets that happen to tie but whose minimum latencies are real
	// service times far below outstanding/throughput. A coincidental tie with
	// the identity broken must not be flagged: min*ops here is ~18, nowhere
	// near the 8192 outstanding.
	outstanding := 512 * 16
	aki := genEntry("aki", 3.5e5, 50)
	redis := genEntry("redis", 3.5e5, 52)
	valkey := genEntry("valkey", 3.4e5, 51)
	if bound, _ := DetectGeneratorBound(aki, redis, valkey, outstanding, DefaultGeneratorBoundEpsilon); bound {
		t.Fatal("a tie without the closed-loop identity must not flag generator-bound")
	}
}

func TestDetectGeneratorBoundNeedsAllTargets(t *testing.T) {
	outstanding := 512 * 16
	aki := genEntry("aki", 2.1e6, float64(outstanding)/2.1e6*1e6)
	valkey := genEntry("valkey", 2.1e6, float64(outstanding)/2.1e6*1e6)
	if bound, _ := DetectGeneratorBound(aki, Skipped("redis", "set"), valkey, outstanding, DefaultGeneratorBoundEpsilon); bound {
		t.Fatal("a skipped target leaves the tie unproven, must not flag")
	}
	// Zero min latency means the identity cannot be tested, so no flag either.
	if bound, _ := DetectGeneratorBound(entry("aki", 2.1e6, 50), entry("redis", 2.1e6, 50), entry("valkey", 2.1e6, 50), outstanding, DefaultGeneratorBoundEpsilon); bound {
		t.Fatal("entries without a min latency cannot prove the identity, must not flag")
	}
}

func TestFlagGeneratorBoundRendersMarker(t *testing.T) {
	outstanding := 512 * 16
	identityMin := func(ops float64) float64 { return float64(outstanding) / ops * 1e6 }
	cmp := NewComparison("set",
		genEntry("aki", 2.13e6, identityMin(2.13e6)),
		genEntry("redis", 2.05e6, identityMin(2.05e6)),
		genEntry("valkey", 2.10e6, identityMin(2.10e6)),
		DefaultRequiredSpeedup,
	)
	cmp.Cell = Cell{Keyspace: 100000, ValueSize: 64, Dist: "uniform", Pipeline: 16, Connections: 512}
	cmp.FlagGeneratorBound(DefaultGeneratorBoundEpsilon)
	if !cmp.GeneratorBound {
		t.Fatal("comparison should be flagged generator-bound")
	}

	var tbuf bytes.Buffer
	cmp.WriteTable(&tbuf)
	if !strings.Contains(tbuf.String(), "GENERATOR-BOUND") {
		t.Fatalf("table missing the generator-bound marker:\n%s", tbuf.String())
	}

	var jbuf bytes.Buffer
	if err := cmp.WriteJSON(&jbuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jbuf.String(), "\"generator_bound\": true") {
		t.Fatalf("json missing generator_bound:\n%s", jbuf.String())
	}
}

// valueEntry builds a row the way FromResult does after the counter split:
// raw ops/s plus a value-bearing figure derived from a hit ratio.
func valueEntry(name string, ops float64, hit float64, p99 float64) Entry {
	e := entry(name, ops, p99)
	total := int64(ops) // one second's worth, good enough for ratios
	e.NilReplies = int64(float64(total) * (1 - hit))
	e.ValueOpsPerSec = ops * hit
	e.HitRatio = hit
	return e
}

func TestGateComparesValueBearingThroughput(t *testing.T) {
	// The f3 M0 LTM shape: the rival posts a huge raw ops/s but two thirds of
	// its replies are nils for evicted keys, while aki answers everything with
	// data. Raw ops/s says aki lost 0.01x; value-bearing ops/s is the honest
	// comparison and the gate must use it.
	aki := valueEntry("aki", 40000, 1.0, 50)
	redis := valueEntry("redis", 4850000, 0.30, 60)
	valkey := valueEntry("valkey", 4150000, 0.30, 55)

	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	wantVsRedis := 40000.0 / (4850000 * 0.30)
	if diff := g.SpeedupVsRedis - wantVsRedis; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("speedup vs redis = %f, want value-bearing ratio %f", g.SpeedupVsRedis, wantVsRedis)
	}
	if g.Pass {
		t.Fatalf("0.027x is still a fail: %s", g.Reason)
	}
}

func TestGateRivalWithZeroValueOps(t *testing.T) {
	// A rival that answered every GET with a nil served nothing. The gate must
	// not divide by zero (Inf cannot marshal to JSON) and must not credit the
	// nils; aki clears the bar over that rival by default.
	aki := valueEntry("aki", 200000, 1.0, 50)
	redis := valueEntry("redis", 5000000, 0.0, 60)
	valkey := valueEntry("valkey", 90000, 1.0, 55)

	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	if !g.Pass {
		t.Fatalf("expected pass, got: %s", g.Reason)
	}
	if g.SpeedupVsRedis != 0 {
		t.Fatalf("speedup vs an all-nil rival must stay 0, not Inf: %f", g.SpeedupVsRedis)
	}

	var jbuf bytes.Buffer
	cmp := NewComparison("get", aki, redis, valkey, DefaultRequiredSpeedup)
	if err := cmp.WriteJSON(&jbuf); err != nil {
		t.Fatalf("json must marshal cleanly with a zero-value-ops rival: %v", err)
	}
}

func TestGateFailsWhenAkiServesOnlyNils(t *testing.T) {
	aki := valueEntry("aki", 200000, 0.0, 50)
	redis := valueEntry("redis", 100000, 1.0, 60)
	valkey := valueEntry("valkey", 95000, 1.0, 55)
	if g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup); g.Pass {
		t.Fatalf("aki with zero value-bearing ops must fail: %s", g.Reason)
	}
}

func TestGateLegacyEntriesFallBackToRawOps(t *testing.T) {
	// Entries with no reply split (old JSON, hand-built rows) must gate exactly
	// as before on raw ops/s.
	aki := entry("aki", 200000, 50)
	redis := entry("redis", 100000, 60)
	valkey := entry("valkey", 95000, 55)
	g := EvaluateGate(aki, redis, valkey, DefaultRequiredSpeedup)
	if !g.Pass || g.SpeedupVsRedis != 2.0 {
		t.Fatalf("legacy entries must gate on ops/s: pass=%v speedup=%f", g.Pass, g.SpeedupVsRedis)
	}
}

func TestFromResultCarriesReplySplit(t *testing.T) {
	res := load.Result{Ops: 400, NilReplies: 100, ErrReplies: 100,
		Elapsed: time.Second, Hist: load.NewLatencyHistogram()}
	e := FromResult("redis", "get", res)
	if e.NilReplies != 100 || e.ErrReplies != 100 {
		t.Fatalf("reply split lost: nil %d err %d", e.NilReplies, e.ErrReplies)
	}
	if e.HitRatio != 0.5 {
		t.Fatalf("hit_ratio = %f, want 0.5", e.HitRatio)
	}
	if e.ValueOpsPerSec != 200 {
		t.Fatalf("value_ops_per_sec = %f, want 200", e.ValueOpsPerSec)
	}

	// The four fields must always appear in the JSON, even at zero, because
	// their absence is what hid the M0 LTM nil-serving in the first place.
	cmp := NewComparison("get", e, e, e, DefaultRequiredSpeedup)
	var jbuf bytes.Buffer
	if err := cmp.WriteJSON(&jbuf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"nil_replies\"", "\"err_replies\"", "\"hit_ratio\"", "\"value_ops_per_sec\""} {
		if !strings.Contains(jbuf.String(), want) {
			t.Fatalf("json missing %s:\n%s", want, jbuf.String())
		}
	}
}

func TestWriteTableShowsValueColumnsAndNotes(t *testing.T) {
	aki := valueEntry("aki", 40000, 1.0, 50)
	redis := valueEntry("redis", 4850000, 0.30, 60)
	valkey := valueEntry("valkey", 4150000, 0.30, 55)
	cmp := NewComparison("get", aki, redis, valkey, DefaultRequiredSpeedup)

	var tbuf bytes.Buffer
	cmp.WriteTable(&tbuf)
	out := tbuf.String()
	if !strings.Contains(out, "vops/sec") || !strings.Contains(out, "hit%") {
		t.Fatalf("table missing the value columns:\n%s", out)
	}
	if !strings.Contains(out, "nil") {
		t.Fatalf("table note should call out the nil replies:\n%s", out)
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
