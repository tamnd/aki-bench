package load_test

import (
	"testing"
	"time"

	"github.com/tamnd/aki-bench/load"
)

func TestProbeServerRedis(t *testing.T) {
	s, err := newFakeServer()
	if err != nil {
		t.Fatalf("fake server: %v", err)
	}
	defer s.close()

	info, err := load.ProbeServer(s.addr(), time.Second)
	if err != nil {
		t.Fatalf("ProbeServer: %v", err)
	}
	if info.Software != "redis" || info.Version != "8.8.0" {
		t.Fatalf("got %+v, want redis 8.8.0", info)
	}
	if got := info.String(); got != "redis 8.8.0" {
		t.Fatalf("String() = %q, want %q", got, "redis 8.8.0")
	}
}

func TestProbeServerValkey(t *testing.T) {
	s, err := newFakeServer()
	if err != nil {
		t.Fatalf("fake server: %v", err)
	}
	// Valkey reports both fields; the probe must name valkey, not the
	// compatibility-shim redis_version.
	s.info = "# Server\r\nredis_version:7.2.4\r\nvalkey_version:9.1.0\r\nserver_name:valkey\r\n"
	defer s.close()

	info, err := load.ProbeServer(s.addr(), time.Second)
	if err != nil {
		t.Fatalf("ProbeServer: %v", err)
	}
	if info.Software != "valkey" || info.Version != "9.1.0" {
		t.Fatalf("got %+v, want valkey 9.1.0", info)
	}
}

func TestProbeServerUnreachable(t *testing.T) {
	// 127.0.0.1:1 is reserved and refuses connections, so the dial fails fast.
	if _, err := load.ProbeServer("127.0.0.1:1", 200*time.Millisecond); err == nil {
		t.Fatal("expected an error probing an unreachable address")
	}
}

func TestServerInfoString(t *testing.T) {
	cases := []struct {
		in   load.ServerInfo
		want string
	}{
		{load.ServerInfo{Software: "redis", Version: "8.8.0"}, "redis 8.8.0"},
		{load.ServerInfo{Version: "8.8.0"}, "8.8.0"},
		{load.ServerInfo{}, "unknown"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("%+v String() = %q, want %q", c.in, got, c.want)
		}
	}
}
