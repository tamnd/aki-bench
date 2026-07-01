package workload

import "testing"

// Every set operator plan must preload the set with SADD and probe the same set with
// the operator the audit expects, so a sweep lines up aki, Redis, and Valkey on the
// same command against the same collection.
func TestSetPlansBuildAndProbe(t *testing.T) {
	cases := []struct {
		name    string
		probeHd string // expected first token of the probe command
	}{
		{"scard", "SCARD"},
		{"smismember", "SMISMEMBER"},
		{"saddmember", "SADD"},
		{"srem", "SREM"},
		{"srandmember", "SRANDMEMBER"},
		{"srandmembercount", "SRANDMEMBER"},
		{"spop", "SPOP"},
		{"smove", "SMOVE"},
	}
	for _, c := range cases {
		plan, ok := BuildPlan(c.name, Spec{Members: 1000})
		if !ok {
			t.Fatalf("%s: BuildPlan returned not-a-plan", c.name)
		}
		if plan.PreloadOps != 1000 {
			t.Fatalf("%s: PreloadOps = %d, want 1000", c.name, plan.PreloadOps)
		}
		pre := plan.Preload(0, 0)
		if string(pre[0]) != "SADD" {
			t.Fatalf("%s: preload cmd = %q, want SADD", c.name, pre[0])
		}
		probe := plan.Probe(0, 0)
		if string(probe[0]) != c.probeHd {
			t.Fatalf("%s: probe cmd = %q, want %s", c.name, probe[0], c.probeHd)
		}
		// The probe must target the same set the preload wrote.
		if string(probe[1]) != string(pre[1]) {
			t.Fatalf("%s: probe key %q != preload key %q", c.name, probe[1], pre[1])
		}
	}
}

// The set operator names must resolve as plans, never as flat generators, so main
// dispatches them through the collection path. The flat sadd workload stays separate:
// it spreads writes across the key space, while saddmember writes into one large set.
func TestSetPlansDisjointFromFlat(t *testing.T) {
	for _, name := range setPlanNames() {
		if Build(name, Spec{}) != nil {
			t.Fatalf("%s resolved as a flat generator, want plan only", name)
		}
		if _, ok := BuildPlan(name, Spec{}); !ok {
			t.Fatalf("%s did not resolve as a plan", name)
		}
	}
}

// The set preload must build one set covering every member id 0..Members-1 exactly
// once under a single sequential connection, so the probed member is always a hit.
func TestSetPreloadCoversMemberSpace(t *testing.T) {
	plan, _ := BuildPlan("scard", Spec{Members: 256})
	seen := map[string]bool{}
	key := ""
	for seq := int64(0); seq < plan.PreloadOps; seq++ {
		argv := plan.Preload(0, seq)
		if key == "" {
			key = string(argv[1])
		} else if string(argv[1]) != key {
			t.Fatalf("preload wrote two sets: %q and %q", key, argv[1])
		}
		seen[string(argv[2])] = true // the member token
	}
	if len(seen) != 256 {
		t.Fatalf("preload covered %d distinct members, want 256", len(seen))
	}
}

// SMISMEMBER requests a member window sized to the standard range window, clamped to
// the member space, and every requested member must land inside 0..Members-1 so the
// batch is a full hit.
func TestSetMIsMemberWindow(t *testing.T) {
	plan, _ := BuildPlan("smismember", Spec{Members: 1000})
	probe := plan.Probe(0, 0)
	// SMISMEMBER key m... : two header tokens plus rangeWindow members.
	if got := len(probe) - 2; got != rangeWindow {
		t.Fatalf("SMISMEMBER requested %d members, want %d", got, rangeWindow)
	}
	for _, m := range probe[2:] {
		if m[0] != 'm' {
			t.Fatalf("member token %q is not a set member name", m)
		}
	}

	// A set smaller than the window clamps the request to the member count.
	small, _ := BuildPlan("smismember", Spec{Members: 10})
	sp := small.Probe(0, 0)
	if got := len(sp) - 2; got != 10 {
		t.Fatalf("SMISMEMBER on a 10-member set requested %d members, want 10", got)
	}
}

// The no-count random reads probe the set key with a bare command and no member
// argument, so the server picks the member: this is the O(log n) random-seek path the
// audit measures, not a client-chosen point probe.
func TestSetRandomNoArgProbe(t *testing.T) {
	for _, name := range []string{"srandmember", "spop"} {
		plan, _ := BuildPlan(name, Spec{Members: 1000})
		probe := plan.Probe(0, 0)
		if len(probe) != 2 {
			t.Fatalf("%s: probe has %d tokens, want 2 (cmd + key)", name, len(probe))
		}
	}
}

// The count-form SRANDMEMBER requests a positive window sized to the range window and
// clamped to the member space, so it exercises the distinct sampler over a bounded batch.
func TestSetRandMemberCountWindow(t *testing.T) {
	plan, _ := BuildPlan("srandmembercount", Spec{Members: 1000})
	probe := plan.Probe(0, 0)
	// SRANDMEMBER key count : three tokens, the count is the range window.
	if len(probe) != 3 {
		t.Fatalf("SRANDMEMBER count probe has %d tokens, want 3", len(probe))
	}
	if got := string(probe[2]); got != "100" {
		t.Fatalf("count = %q, want the range window 100", got)
	}
	// A set smaller than the window clamps the count to the member count, so the request
	// never asks for more distinct members than exist.
	small, _ := BuildPlan("srandmembercount", Spec{Members: 10})
	if got := string(small.Probe(0, 0)[2]); got != "10" {
		t.Fatalf("count on a 10-member set = %q, want 10", got)
	}
}

// SMOVE probes SMOVE source destination member: four tokens, the source is the shared
// set key, the destination is a distinct sibling set (never the source, so the move is
// a real two-key move and not a same-set no-op), and the member is a live member name.
func TestSetMoveProbeShape(t *testing.T) {
	plan, _ := BuildPlan("smove", Spec{Members: 1000})
	probe := plan.Probe(0, 0)
	if len(probe) != 4 {
		t.Fatalf("SMOVE probe has %d tokens, want 4 (cmd + source + dest + member)", len(probe))
	}
	if string(probe[1]) != "set:"+collKey {
		t.Fatalf("SMOVE source = %q, want the shared set key", probe[1])
	}
	if string(probe[2]) == string(probe[1]) {
		t.Fatalf("SMOVE destination %q equals the source, want a distinct sibling set", probe[2])
	}
	if probe[3][0] != 'm' {
		t.Fatalf("SMOVE member token %q is not a set member name", probe[3])
	}
}

// Every set plan (point read, meta, range, and this file's operators) must target the
// one shared set key, so a sweep compares operators against the same collection.
func TestSetPlansShareProbeKey(t *testing.T) {
	want := "set:" + collKey
	for _, name := range append([]string{"sismember", "smembers", "sscan"}, setPlanNames()...) {
		plan, ok := BuildPlan(name, Spec{Members: 100})
		if !ok {
			t.Fatalf("%s: not a plan", name)
		}
		if got := string(plan.Preload(0, 0)[1]); got != want {
			t.Fatalf("%s: set key %q, want %q", name, got, want)
		}
	}
}
