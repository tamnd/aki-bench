package load_test

import (
	"testing"
	"time"

	"github.com/tamnd/aki-bench/load"
)

func TestProbeEvictionStats(t *testing.T) {
	s, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.info = "# Stats\r\n" +
		"keyspace_hits:1500000\r\n" +
		"keyspace_misses:3350000\r\n" +
		"evicted_keys:1300000\r\n" +
		"# Memory\r\n" +
		"maxmemory:536870912\r\n" +
		"maxmemory_policy:allkeys-lfu\r\n"

	st, err := load.ProbeEvictionStats(s.addr(), time.Second)
	if err != nil {
		t.Fatalf("ProbeEvictionStats: %v", err)
	}
	if st.KeyspaceHits != 1500000 || st.KeyspaceMisses != 3350000 {
		t.Fatalf("hits/misses = %d/%d", st.KeyspaceHits, st.KeyspaceMisses)
	}
	if st.EvictedKeys != 1300000 {
		t.Fatalf("evicted_keys = %d", st.EvictedKeys)
	}
	if st.Maxmemory != 536870912 || st.MaxmemoryPolicy != "allkeys-lfu" {
		t.Fatalf("cap = %d policy = %q", st.Maxmemory, st.MaxmemoryPolicy)
	}
}

func TestProbeEvictionStatsAbsentFields(t *testing.T) {
	// A server whose INFO carries none of the fields (or a fake default block)
	// reports zeros, not an error: a partial readback beats none.
	s, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	st, err := load.ProbeEvictionStats(s.addr(), time.Second)
	if err != nil {
		t.Fatalf("ProbeEvictionStats: %v", err)
	}
	if st != (load.EvictionStats{}) {
		t.Fatalf("expected zero stats, got %+v", st)
	}
}

func TestEvictionStatsSub(t *testing.T) {
	pre := load.EvictionStats{KeyspaceHits: 100, KeyspaceMisses: 50, EvictedKeys: 10,
		Maxmemory: 1 << 29, MaxmemoryPolicy: "allkeys-lfu"}
	post := load.EvictionStats{KeyspaceHits: 400, KeyspaceMisses: 850, EvictedKeys: 310,
		Maxmemory: 1 << 29, MaxmemoryPolicy: "allkeys-lfu"}
	d := post.Sub(pre)
	if d.KeyspaceHits != 300 || d.KeyspaceMisses != 800 || d.EvictedKeys != 300 {
		t.Fatalf("delta wrong: %+v", d)
	}
	if d.Maxmemory != 1<<29 || d.MaxmemoryPolicy != "allkeys-lfu" {
		t.Fatalf("config fields must come from the later snapshot: %+v", d)
	}
}

func TestProbeEvictionStatsUnreachable(t *testing.T) {
	if _, err := load.ProbeEvictionStats("127.0.0.1:1", 200*time.Millisecond); err == nil {
		t.Fatal("expected a dial error")
	}
}
