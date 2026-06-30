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

// GETRANGE must request a rangeWindow-wide window that always sits inside the
// value, for both a large value (a real window) and a value smaller than the
// window (clamped to the value, never a negative or inverted range).
func TestGetRangeWindowStaysInValue(t *testing.T) {
	for _, valueSize := range []int{8, 64, 4096} {
		gen := GetRange(Spec{ValueSize: valueSize, KeyCount: 1000})
		for seq := int64(0); seq < 1000; seq++ {
			argv := gen(0, seq)
			if string(argv[0]) != "GETRANGE" {
				t.Fatalf("cmd = %q, want GETRANGE", argv[0])
			}
			start := atoi(t, argv[2])
			end := atoi(t, argv[3])
			if start < 0 {
				t.Fatalf("value %d seq %d: start %d is negative", valueSize, seq, start)
			}
			if end < start {
				t.Fatalf("value %d seq %d: end %d before start %d", valueSize, seq, end, start)
			}
			if end-start+1 != rangeWindow {
				t.Fatalf("value %d seq %d: window %d..%d is %d wide, want %d", valueSize, seq, start, end, end-start+1, rangeWindow)
			}
			// For a value at least one window wide the window must stay inside it.
			if valueSize >= rangeWindow && end > int64(valueSize)-1 {
				t.Fatalf("value %d seq %d: end %d runs past last byte %d", valueSize, seq, end, valueSize-1)
			}
		}
	}
}

// GETRANGE reads keys that must already hold a value, so it has to declare a
// preload the way GET and mixed do, or every probe reads an empty string.
func TestGetRangeDeclaresPreload(t *testing.T) {
	gen, ops, ok := PreloadFor("getrange", Spec{ValueSize: 4096, KeyCount: 1000})
	if !ok {
		t.Fatal("getrange: PreloadFor returned ok=false, want a preload")
	}
	if ops != 1000 {
		t.Fatalf("getrange: preload ops = %d, want 1000", ops)
	}
	argv := gen(0, 0)
	if string(argv[0]) != "SET" {
		t.Fatalf("getrange: preload cmd = %q, want SET", argv[0])
	}
	if len(argv[2]) != 4096 {
		t.Fatalf("getrange: preload value is %d bytes, want 4096 (the windowed read needs a large value)", len(argv[2]))
	}
}

func TestUnknownWorkload(t *testing.T) {
	if Build("nope", Spec{}) != nil {
		t.Fatal("expected nil for unknown workload")
	}
}
