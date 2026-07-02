package workload

import "testing"

// Every list operator plan must preload the list with RPUSH and probe the same list
// with the operator the audit expects, so a sweep lines up aki, Redis, and Valkey on
// the same command against the same collection. rpoplpush is the exception on the key
// check: its probe names the source list as the first key the same way, so the shared
// assertion still holds.
func TestListPlansBuildAndProbe(t *testing.T) {
	cases := []struct {
		name    string
		probeHd string // expected first token of the probe command
	}{
		{"llen", "LLEN"},
		{"lset", "LSET"},
		{"lpos", "LPOS"},
		{"linsert", "LINSERT"},
		{"lrem", "LREM"},
		{"rpoplpush", "RPOPLPUSH"},
		{"rpushtail", "RPUSH"},
		{"lpushhead", "LPUSH"},
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
		if string(pre[0]) != "RPUSH" {
			t.Fatalf("%s: preload cmd = %q, want RPUSH", c.name, pre[0])
		}
		probe := plan.Probe(0, 0)
		if string(probe[0]) != c.probeHd {
			t.Fatalf("%s: probe cmd = %q, want %s", c.name, probe[0], c.probeHd)
		}
		// The probe must target the same list the preload wrote.
		if string(probe[1]) != string(pre[1]) {
			t.Fatalf("%s: probe key %q != preload key %q", c.name, probe[1], pre[1])
		}
	}
}

// The list operator names must resolve as plans, never as flat generators, so main
// dispatches them through the collection path. The flat lpush/rpush workloads stay
// separate: they spread pushes across the key space, while rpushtail and lpushhead
// push into one large list.
func TestListPlansDisjointFromFlat(t *testing.T) {
	for _, name := range listPlanNames() {
		if Build(name, Spec{}) != nil {
			t.Fatalf("%s resolved as a flat generator, want plan only", name)
		}
		if _, ok := BuildPlan(name, Spec{}); !ok {
			t.Fatalf("%s did not resolve as a plan", name)
		}
	}
}

// The list preload must build one list covering every element id 0..Members-1 exactly
// once under a single sequential connection, so an index or value probe is always a hit.
func TestListPreloadCoversEveryElement(t *testing.T) {
	plan, ok := BuildPlan("llen", Spec{Members: 5})
	if !ok {
		t.Fatal("llen: BuildPlan returned not-a-plan")
	}
	seen := map[string]bool{}
	for seq := int64(0); seq < plan.PreloadOps; seq++ {
		cmd := plan.Preload(0, seq)
		seen[string(cmd[2])] = true
	}
	for i := range int64(5) {
		if !seen[string(memberName(i))] {
			t.Fatalf("preload missed element %s", memberName(i))
		}
	}
}
