package load

import "testing"

func TestHistogramPercentiles(t *testing.T) {
	h := NewLatencyHistogram()
	for i := int64(1); i <= 1000; i++ {
		h.RecordValue(i * 1000) // microseconds in nanoseconds
	}
	if h.TotalCount() != 1000 {
		t.Fatalf("count = %d, want 1000", h.TotalCount())
	}
	p50 := h.ValueAtPercentile(50)
	if p50 < 490_000 || p50 > 520_000 {
		t.Fatalf("p50 = %d ns, out of expected band", p50)
	}
	p99 := h.ValueAtPercentile(99)
	if p99 < 980_000 {
		t.Fatalf("p99 = %d ns, lower than expected", p99)
	}
	if h.Max() != 1_000_000 {
		t.Fatalf("max = %d, want 1000000", h.Max())
	}
}

func TestHistogramMerge(t *testing.T) {
	a := NewLatencyHistogram()
	b := NewLatencyHistogram()
	for i := 0; i < 500; i++ {
		a.RecordValue(1000)
		b.RecordValue(2000)
	}
	a.Merge(b)
	if a.TotalCount() != 1000 {
		t.Fatalf("merged count = %d, want 1000", a.TotalCount())
	}
}

func TestRecordCorrectedValueBackfills(t *testing.T) {
	h := NewLatencyHistogram()
	// One operation took 10x the expected interval, so the correction should
	// synthesize the requests a stalled client missed.
	h.RecordCorrectedValue(10_000, 1_000)
	if h.TotalCount() <= 1 {
		t.Fatalf("expected coordinated-omission backfill, got count %d", h.TotalCount())
	}
}
