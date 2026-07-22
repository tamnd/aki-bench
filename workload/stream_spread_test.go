package workload

import "testing"

// The multi-stream consumer-group probe spreads across streamSpreadN distinct
// streams so the load fans across a sharded engine's shards instead of pinning to
// one hot stream. The preload lays out each stream's XGROUP CREATE followed by its
// entries as one sequential pass, and the probe keys by connection so different
// connections read different streams.
func TestXReadGroupNSpreadsStreams(t *testing.T) {
	plan, ok := BuildPlan("xreadgroupn", Spec{})
	if !ok {
		t.Fatal("xreadgroupn: BuildPlan returned not-a-plan")
	}
	perStream := int64(1 + streamSpreadDepth)
	if plan.PreloadOps != int64(streamSpreadN)*perStream {
		t.Fatalf("xreadgroupn: PreloadOps = %d, want %d", plan.PreloadOps, int64(streamSpreadN)*perStream)
	}
	// Stream 0 is created at its first preload op, then filled.
	first := plan.Preload(0, 0)
	if string(first[0]) != "XGROUP" || string(first[len(first)-1]) != "MKSTREAM" {
		t.Fatalf("xreadgroupn: seq 0 preload = %q, want XGROUP CREATE ... MKSTREAM", first)
	}
	if string(first[2]) != "stream:sp:0" {
		t.Fatalf("xreadgroupn: first group on %q, want stream:sp:0", first[2])
	}
	if a := plan.Preload(0, 1); string(a[0]) != "XADD" || string(a[1]) != "stream:sp:0" {
		t.Fatalf("xreadgroupn: seq 1 preload = %q %q, want XADD on stream:sp:0", a[0], a[1])
	}
	// Stream 1's create lands exactly perStream ops later.
	if c := plan.Preload(0, perStream); string(c[0]) != "XGROUP" || string(c[2]) != "stream:sp:1" {
		t.Fatalf("xreadgroupn: seq %d preload = %q, want XGROUP CREATE on stream:sp:1", perStream, c)
	}
	// Two connections in different residue classes read different streams.
	r0 := plan.Probe(0, 1) // odd seq -> the XREADGROUP deliver
	r1 := plan.Probe(1, 1)
	if string(r0[0]) != "XREADGROUP" {
		t.Fatalf("xreadgroupn: odd-seq probe = %q, want XREADGROUP", r0[0])
	}
	sk0, sk1 := string(r0[len(r0)-2]), string(r1[len(r1)-2])
	if sk0 != "stream:sp:0" || sk1 != "stream:sp:1" {
		t.Fatalf("xreadgroupn: conn 0/1 read %q/%q, want stream:sp:0/stream:sp:1", sk0, sk1)
	}
}
