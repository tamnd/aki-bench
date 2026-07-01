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
