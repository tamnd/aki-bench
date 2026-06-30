package workload

import "testing"

// Every range, scan, and algebra plan must preload the collection it then probes,
// with the probe command the audit expects and the probe targeting a key the
// preload populated.
func TestRangePlansBuildAndProbe(t *testing.T) {
	cases := []struct {
		name      string
		preloadHd string // expected first token of the preload command
		probeHd   string // expected first token of the probe command
		preKeyIdx int    // argv index of the key in the preload command
		probeKey  string // the key the probe must target
	}{
		{"lrange", "RPUSH", "LRANGE", 1, "list:probe"},
		{"zrange", "ZADD", "ZRANGE", 1, "zset:probe"},
		{"zrangebyscore", "ZADD", "ZRANGEBYSCORE", 1, "zset:probe"},
		{"hscan", "HSET", "HSCAN", 1, "hash:probe"},
		{"sscan", "SADD", "SSCAN", 1, "set:probe"},
		{"smembers", "SADD", "SMEMBERS", 1, "set:probe"},
		{"hgetall", "HSET", "HGETALL", 1, "hash:probe"},
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
		if string(probe[1]) != c.probeKey {
			t.Fatalf("%s: probe key = %q, want %s", c.name, probe[1], c.probeKey)
		}
		if string(pre[c.preKeyIdx]) != c.probeKey {
			t.Fatalf("%s: preload key = %q, want %s (probe must hit the populated collection)", c.name, pre[c.preKeyIdx], c.probeKey)
		}
	}
}

// The bounded range probes must request exactly rangeWindow elements and must
// never run past the end of the member space, even for the highest selector slot.
func TestRangeWindowStaysInBounds(t *testing.T) {
	members := int64(1000)
	for _, name := range []string{"lrange", "zrange"} {
		plan, _ := BuildPlan(name, Spec{Members: int(members)})
		// Drive a wide span of sequence values so the selector lands near the end.
		for seq := range members {
			probe := plan.Probe(0, seq)
			start := atoi(t, probe[2])
			stop := atoi(t, probe[3])
			if stop-start+1 != rangeWindow {
				t.Fatalf("%s seq %d: window %d..%d is %d wide, want %d", name, seq, start, stop, stop-start+1, rangeWindow)
			}
			if stop > members-1 {
				t.Fatalf("%s seq %d: stop %d runs past last index %d", name, seq, stop, members-1)
			}
			if start < 0 {
				t.Fatalf("%s seq %d: start %d is negative", name, seq, start)
			}
		}
	}
}

// The scan probes must carry a COUNT bound, which is what makes them the
// bound-not-materialize alternative to the whole-collection reads.
func TestScanProbesAreBounded(t *testing.T) {
	for _, name := range []string{"hscan", "sscan"} {
		plan, _ := BuildPlan(name, Spec{Members: 1000})
		probe := plan.Probe(0, 0)
		// argv: SCAN key cursor COUNT n
		if string(probe[2]) != "0" {
			t.Fatalf("%s: cursor = %q, want 0", name, probe[2])
		}
		if string(probe[3]) != "COUNT" {
			t.Fatalf("%s: expected COUNT bound, got %q", name, probe[3])
		}
		if atoi(t, probe[4]) != rangeWindow {
			t.Fatalf("%s: COUNT = %s, want %d", name, probe[4], rangeWindow)
		}
	}
}

// The algebra plans build two equal sets in one sequential pass and intersect or
// union them. The preload must cover both source keys and every member id, and
// the probe must reference both sources.
func TestAlgebraPlansBuildTwoSources(t *testing.T) {
	for _, name := range []string{"sinter", "sunion"} {
		plan, ok := BuildPlan(name, Spec{Members: 256})
		if !ok {
			t.Fatalf("%s: BuildPlan returned not-a-plan", name)
		}
		if plan.PreloadOps != 512 {
			t.Fatalf("%s: PreloadOps = %d, want 512 (two sets of 256)", name, plan.PreloadOps)
		}
		keys := map[string]map[string]bool{}
		for seq := int64(0); seq < plan.PreloadOps; seq++ {
			argv := plan.Preload(0, seq)
			key := string(argv[1])
			if keys[key] == nil {
				keys[key] = map[string]bool{}
			}
			keys[key][string(argv[2])] = true
		}
		if len(keys) != 2 {
			t.Fatalf("%s: preload wrote %d distinct sets, want 2", name, len(keys))
		}
		for key, members := range keys {
			if len(members) != 256 {
				t.Fatalf("%s: set %s has %d members, want 256", name, key, len(members))
			}
		}
		probe := plan.Probe(0, 0)
		if len(probe) != 3 {
			t.Fatalf("%s: probe has %d args, want 3 (cmd + two sources)", name, len(probe))
		}
		if string(probe[1]) == string(probe[2]) {
			t.Fatalf("%s: probe references the same source twice (%q)", name, probe[1])
		}
	}
}

// Every range, scan, and algebra name must resolve as a plan and must not also
// resolve as a flat generator, so main dispatches each exactly once.
func TestRangePlanAndFlatDisjoint(t *testing.T) {
	for _, name := range rangePlanNames() {
		if _, ok := BuildPlan(name, Spec{}); !ok {
			t.Fatalf("%s did not resolve as a plan", name)
		}
		if Build(name, Spec{}) != nil {
			t.Fatalf("%s resolved as a flat generator, want plan only", name)
		}
	}
}

// atoi parses a decimal command argument for the assertions above.
func atoi(t *testing.T, b []byte) int64 {
	t.Helper()
	var n int64
	neg := false
	for i, c := range b {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			t.Fatalf("not an integer: %q", b)
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}
