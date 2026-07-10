// Band axes for the f3 gate matrix (spec 2064/f3/18 section 2.1). The matrix
// names its workload sizes as first-class bands: cardinality rows at 1, 10, 10k,
// and 1M pin the four representations of the size ladder, value rows run from
// 16B to 1MiB, and the band-transition sweeps ramp the value size across the
// string model's placement thresholds. This file gives those bands names and
// parsers so a gate row is a flag value, not a hand-assembled recipe, and so a
// scenario script and the emitted results row spell a band the same way.
package workload

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseCount parses a cardinality or keyspace band token: a plain integer, or
// an integer with a k or m multiplier suffix (case-insensitive), so the doc 18
// band names 1, 10, 10k, and 1M parse as written. 10k is 10_000 and 1m is
// 1_000_000; counts are decimal because they name how many, not how big.
func ParseCount(tok string) (int, error) {
	t := strings.ToLower(strings.TrimSpace(tok))
	if t == "" {
		return 0, fmt.Errorf("empty count")
	}
	mult := 1
	switch t[len(t)-1] {
	case 'k':
		mult = 1000
		t = t[:len(t)-1]
	case 'm':
		mult = 1000000
		t = t[:len(t)-1]
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, fmt.Errorf("bad count %q: %w", tok, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("count %q must be positive", tok)
	}
	return n * mult, nil
}

// ParseSize parses a value-size band token: a plain byte count, or a count with
// a b, k/kb/kib, or m/mb/mib suffix (case-insensitive). Sizes are binary, since
// they name bytes and the doc 18 value bands are 1KiB and 64KiB and 1MiB: 1k is
// 1024 and 1m is 1048576.
func ParseSize(tok string) (int, error) {
	t := strings.ToLower(strings.TrimSpace(tok))
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := 1
	switch {
	case strings.HasSuffix(t, "kib"), strings.HasSuffix(t, "kb"):
		mult = 1024
		t = strings.TrimSuffix(strings.TrimSuffix(t, "b"), "i")
		t = strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "mib"), strings.HasSuffix(t, "mb"):
		mult = 1 << 20
		t = strings.TrimSuffix(strings.TrimSuffix(t, "b"), "i")
		t = strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "k"):
		mult = 1024
		t = strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "m"):
		mult = 1 << 20
		t = strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "b"):
		t = strings.TrimSuffix(t, "b")
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, fmt.Errorf("bad size %q: %w", tok, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size %q must be positive", tok)
	}
	return n * mult, nil
}

// CardBands is the gate cardinality ladder: 1 and 10 sit in the inline band
// where Redis's listpack encodings win today, 10k in the native band, and 1M in
// the partitioned band, pinning the four representations of the size ladder.
func CardBands() []int { return []int{1, 10, 10000, 1000000} }

// CardSweepBands is the off-diagonal cardinality coverage between and beyond the
// gate bands, run as sweep rows so the gate ladder cannot hide a hole.
func CardSweepBands() []int { return []int{100, 1000, 100000, 10000000} }

// ValueBands is the gate value ladder: 64B is the historical baseline every f1
// number is denominated in, 1KiB crosses the separation threshold, and 64KiB
// enters the giant-value streaming path.
func ValueBands() []int { return []int{64, 1024, 64 * 1024} }

// BandThresholds is the string model's placement thresholds (doc 09, baked by
// the M0 labs): values above 1KiB leave the inline band for the separated value
// log, and values above 64KiB leave it for the chunked giant-value path.
func BandThresholds() []int { return []int{1024, 64 * 1024} }

// BandTransitionSweep is the value-size ramp for the band-transition rows: for
// each placement threshold it straddles the boundary with a size at half, at,
// and at double the threshold, so a cliff at a promotion boundary shows up as a
// step between adjacent rows instead of hiding between two distant gate points.
func BandTransitionSweep() []int {
	var sizes []int
	for _, t := range BandThresholds() {
		sizes = append(sizes, t/2, t, t*2)
	}
	return sizes
}
