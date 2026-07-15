package main

import (
	"testing"

	"github.com/tamnd/aki-bench/target"
)

func TestParseOrder(t *testing.T) {
	got, err := parseOrder("valkey,redis,aki")
	if err != nil {
		t.Fatal(err)
	}
	want := []target.Kind{target.Valkey, target.Redis, target.Aki}
	for i, k := range want {
		if got[i] != k {
			t.Fatalf("parseOrder = %v, want %v", got, want)
		}
	}

	// Spaces after commas are a natural way to write the flag and must parse.
	if _, err := parseOrder("aki, redis, valkey"); err != nil {
		t.Fatalf("spaced order rejected: %v", err)
	}

	// A subset would drop a row, a repeat would measure one server twice under
	// the same label, and an unknown name is a typo; all must fail loudly.
	for _, bad := range []string{"aki,redis", "aki,redis,redis", "aki,redis,memcached", ""} {
		if _, err := parseOrder(bad); err == nil {
			t.Errorf("parseOrder(%q) accepted, want error", bad)
		}
	}
}
