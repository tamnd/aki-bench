package workload

import "testing"

func TestParseCountBands(t *testing.T) {
	cases := map[string]int{
		"1":   1,
		"10":  10,
		"100": 100,
		"1k":  1000,
		"10k": 10000,
		"1M":  1000000,
		"10m": 10000000,
	}
	for tok, want := range cases {
		got, err := ParseCount(tok)
		if err != nil {
			t.Errorf("ParseCount(%q): %v", tok, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCount(%q) = %d, want %d", tok, got, want)
		}
	}
	for _, bad := range []string{"", "k", "-1", "0", "1x"} {
		if _, err := ParseCount(bad); err == nil {
			t.Errorf("ParseCount(%q) should fail", bad)
		}
	}
}

func TestParseSizeBands(t *testing.T) {
	cases := map[string]int{
		"16":    16,
		"64":    64,
		"64b":   64,
		"1k":    1024,
		"1KiB":  1024,
		"1kb":   1024,
		"64k":   64 * 1024,
		"64KiB": 64 * 1024,
		"1m":    1 << 20,
		"1MiB":  1 << 20,
	}
	for tok, want := range cases {
		got, err := ParseSize(tok)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tok, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", tok, got, want)
		}
	}
	for _, bad := range []string{"", "kib", "-64", "0", "1g"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestGateBandsMatchDoc18(t *testing.T) {
	// The gate bands are contract values from spec 2064/f3/18 section 2.1; a
	// drift here silently changes what every published row means.
	wantCard := []int{1, 10, 10000, 1000000}
	if got := CardBands(); !equalInts(got, wantCard) {
		t.Errorf("CardBands() = %v, want %v", got, wantCard)
	}
	wantVal := []int{64, 1024, 65536}
	if got := ValueBands(); !equalInts(got, wantVal) {
		t.Errorf("ValueBands() = %v, want %v", got, wantVal)
	}
}

func TestValueSizeSweepReachesOneMiB(t *testing.T) {
	sweep := ValueSizeSweep()
	if sweep[len(sweep)-1] != 1<<20 {
		t.Errorf("value sweep must reach 1MiB, got %v", sweep)
	}
}

func TestBandTransitionSweepStraddlesThresholds(t *testing.T) {
	sizes := map[int]bool{}
	for _, s := range BandTransitionSweep() {
		sizes[s] = true
	}
	for _, th := range BandThresholds() {
		for _, s := range []int{th / 2, th, th * 2} {
			if !sizes[s] {
				t.Errorf("transition sweep misses %d around threshold %d: %v", s, th, BandTransitionSweep())
			}
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
