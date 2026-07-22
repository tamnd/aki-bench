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

// sustained must alternate the re-add and the pop on seq parity so a destructive
// probe never drains its collection: even seq issues the re-add, odd seq issues the
// pop, and the two are balanced one command at a time. It also underpins the gate
// scorer, which zeroes a ratio when every server drains to nil replies, so the
// balance is what keeps a destructive row scorable.
func TestSustainedAlternatesAndBalances(t *testing.T) {
	readd := func(conn int, seq int64) [][]byte { return [][]byte{[]byte("ADD"), refillName(conn, seq)} }
	pop := func(conn int, seq int64) [][]byte { return [][]byte{[]byte("POP")} }
	probe := sustained(readd, pop)

	adds, pops := 0, 0
	names := map[string]int{}
	const n = 10000
	for seq := int64(0); seq < n; seq++ {
		argv := probe(3, seq)
		switch string(argv[0]) {
		case "ADD":
			if seq&1 != 0 {
				t.Fatalf("seq %d is odd but issued a re-add, want a pop", seq)
			}
			adds++
			names[string(argv[1])]++
		case "POP":
			if seq&1 == 0 {
				t.Fatalf("seq %d is even but issued a pop, want a re-add", seq)
			}
			pops++
		default:
			t.Fatalf("seq %d issued %q, want ADD or POP", seq, argv[0])
		}
	}
	// Half the ops repopulate, half consume, so the collection holds its cardinality.
	if adds != n/2 || pops != n/2 {
		t.Fatalf("adds=%d pops=%d, want %d each (balanced add/pop mix)", adds, pops, n/2)
	}
	// Every re-add name is unique, so an add is always a genuine new member and never
	// a colliding no-op that would let pops outrun adds and drain the collection.
	for name, count := range names {
		if count != 1 {
			t.Fatalf("re-add name %q issued %d times, want 1 (unique per seq)", name, count)
		}
	}
}

// refillName must be unique across connections so two connections re-adding at the
// same seq never collide: a collision would make one re-add a silent no-op while both
// connections' pops still removed a member, draining the collection the sustained mix
// exists to hold.
func TestRefillNameUniquePerConnection(t *testing.T) {
	seen := map[string]bool{}
	for conn := 0; conn < 32; conn++ {
		for seq := int64(0); seq < 100; seq++ {
			name := string(refillName(conn, seq))
			if seen[name] {
				t.Fatalf("refillName collision at conn %d seq %d: %q", conn, seq, name)
			}
			seen[name] = true
		}
	}
}
