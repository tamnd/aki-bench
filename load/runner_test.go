package load_test

import (
	"context"
	"testing"
	"time"

	"github.com/tamnd/aki-bench/load"
	"github.com/tamnd/aki-bench/workload"
)

func TestClientRoundTrip(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	cl, err := load.Dial(srv.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.WriteCommand([][]byte{[]byte("SET"), []byte("k"), []byte("v")}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Flush(); err != nil {
		t.Fatal(err)
	}
	v, err := cl.ReadReplyValue()
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := v.(string); !ok || s != "OK" {
		t.Fatalf("want +OK, got %#v", v)
	}

	if err := cl.WriteCommand([][]byte{[]byte("GET"), []byte("k")}); err != nil {
		t.Fatal(err)
	}
	_ = cl.Flush()
	v, err = cl.ReadReplyValue()
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := v.([]byte); !ok || string(b) != "v" {
		t.Fatalf("want bulk v, got %#v", v)
	}
}

func TestRunClosedLoopByRequests(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	gen := workload.Set(workload.Spec{ValueSize: 8, KeyCount: 100})
	res, err := load.Run(context.Background(), load.Config{
		Addr:        srv.addr(),
		Connections: 4,
		Pipeline:    8,
		Requests:    2000,
		Gen:         gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ops == 0 {
		t.Fatal("expected nonzero ops")
	}
	if res.Errors != 0 {
		t.Fatalf("expected no errors, got %d", res.Errors)
	}
	if res.OpsPerSec() <= 0 {
		t.Fatal("expected positive throughput")
	}
	if res.Hist.TotalCount() != res.Ops {
		t.Fatalf("histogram count %d != ops %d", res.Hist.TotalCount(), res.Ops)
	}
}

func TestRunClosedLoopByDuration(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	gen := workload.Get(workload.Spec{KeyCount: 100})
	res, err := load.Run(context.Background(), load.Config{
		Addr:        srv.addr(),
		Connections: 2,
		Pipeline:    1,
		Duration:    150 * time.Millisecond,
		Gen:         gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ops == 0 {
		t.Fatal("expected nonzero ops within the duration window")
	}
}

func TestRunOpenLoop(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	gen := workload.Incr(workload.Spec{KeyCount: 16})
	res, err := load.Run(context.Background(), load.Config{
		Addr:        srv.addr(),
		Connections: 4,
		Mode:        load.OpenLoop,
		TargetRate:  4000,
		Duration:    200 * time.Millisecond,
		Gen:         gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ops == 0 {
		t.Fatal("expected nonzero ops in open-loop")
	}
	if p99 := res.Hist.ValueAtPercentile(99); p99 <= 0 {
		t.Fatal("expected a recorded p99 latency")
	}
}

func TestRunNoServer(t *testing.T) {
	gen := workload.Set(workload.Spec{})
	_, err := load.Run(context.Background(), load.Config{
		Addr:        "127.0.0.1:1", // nothing listens here
		Connections: 2,
		Requests:    10,
		DialTimeout: 200 * time.Millisecond,
		Gen:         gen,
	})
	if err == nil {
		t.Fatal("expected a dial error when no server is present")
	}
	var de *load.DialError
	if !asDialError(err, &de) {
		t.Fatalf("expected *load.DialError, got %T", err)
	}
}

func asDialError(err error, target **load.DialError) bool {
	for err != nil {
		if de, ok := err.(*load.DialError); ok {
			*target = de
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
