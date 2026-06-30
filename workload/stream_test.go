package workload

import (
	"strings"
	"testing"
)

// XADD is a flat write generator: it must resolve from the Registry, produce an
// XADD against the stream key space with a server-assigned id, and not also
// resolve as a plan.
func TestXAddIsFlatWrite(t *testing.T) {
	gen := Build("xadd", Spec{ValueSize: 32, KeyCount: 1000})
	if gen == nil {
		t.Fatal("xadd did not resolve as a flat generator")
	}
	if _, ok := BuildPlan("xadd", Spec{}); ok {
		t.Fatal("xadd resolved as a plan, want flat only")
	}
	argv := gen(0, 1)
	if string(argv[0]) != "XADD" {
		t.Fatalf("cmd = %q, want XADD", argv[0])
	}
	if !strings.HasPrefix(string(argv[1]), "stream:") {
		t.Fatalf("key = %q, want a stream: key", argv[1])
	}
	if string(argv[2]) != "*" {
		t.Fatalf("id = %q, want * (server-assigned)", argv[2])
	}
}

// XRANGE, XREAD, and XREADGROUP must each request a COUNT-bounded window so their
// cost tracks the window, not the stream length, and each probe must target the
// stream the preload populated.
func TestStreamReadsAreBounded(t *testing.T) {
	cases := []struct {
		name     string
		probeHd  string
		keyIdx   int // argv index of the stream key in the probe
		countIdx int // argv index of the COUNT bound value in the probe
	}{
		{"xrange", "XRANGE", 1, 5},
		{"xread", "XREAD", 4, 2},
		{"xreadgroup", "XREADGROUP", 7, 5},
	}
	for _, c := range cases {
		plan, ok := BuildPlan(c.name, Spec{Members: 1000})
		if !ok {
			t.Fatalf("%s: BuildPlan returned not-a-plan", c.name)
		}
		probe := plan.Probe(0, 0)
		if string(probe[0]) != c.probeHd {
			t.Fatalf("%s: probe cmd = %q, want %s", c.name, probe[0], c.probeHd)
		}
		if string(probe[c.keyIdx]) != "stream:"+collKey {
			t.Fatalf("%s: probe key = %q, want stream:%s", c.name, probe[c.keyIdx], collKey)
		}
		if atoi(t, probe[c.countIdx]) != rangeWindow {
			t.Fatalf("%s: COUNT = %q, want %d", c.name, probe[c.countIdx], rangeWindow)
		}
	}
}

// XREADGROUP needs the consumer group to exist before the stream is read. With a
// single sequential preload connection the only place to create it is seq 0, so
// the preload must issue XGROUP CREATE ... MKSTREAM at seq 0 and XADD afterward,
// and PreloadOps must account for the extra create op.
func TestXReadGroupCreatesGroupFirst(t *testing.T) {
	members := 1000
	plan, ok := BuildPlan("xreadgroup", Spec{Members: members})
	if !ok {
		t.Fatal("xreadgroup: BuildPlan returned not-a-plan")
	}
	if plan.PreloadOps != int64(members)+1 {
		t.Fatalf("xreadgroup: PreloadOps = %d, want %d (Members + one XGROUP CREATE)", plan.PreloadOps, members+1)
	}
	first := plan.Preload(0, 0)
	if string(first[0]) != "XGROUP" || string(first[1]) != "CREATE" {
		t.Fatalf("xreadgroup: seq 0 preload = %q %q, want XGROUP CREATE", first[0], first[1])
	}
	if string(first[len(first)-1]) != "MKSTREAM" {
		t.Fatalf("xreadgroup: seq 0 preload missing MKSTREAM, got %q", first)
	}
	if string(first[2]) != "stream:"+collKey {
		t.Fatalf("xreadgroup: group is created on %q, want stream:%s", first[2], collKey)
	}
	rest := plan.Preload(0, 1)
	if string(rest[0]) != "XADD" {
		t.Fatalf("xreadgroup: seq 1 preload = %q, want XADD", rest[0])
	}
	if string(rest[1]) != "stream:"+collKey {
		t.Fatalf("xreadgroup: XADD targets %q, want stream:%s", rest[1], collKey)
	}
	// The probe must read new messages with the > selector against the group.
	probe := plan.Probe(0, 0)
	if string(probe[1]) != "GROUP" || string(probe[2]) != "g" {
		t.Fatalf("xreadgroup: probe = %q, want GROUP g", probe)
	}
	if string(probe[len(probe)-1]) != ">" {
		t.Fatalf("xreadgroup: probe selector = %q, want > (new messages)", probe[len(probe)-1])
	}
}

// The stream plan names must resolve as plans and must not also resolve as flat
// generators, so main dispatches each exactly once.
func TestStreamPlanAndFlatDisjoint(t *testing.T) {
	for _, name := range streamPlanNames() {
		if _, ok := BuildPlan(name, Spec{}); !ok {
			t.Fatalf("%s did not resolve as a plan", name)
		}
		if Build(name, Spec{}) != nil {
			t.Fatalf("%s resolved as a flat generator, want plan only", name)
		}
	}
}
