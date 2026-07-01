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
		{"lpop", "RPUSH", "LPOP", 1, "list:probe"},
		{"lindex", "RPUSH", "LINDEX", 1, "list:probe"},
		{"zrange", "ZADD", "ZRANGE", 1, "zset:probe"},
		{"zrangebyscore", "ZADD", "ZRANGEBYSCORE", 1, "zset:probe"},
		{"hscan", "HSET", "HSCAN", 1, "hash:probe"},
		{"sscan", "SADD", "SSCAN", 1, "set:probe"},
		{"smembers", "SADD", "SMEMBERS", 1, "set:probe"},
		{"hgetall", "HSET", "HGETALL", 1, "hash:probe"},
		{"xrange", "XADD", "XRANGE", 1, "stream:probe"},
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

// The algebra plans build two half-overlapping sets in one sequential pass and combine
// them. The preload must cover both source keys and every member id, each set must end
// with Members distinct members, and the two must share exactly their half band so every
// algebra form does real work rather than a degenerate empty or identity result. The read
// forms (SINTER/SUNION/SDIFF) probe two sources; SINTERCARD prefixes the numkeys count.
func TestAlgebraPlansBuildTwoSources(t *testing.T) {
	for _, name := range []string{"sinter", "sunion", "sdiff", "sintercard"} {
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
		var setKeys []string
		for key, members := range keys {
			if len(members) != 256 {
				t.Fatalf("%s: set %s has %d members, want 256", name, key, len(members))
			}
			setKeys = append(setKeys, key)
		}
		// The two sets must share exactly half their members (128 of 256), the middle band
		// the shifted preload produces, so SINTER/SINTERCARD count 128, SDIFF returns 128,
		// and SUNION returns 384 rather than a degenerate full or zero overlap.
		shared := 0
		for m := range keys[setKeys[0]] {
			if keys[setKeys[1]][m] {
				shared++
			}
		}
		if shared != 128 {
			t.Fatalf("%s: sets share %d members, want 128 (half overlap)", name, shared)
		}

		probe := plan.Probe(0, 0)
		switch name {
		case "sintercard":
			// SINTERCARD numkeys key key: four tokens, numkeys is 2, two distinct sources.
			if len(probe) != 4 {
				t.Fatalf("%s: probe has %d args, want 4 (cmd + numkeys + two sources)", name, len(probe))
			}
			if string(probe[1]) != "2" {
				t.Fatalf("%s: numkeys = %q, want 2", name, probe[1])
			}
			if string(probe[2]) == string(probe[3]) {
				t.Fatalf("%s: probe references the same source twice (%q)", name, probe[2])
			}
		default:
			if len(probe) != 3 {
				t.Fatalf("%s: probe has %d args, want 3 (cmd + two sources)", name, len(probe))
			}
			if string(probe[1]) == string(probe[2]) {
				t.Fatalf("%s: probe references the same source twice (%q)", name, probe[1])
			}
		}
	}
}

// ZUNIONSTORE builds two equal sorted sets in one pass and unions them into a
// destination with weights. The preload must cover both source zsets and every
// member id, and the probe must name both sources plus the WEIGHTS clause without
// pointing at a source as the destination.
func TestZUnionPlanBuildsTwoSources(t *testing.T) {
	plan, ok := BuildPlan("zunion", Spec{Members: 256})
	if !ok {
		t.Fatal("zunion: BuildPlan returned not-a-plan")
	}
	if plan.PreloadOps != 512 {
		t.Fatalf("zunion: PreloadOps = %d, want 512 (two zsets of 256)", plan.PreloadOps)
	}
	keys := map[string]map[string]bool{}
	for seq := int64(0); seq < plan.PreloadOps; seq++ {
		argv := plan.Preload(0, seq)
		if string(argv[0]) != "ZADD" {
			t.Fatalf("zunion: preload cmd = %q, want ZADD", argv[0])
		}
		key := string(argv[1])
		if keys[key] == nil {
			keys[key] = map[string]bool{}
		}
		keys[key][string(argv[3])] = true // ZADD key score member
	}
	if len(keys) != 2 {
		t.Fatalf("zunion: preload wrote %d distinct zsets, want 2", len(keys))
	}
	for key, members := range keys {
		if len(members) != 256 {
			t.Fatalf("zunion: zset %s has %d members, want 256", key, len(members))
		}
	}
	probe := plan.Probe(0, 0)
	// argv: ZUNIONSTORE dest 2 a b WEIGHTS 1 1
	if string(probe[0]) != "ZUNIONSTORE" {
		t.Fatalf("zunion: probe cmd = %q, want ZUNIONSTORE", probe[0])
	}
	if string(probe[2]) != "2" {
		t.Fatalf("zunion: numkeys = %q, want 2", probe[2])
	}
	src := map[string]bool{string(probe[3]): true, string(probe[4]): true}
	if len(src) != 2 {
		t.Fatalf("zunion: probe names the same source twice (%q, %q)", probe[3], probe[4])
	}
	if src[string(probe[1])] {
		t.Fatalf("zunion: destination %q is also a source", probe[1])
	}
	if string(probe[5]) != "WEIGHTS" {
		t.Fatalf("zunion: expected WEIGHTS clause, got %q", probe[5])
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
