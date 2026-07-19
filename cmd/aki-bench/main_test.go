package main

import (
	"testing"

	"github.com/tamnd/aki-bench/workload"
)

func TestSplitWorkloads(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"set", []string{"set"}},
		{"sinter, sunion ,sdiff", []string{"sinter", "sunion", "sdiff"}},
		{" , ,get, ", []string{"get"}},
		{"", nil},
	}
	for _, c := range cases {
		got := splitWorkloads(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitWorkloads(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitWorkloads(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestPlanGroupsAlgebraSuite is the core of preload reuse: the seven set-algebra
// forms must collapse into one preload group so the two source sets are built once,
// with the group's PreloadOps matching a single source-build pass.
func TestPlanGroupsAlgebraSuite(t *testing.T) {
	spec := workload.Spec{Members: 1000, ValueSize: 64, KeyCount: 1000}
	names := []string{"sinter", "sunion", "sdiff", "sintercard", "sinterstore", "sunionstore", "sdiffstore"}
	groups, err := planGroups(names, spec)
	if err != nil {
		t.Fatalf("planGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("algebra suite made %d groups, want 1 (shared preload)", len(groups))
	}
	g := groups[0]
	if len(g.probes) != len(names) {
		t.Fatalf("group has %d probes, want %d", len(g.probes), len(names))
	}
	if g.preload == nil {
		t.Fatal("algebra group has no preload")
	}
	if want := int64(spec.Members) * 2; g.preloadOps != want {
		t.Fatalf("preloadOps = %d, want %d (both sources in one pass)", g.preloadOps, want)
	}
	// Probe order matches the requested order so the reads run before the stores.
	for i, n := range names {
		if g.probes[i].name != n {
			t.Fatalf("probe[%d] = %q, want %q", i, g.probes[i].name, n)
		}
	}
}

// TestPlanGroupsGrouping checks that algebra forms stay in one group even when other
// workloads are interleaved, that a flat read workload gets its own preload, and that
// a flat write workload gets a group with no preload.
func TestPlanGroupsGrouping(t *testing.T) {
	spec := workload.Spec{Members: 100, ValueSize: 64, KeyCount: 100}

	groups, err := planGroups([]string{"sinter", "get", "sunion", "set"}, spec)
	if err != nil {
		t.Fatalf("planGroups: %v", err)
	}
	// sinter opens the algebra group; get and set each stand alone; sunion joins the
	// algebra group already opened by sinter. So: [algebra{sinter,sunion}, get, set].
	if len(groups) != 3 {
		t.Fatalf("made %d groups, want 3", len(groups))
	}
	algebra := groups[0]
	if len(algebra.probes) != 2 || algebra.probes[0].name != "sinter" || algebra.probes[1].name != "sunion" {
		t.Fatalf("algebra group probes = %+v, want sinter,sunion", algebra.probes)
	}
	get := groups[1]
	if len(get.probes) != 1 || get.probes[0].name != "get" || get.preload == nil {
		t.Fatalf("get group = %+v, want one get probe with a preload", get)
	}
	set := groups[2]
	if len(set.probes) != 1 || set.probes[0].name != "set" || set.preload != nil {
		t.Fatalf("set group = %+v, want one set probe with no preload", set)
	}
}

func TestPlanGroupsUnknown(t *testing.T) {
	if _, err := planGroups([]string{"set", "definitelynotaworkload"}, workload.Spec{}); err == nil {
		t.Fatal("planGroups accepted an unknown workload, want error")
	}
}

func TestSuffixJSONPath(t *testing.T) {
	cases := []struct{ path, name, want string }{
		{"out.json", "sinter", "out-sinter.json"},
		{"results/run.json", "sunion", "results/run-sunion.json"},
		{"noext", "sdiff", "noext-sdiff"},
	}
	for _, c := range cases {
		if got := suffixJSONPath(c.path, c.name); got != c.want {
			t.Fatalf("suffixJSONPath(%q, %q) = %q, want %q", c.path, c.name, got, c.want)
		}
	}
}
