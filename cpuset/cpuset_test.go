package cpuset

import "testing"

func TestPartitionDisjointAndComplete(t *testing.T) {
	cases := []struct {
		numCPU, client       int
		wantServer, wantClnt string
	}{
		{6, 2, "0-3", "4-5"},
		{8, 2, "0-5", "6-7"},
		{4, 1, "0-2", "3"},
		{2, 1, "0", "1"},
		// client clamped up to one
		{4, 0, "0-2", "3"},
		// client clamped down so the server is never empty
		{4, 9, "0", "1-3"},
	}
	for _, c := range cases {
		server, client, err := Partition(c.numCPU, c.client)
		if err != nil {
			t.Fatalf("Partition(%d,%d): %v", c.numCPU, c.client, err)
		}
		if server != c.wantServer || client != c.wantClnt {
			t.Errorf("Partition(%d,%d) = %q,%q want %q,%q", c.numCPU, c.client, server, client, c.wantServer, c.wantClnt)
		}
		// The two halves must cover the whole machine and not overlap.
		sn, err := Count(server)
		if err != nil {
			t.Fatalf("Count(%q): %v", server, err)
		}
		cn, err := Count(client)
		if err != nil {
			t.Fatalf("Count(%q): %v", client, err)
		}
		if sn+cn != c.numCPU {
			t.Errorf("Partition(%d,%d) covers %d cores, want %d", c.numCPU, c.client, sn+cn, c.numCPU)
		}
		if sn == 0 || cn == 0 {
			t.Errorf("Partition(%d,%d) left an empty half: server=%d client=%d", c.numCPU, c.client, sn, cn)
		}
	}
}

func TestPartitionRejectsTinyMachine(t *testing.T) {
	if _, _, err := Partition(1, 1); err == nil {
		t.Fatal("Partition(1,1) should error, a single core cannot be split")
	}
}

func TestDefaultClientCores(t *testing.T) {
	cases := []struct {
		numCPU, want int
	}{
		{2, 1},  // quarter is 0, floor 2 clamps to numCPU-1
		{4, 2},  // quarter is 1, floor lifts to 2
		{6, 2},  // matches the redis-benchmark --threads 4 cross check
		{8, 2},  // quarter is 2
		{16, 4}, // quarter is 4
		{32, 8},
	}
	for _, c := range cases {
		if got := DefaultClientCores(c.numCPU); got != c.want {
			t.Errorf("DefaultClientCores(%d) = %d want %d", c.numCPU, got, c.want)
		}
		// Whatever it returns must leave the server at least one core.
		if DefaultClientCores(c.numCPU) >= c.numCPU {
			t.Errorf("DefaultClientCores(%d) took the whole machine", c.numCPU)
		}
	}
}

func TestCount(t *testing.T) {
	cases := []struct {
		list string
		want int
	}{
		{"0", 1},
		{"0-3", 4},
		{"4-5", 2},
		{"0-3,6", 5},
		{"0,2,4", 3},
		{"0-1,4-5", 4},
		{" 0-3 , 6 ", 5},
	}
	for _, c := range cases {
		got, err := Count(c.list)
		if err != nil {
			t.Fatalf("Count(%q): %v", c.list, err)
		}
		if got != c.want {
			t.Errorf("Count(%q) = %d want %d", c.list, got, c.want)
		}
	}
}

func TestCountRejectsBadLists(t *testing.T) {
	for _, list := range []string{"", "  ", "x", "3-1", "0-", "-2"} {
		if _, err := Count(list); err == nil {
			t.Errorf("Count(%q) should error", list)
		}
	}
}
