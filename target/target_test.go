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
	mem := launchArgs(Redis, 6400, "/tmp/x", InMemory, "", "", nil)
	if !contains(mem, "no") {
		t.Fatalf("in-memory redis should disable appendonly: %v", mem)
	}
	dur := launchArgs(Redis, 6400, "/tmp/x", Durable, "", "", nil)
	if !contains(dur, "yes") || !contains(dur, "always") {
		t.Fatalf("durable redis should set appendonly always: %v", dur)
	}

	akiMem := launchArgs(Aki, 6400, "/tmp/x", InMemory, "", "", nil)
	if !contains(akiMem, "--addr") || !contains(akiMem, "127.0.0.1:6400") {
		t.Fatalf("aki should listen on the chosen addr: %v", akiMem)
	}
	if !contains(akiMem, "--dir") || !contains(akiMem, "/tmp/x") {
		t.Fatalf("aki should use the data dir: %v", akiMem)
	}
	if !contains(akiMem, "no") {
		t.Fatalf("in-memory aki should disable appendonly: %v", akiMem)
	}
	akiDur := launchArgs(Aki, 6400, "/tmp/x", Durable, "", "", nil)
	if !contains(akiDur, "yes") || !contains(akiDur, "always") {
		t.Fatalf("durable aki should set appendonly always: %v", akiDur)
	}

	// The engine and net selectors must reach aki's flags when set, and Redis must
	// never receive them.
	akiEng := launchArgs(Aki, 6400, "/tmp/x", InMemory, "hot", "reactor", nil)
	if !contains(akiEng, "--aki-engine") || !contains(akiEng, "hot") {
		t.Fatalf("aki should pass the engine flag: %v", akiEng)
	}
	if !contains(akiEng, "--aki-net") || !contains(akiEng, "reactor") {
		t.Fatalf("aki should pass the net flag: %v", akiEng)
	}
	redisEng := launchArgs(Redis, 6400, "/tmp/x", InMemory, "hot", "reactor", nil)
	if contains(redisEng, "--aki-engine") || contains(redisEng, "--aki-net") {
		t.Fatalf("redis must not receive aki engine flags: %v", redisEng)
	}

	// Extra args reach a launched aki verbatim (the set campaign's -set-algebra-merge
	// passthrough) and never leak onto Redis, which does not understand them.
	akiExtra := launchArgs(Aki, 6400, "/tmp/x", InMemory, "f1raw", "", []string{"--set-algebra-merge"})
	if !contains(akiExtra, "--set-algebra-merge") {
		t.Fatalf("aki should pass extra args: %v", akiExtra)
	}
	redisExtra := launchArgs(Redis, 6400, "/tmp/x", InMemory, "", "", []string{"--set-algebra-merge"})
	if contains(redisExtra, "--set-algebra-merge") {
		t.Fatalf("redis must not receive aki extra args: %v", redisExtra)
	}
}

// TestLaunchArgsF3 checks the f3 engine gets f3srv's own CLI shape, which differs
// from the aki binary and f1srv: single-dash -addr and -net, no `server`
// subcommand, no --dir, and no appendonly flags (f3 launches in-memory here).
func TestLaunchArgsF3(t *testing.T) {
	f3 := launchArgs(Aki, 6400, "/tmp/x", InMemory, "f3", "reactor", []string{"-shards", "8"})
	if !contains(f3, "-addr") || !contains(f3, "127.0.0.1:6400") {
		t.Fatalf("f3 should listen on the chosen addr with a single-dash -addr: %v", f3)
	}
	if !contains(f3, "-net") || !contains(f3, "reactor") {
		t.Fatalf("f3 should pass the net model through -net: %v", f3)
	}
	if !contains(f3, "-shards") || !contains(f3, "8") {
		t.Fatalf("f3 should append extra args verbatim: %v", f3)
	}
	if contains(f3, "server") {
		t.Fatalf("f3srv has no server subcommand: %v", f3)
	}
	if contains(f3, "--dir") || contains(f3, "--addr") || contains(f3, "--aki-engine") {
		t.Fatalf("f3srv does not take the aki-binary flag shape: %v", f3)
	}
	if contains(f3, "--appendonly") || contains(f3, "yes") || contains(f3, "no") {
		t.Fatalf("f3 launches in-memory, no appendonly flags: %v", f3)
	}

	// A durable request still reaches launchArgs as InMemory for f3 because the
	// engine falls back to btree upstream, so this path never needs appendonly.
	// f1raw keeps the server-subcommand shape it always had.
	f1 := launchArgs(Aki, 6400, "/tmp/x", InMemory, "f1raw", "", nil)
	if !contains(f1, "server") || !contains(f1, "--dir") {
		t.Fatalf("f1raw should keep the server-subcommand shape: %v", f1)
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
