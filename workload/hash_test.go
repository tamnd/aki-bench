package workload

import "testing"

// Every hash operator plan must preload the hash with HSET and probe the same hash
// with the operator the audit expects, so a sweep lines up aki, Redis, and Valkey on
// the same command against the same collection.
func TestHashPlansBuildAndProbe(t *testing.T) {
	cases := []struct {
		name    string
		probeHd string // expected first token of the probe command
	}{
		{"hmget", "HMGET"},
		{"hexists", "HEXISTS"},
		{"hlen", "HLEN"},
		{"hstrlen", "HSTRLEN"},
		{"hkeys", "HKEYS"},
		{"hvals", "HVALS"},
		{"hsetfield", "HSET"},
		{"hsetnx", "HSETNX"},
		{"hdel", "HDEL"},
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
		if string(pre[0]) != "HSET" {
			t.Fatalf("%s: preload cmd = %q, want HSET", c.name, pre[0])
		}
		probe := plan.Probe(0, 0)
		if string(probe[0]) != c.probeHd {
			t.Fatalf("%s: probe cmd = %q, want %s", c.name, probe[0], c.probeHd)
		}
		// The probe must target the same hash the preload wrote.
		if string(probe[1]) != string(pre[1]) {
			t.Fatalf("%s: probe key %q != preload key %q", c.name, probe[1], pre[1])
		}
	}
}

// The hash operator names must resolve as plans, never as flat generators, so main
// dispatches them through the collection path.
func TestHashPlansDisjointFromFlat(t *testing.T) {
	for _, name := range hashPlanNames() {
		if Build(name, Spec{}) != nil {
			t.Fatalf("%s resolved as a flat generator, want plan only", name)
		}
		if _, ok := BuildPlan(name, Spec{}); !ok {
			t.Fatalf("%s did not resolve as a plan", name)
		}
	}
}

// The hash preload must build one hash covering every field id 0..Members-1 exactly
// once under a single sequential connection, so the probed field is always a hit.
func TestHashPreloadCoversFieldSpace(t *testing.T) {
	plan, _ := BuildPlan("hexists", Spec{Members: 256})
	seen := map[string]bool{}
	key := ""
	for seq := int64(0); seq < plan.PreloadOps; seq++ {
		argv := plan.Preload(0, seq)
		if key == "" {
			key = string(argv[1])
		} else if string(argv[1]) != key {
			t.Fatalf("preload wrote two hashes: %q and %q", key, argv[1])
		}
		seen[string(argv[2])] = true // the field token
	}
	if len(seen) != 256 {
		t.Fatalf("preload covered %d distinct fields, want 256", len(seen))
	}
}

// HMGET requests a field window sized to the standard range window, clamped to the
// member space, and every requested field must land inside 0..Members-1 so the batch
// is a full hit.
func TestHashMGetWindow(t *testing.T) {
	plan, _ := BuildPlan("hmget", Spec{Members: 1000})
	probe := plan.Probe(0, 0)
	// HMGET key f... : two header tokens plus rangeWindow fields.
	if got := len(probe) - 2; got != rangeWindow {
		t.Fatalf("HMGET requested %d fields, want %d", got, rangeWindow)
	}
	for _, f := range probe[2:] {
		if f[0] != 'f' {
			t.Fatalf("field token %q is not a hash field name", f)
		}
	}

	// A hash smaller than the window clamps the request to the member count.
	small, _ := BuildPlan("hmget", Spec{Members: 10})
	sp := small.Probe(0, 0)
	if got := len(sp) - 2; got != 10 {
		t.Fatalf("HMGET on a 10-field hash requested %d fields, want 10", got)
	}
}
