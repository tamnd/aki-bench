package load_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tamnd/aki-bench/load"
)

// setKey stores one key on the fake server through a real client, so the
// coverage probe reads back exactly what a SET workload would have written.
func setKey(t *testing.T, addr, key, val string) {
	t.Helper()
	cl, err := load.Dial(addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.WriteCommand([][]byte{[]byte("SET"), []byte(key), []byte(val)}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := cl.ReadReply(); err != nil {
		t.Fatal(err)
	}
}

const (
	covKeyCount  = 200
	covValueSize = 8
	covFill      = 'x'
)

func covValue(b byte, n int) string {
	s := make([]byte, n)
	for i := range s {
		s[i] = b
	}
	return string(s)
}

func probe(t *testing.T, addr string, sample int) load.RetrievabilityResult {
	t.Helper()
	res, err := load.ProbeRetrievability(load.RetrievabilitySpec{
		Addr:      addr,
		KeyPrefix: "key:",
		KeyCount:  covKeyCount,
		ValueSize: covValueSize,
		Fill:      covFill,
		Sample:    sample,
		Seed:      1,
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestCoverageAllPresent: every key holds the exact value the workload writes,
// so the whole sample is retrievable and the fraction is 1.0.
func TestCoverageAllPresent(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	for i := 0; i < covKeyCount; i++ {
		setKey(t, srv.addr(), "key:"+strconv.Itoa(i), covValue(covFill, covValueSize))
	}
	res := probe(t, srv.addr(), covKeyCount)
	if res.Sampled != covKeyCount || res.Retrievable != covKeyCount || res.Missing != 0 {
		t.Fatalf("all-present probe: %+v", res)
	}
	if f := res.Fraction(); f != 1.0 {
		t.Fatalf("fraction = %v, want 1.0", f)
	}
}

// TestCoverageAllMissing: nothing was written, so every GET returns nil and the
// probe counts each as a miss. This is the eviction case the M0 LTM cell hid.
func TestCoverageAllMissing(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	res := probe(t, srv.addr(), covKeyCount)
	if res.Missing != covKeyCount || res.Retrievable != 0 {
		t.Fatalf("all-missing probe: %+v", res)
	}
	if f := res.Fraction(); f != 0 {
		t.Fatalf("fraction = %v, want 0", f)
	}
}

// TestCoverageWrongLength: the value is present but truncated, so the probe
// flags it as wrong length rather than crediting it as retrievable.
func TestCoverageWrongLength(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	for i := 0; i < covKeyCount; i++ {
		setKey(t, srv.addr(), "key:"+strconv.Itoa(i), covValue(covFill, covValueSize-1))
	}
	res := probe(t, srv.addr(), covKeyCount)
	if res.WrongLength != covKeyCount || res.Retrievable != 0 {
		t.Fatalf("wrong-length probe: %+v", res)
	}
}

// TestCoverageCorrupt: the value is the right length but the wrong content, so
// the checksum scan catches it as corrupt.
func TestCoverageCorrupt(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	for i := 0; i < covKeyCount; i++ {
		setKey(t, srv.addr(), "key:"+strconv.Itoa(i), covValue('y', covValueSize))
	}
	res := probe(t, srv.addr(), covKeyCount)
	if res.Corrupt != covKeyCount || res.Retrievable != 0 {
		t.Fatalf("corrupt probe: %+v", res)
	}
}

// TestCoverageHalfEvicted: only the even keys were written, so a large uniform
// sample lands near half retrievable and half missing. The bookkeeping is exact
// (retrievable plus missing equals sampled); the split is statistical, checked
// with a wide tolerance so the test is not flaky.
func TestCoverageHalfEvicted(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	// The probe caps the sample at the keyspace, so a wide statistical draw needs
	// a keyspace at least as large as the sample. Draws are with replacement, so
	// sampling the whole 2000-key space still averages over independent picks.
	const keyspace = 2000
	for i := 0; i < keyspace; i += 2 {
		setKey(t, srv.addr(), "key:"+strconv.Itoa(i), covValue(covFill, covValueSize))
	}
	sample := keyspace
	res, err := load.ProbeRetrievability(load.RetrievabilitySpec{
		Addr:      srv.addr(),
		KeyPrefix: "key:",
		KeyCount:  keyspace,
		ValueSize: covValueSize,
		Fill:      covFill,
		Sample:    sample,
		Seed:      1,
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sampled != sample {
		t.Fatalf("sampled = %d, want %d", res.Sampled, sample)
	}
	if res.Retrievable+res.Missing != sample {
		t.Fatalf("retrievable+missing = %d, want %d (res %+v)", res.Retrievable+res.Missing, sample, res)
	}
	if f := res.Fraction(); f < 0.4 || f > 0.6 {
		t.Fatalf("half-evicted fraction = %v, want near 0.5", f)
	}
}

// TestCoverageZeroSample: a zero sample or an empty keyspace is a no-op, not an
// error, so a caller can leave the probe off without special-casing.
func TestCoverageZeroSample(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	res, err := load.ProbeRetrievability(load.RetrievabilitySpec{
		Addr: srv.addr(), KeyPrefix: "key:", KeyCount: covKeyCount, ValueSize: covValueSize, Fill: covFill, Sample: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sampled != 0 {
		t.Fatalf("zero-sample probe ran: %+v", res)
	}
}
