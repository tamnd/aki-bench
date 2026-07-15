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
	mem := launchArgs(Redis, 6400, "/tmp/x", InMemory, "", "")
	if !contains(mem, "no") {
		t.Fatalf("in-memory redis should disable appendonly: %v", mem)
	}
	dur := launchArgs(Redis, 6400, "/tmp/x", Durable, "", "")
	if !contains(dur, "yes") || !contains(dur, "always") {
		t.Fatalf("durable redis should set appendonly always: %v", dur)
	}

	akiMem := launchArgs(Aki, 6400, "/tmp/x", InMemory, "", "")
	if !contains(akiMem, "--addr") || !contains(akiMem, "127.0.0.1:6400") {
		t.Fatalf("aki should listen on the chosen addr: %v", akiMem)
	}
	if !contains(akiMem, "--dir") || !contains(akiMem, "/tmp/x") {
		t.Fatalf("aki should use the data dir: %v", akiMem)
	}
	if !contains(akiMem, "no") {
		t.Fatalf("in-memory aki should disable appendonly: %v", akiMem)
	}
	akiDur := launchArgs(Aki, 6400, "/tmp/x", Durable, "", "")
	if !contains(akiDur, "yes") || !contains(akiDur, "always") {
		t.Fatalf("durable aki should set appendonly always: %v", akiDur)
	}

	// The engine and net selectors must reach aki's flags when set, and Redis must
	// never receive them.
	akiEng := launchArgs(Aki, 6400, "/tmp/x", InMemory, "hot", "reactor")
	if !contains(akiEng, "--aki-engine") || !contains(akiEng, "hot") {
		t.Fatalf("aki should pass the engine flag: %v", akiEng)
	}
	if !contains(akiEng, "--aki-net") || !contains(akiEng, "reactor") {
		t.Fatalf("aki should pass the net flag: %v", akiEng)
	}
	redisEng := launchArgs(Redis, 6400, "/tmp/x", InMemory, "hot", "reactor")
	if contains(redisEng, "--aki-engine") || contains(redisEng, "--aki-net") {
		t.Fatalf("redis must not receive aki engine flags: %v", redisEng)
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

func TestLaunchArgsF3(t *testing.T) {
	// f3srv's whole M0 flag surface is --addr, --shards, and --arena-mib, and
	// the fairness rule runs it on shipped defaults, so the launch line must be
	// the bare listen address: no server subcommand, no --dir, no appendonly
	// flags, and no --aki-engine (the binary is the engine).
	args := launchArgs(Aki, 6400, "/tmp/x", InMemory, "f3", "")
	want := []string{"--addr", "127.0.0.1:6400"}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("f3 launch args = %v, want %v", args, want)
	}
	if contains(args, "server") || contains(args, "--dir") || contains(args, "--appendonly") {
		t.Fatalf("f3srv must not receive the aki binary's flag shape: %v", args)
	}
}

func TestLaunchArgsSqlo1(t *testing.T) {
	// sqlo1srv's whole S0 flag surface is -addr and -store, and it runs on its
	// shipped defaults, so the launch line must be the bare listen address: no
	// server subcommand, no --dir, no appendonly flags, and no --aki-engine
	// (the binary is the engine).
	args := launchArgs(Aki, 6400, "/tmp/x", InMemory, "sqlo1", "")
	want := []string{"-addr", "127.0.0.1:6400"}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("sqlo1 launch args = %v, want %v", args, want)
	}
	if contains(args, "server") || contains(args, "--dir") || contains(args, "--appendonly") {
		t.Fatalf("sqlo1srv must not receive the aki binary's flag shape: %v", args)
	}
}
