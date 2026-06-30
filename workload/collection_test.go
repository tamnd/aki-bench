package workload

import (
	"strings"
	"testing"
)

func argvString(argv [][]byte) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = string(a)
	}
	return strings.Join(parts, " ")
}

// Each collection plan must preload the right number of ops and probe the same
// collection it built, with the probed command name the audit expects.
func TestPlansBuildAndProbe(t *testing.T) {
	cases := []struct {
		name      string
		preloadHd string // expected first token of the preload command
		probeHd   string // expected first token of the probe command
	}{
		{"sismember", "SADD", "SISMEMBER"},
		{"hget", "HSET", "HGET"},
		{"zscore", "ZADD", "ZSCORE"},
		{"zrank", "ZADD", "ZRANK"},
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
		if string(pre[0]) != c.preloadHd {
			t.Fatalf("%s: preload cmd = %q, want %s", c.name, pre[0], c.preloadHd)
		}
		probe := plan.Probe(0, 0)
		if string(probe[0]) != c.probeHd {
			t.Fatalf("%s: probe cmd = %q, want %s", c.name, probe[0], c.probeHd)
		}
		// The probe must target the same key the preload wrote.
		if string(probe[1]) != string(pre[1]) {
			t.Fatalf("%s: probe key %q != preload key %q", c.name, probe[1], pre[1])
		}
	}
}

// A flat workload name must not resolve as a plan, and a plan name must not
// resolve as a flat generator, so main can dispatch on which one matched.
func TestPlanAndFlatDisjoint(t *testing.T) {
	if _, ok := BuildPlan("get", Spec{}); ok {
		t.Fatal("get resolved as a plan, want flat only")
	}
	if Build("sismember", Spec{}) != nil {
		t.Fatal("sismember resolved as a flat generator, want plan only")
	}
}

// The preload must cover every member id 0..Members-1 exactly once when driven by
// a single sequential connection, which is how main runs it.
func TestPreloadCoversMemberSpace(t *testing.T) {
	plan, _ := BuildPlan("sismember", Spec{Members: 256})
	seen := map[string]bool{}
	for seq := int64(0); seq < plan.PreloadOps; seq++ {
		argv := plan.Preload(0, seq)
		seen[string(argv[2])] = true // the member token
	}
	if len(seen) != 256 {
		t.Fatalf("preload covered %d distinct members, want 256 (%s ...)", len(seen), argvString(plan.Preload(0, 0)))
	}
}
