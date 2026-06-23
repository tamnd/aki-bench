package workload

import (
	"strings"
	"testing"
)

func TestRegistryCoversNames(t *testing.T) {
	reg := Registry()
	for _, n := range Names() {
		if _, ok := reg[n]; !ok {
			t.Fatalf("registry missing workload %q", n)
		}
	}
}

func TestGeneratorsProduceCommands(t *testing.T) {
	spec := Spec{ValueSize: 32, KeyCount: 1000, ReadRatio: 80}
	for _, n := range Names() {
		gen := Build(n, spec)
		if gen == nil {
			t.Fatalf("no generator for %q", n)
		}
		argv := gen(0, 1)
		if len(argv) == 0 {
			t.Fatalf("%s produced empty argv", n)
		}
		cmd := strings.ToUpper(string(argv[0]))
		if cmd == "" {
			t.Fatalf("%s produced empty command name", n)
		}
	}
}

func TestKeysStayInKeyspace(t *testing.T) {
	spec := Spec{ValueSize: 8, KeyCount: 10}
	gen := Set(spec)
	seen := map[string]bool{}
	for seq := int64(0); seq < 100; seq++ {
		argv := gen(0, seq)
		seen[string(argv[1])] = true
	}
	if len(seen) != 10 {
		t.Fatalf("expected 10 distinct keys in a key space of 10, got %d", len(seen))
	}
}

func TestMixedRespectsReadRatio(t *testing.T) {
	gen := Mixed(Spec{ValueSize: 8, KeyCount: 100, ReadRatio: 80})
	reads, writes := 0, 0
	for seq := int64(0); seq < 100; seq++ {
		argv := gen(0, seq)
		if strings.ToUpper(string(argv[0])) == "GET" {
			reads++
		} else {
			writes++
		}
	}
	if reads != 80 || writes != 20 {
		t.Fatalf("ratio off: reads %d writes %d", reads, writes)
	}
}

func TestUnknownWorkload(t *testing.T) {
	if Build("nope", Spec{}) != nil {
		t.Fatal("expected nil for unknown workload")
	}
}
