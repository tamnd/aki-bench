package target

import (
	"net"
	"testing"
)

func TestProvideConnectMode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	tg, err := Provide(Spec{Kind: Redis, Addr: ln.Addr().String()})
	if err != nil {
		t.Fatalf("connect mode should succeed against a listening socket: %v", err)
	}
	defer tg.Close()
	if tg.Addr != ln.Addr().String() {
		t.Fatalf("addr = %s, want %s", tg.Addr, ln.Addr().String())
	}
}

func TestProvideConnectUnreachableSkips(t *testing.T) {
	_, err := Provide(Spec{Kind: Redis, Addr: "127.0.0.1:1"})
	if err == nil {
		t.Fatal("expected a skip for an unreachable address")
	}
	if !IsSkip(err) {
		t.Fatalf("expected SkipError, got %T", err)
	}
}

func TestProvideMissingBinarySkips(t *testing.T) {
	_, err := Provide(Spec{Kind: Aki, Binary: "definitely-not-a-real-binary-xyz"})
	if err == nil {
		t.Fatal("expected a skip for a missing binary")
	}
	if !IsSkip(err) {
		t.Fatalf("expected SkipError, got %T", err)
	}
}

func TestLaunchArgsFairness(t *testing.T) {
	mem := launchArgs(Redis, 6400, "/tmp/x", InMemory)
	if !contains(mem, "no") {
		t.Fatalf("in-memory redis should disable appendonly: %v", mem)
	}
	dur := launchArgs(Redis, 6400, "/tmp/x", Durable)
	if !contains(dur, "yes") || !contains(dur, "always") {
		t.Fatalf("durable redis should set appendonly always: %v", dur)
	}

	akiMem := launchArgs(Aki, 6400, "/tmp/x", InMemory)
	if !contains(akiMem, "--addr") || !contains(akiMem, "127.0.0.1:6400") {
		t.Fatalf("aki should listen on the chosen addr: %v", akiMem)
	}
	if !contains(akiMem, "--dir") || !contains(akiMem, "/tmp/x") {
		t.Fatalf("aki should use the data dir: %v", akiMem)
	}
	if !contains(akiMem, "no") {
		t.Fatalf("in-memory aki should disable appendonly: %v", akiMem)
	}
	akiDur := launchArgs(Aki, 6400, "/tmp/x", Durable)
	if !contains(akiDur, "yes") || !contains(akiDur, "always") {
		t.Fatalf("durable aki should set appendonly always: %v", akiDur)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
