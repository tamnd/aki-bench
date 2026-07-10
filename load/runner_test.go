package load_test

import (
	"context"
	"strconv"
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

func TestRunWarmupExcludedFromResult(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	gen := workload.Set(workload.Spec{ValueSize: 64, KeyCount: 100})
	res, err := load.Run(context.Background(), load.Config{
		Addr:        srv.addr(),
		Connections: 2,
		Pipeline:    4,
		Requests:    400,
		Warmup:      100 * time.Millisecond,
		Gen:         gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The warm drive hammers the fake server for 100ms before timing starts,
	// which is thousands of extra operations on this loopback path. None of
	// them may leak into the result: the op count, the histogram, and the wire
	// bytes must all describe exactly the 400 measured requests.
	if res.Ops != 400 {
		t.Fatalf("ops %d, want exactly the 400 measured requests", res.Ops)
	}
	if res.Hist.TotalCount() != res.Ops {
		t.Fatalf("histogram count %d != ops %d, warmup samples leaked", res.Hist.TotalCount(), res.Ops)
	}
	// A SET frame is under 200 wire bytes here, so measured writes stay well
	// below this cap; 100ms of warmup traffic on loopback would blow past it.
	if res.BytesWritten > res.Ops*200 {
		t.Fatalf("bytes written %d too high for %d ops, warmup bytes leaked", res.BytesWritten, res.Ops)
	}
	if res.Errors != 0 {
		t.Fatalf("expected no errors, got %d", res.Errors)
	}
}

func TestRunCountsWireBytes(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	gen := workload.Set(workload.Spec{ValueSize: 64, KeyCount: 100})
	res, err := load.Run(context.Background(), load.Config{
		Addr:        srv.addr(),
		Connections: 2,
		Pipeline:    4,
		Requests:    400,
		Gen:         gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every SET frame carries at least its 64-byte payload on the way out and a
	// +OK\r\n on the way back, so the wire totals are bounded below by the op
	// count. This is the CF20 bytes/s column's source; a zero here would mean a
	// bandwidth-bound giant-value cell could pass as a throughput tie unseen.
	if res.BytesWritten < res.Ops*64 {
		t.Fatalf("bytes written %d too low for %d ops of 64B values", res.BytesWritten, res.Ops)
	}
	if res.BytesRead < res.Ops*5 {
		t.Fatalf("bytes read %d too low for %d +OK replies", res.BytesRead, res.Ops)
	}
	if res.BytesPerSec() <= 0 {
		t.Fatal("expected positive wire bandwidth")
	}
}

// TestRunSplitsNilAndErrorReplies drives a mix where every fourth operation is
// a SET (value-bearing ack), a GET of that key (value-bearing hit), a GET of a
// key that was never written (nil reply), and a command the server refuses
// (server error reply). The runner must count all of them as completed ops but
// report the nil and error replies separately, because a larger-than-memory
// comparison gates on the ops that actually returned data.
func TestRunSplitsNilAndErrorReplies(t *testing.T) {
	srv, err := newFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	gen := func(conn int, seq int64) [][]byte {
		key := []byte("k:" + strconv.Itoa(conn))
		switch seq % 4 {
		case 0:
			return [][]byte{[]byte("SET"), key, []byte("v")}
		case 1:
			return [][]byte{[]byte("GET"), key}
		case 2:
			return [][]byte{[]byte("GET"), []byte("never-written")}
		default:
			return [][]byte{[]byte("FAILME")}
		}
	}
	res, err := load.Run(context.Background(), load.Config{
		Addr:        srv.addr(),
		Connections: 2,
		Pipeline:    4,
		Requests:    400,
		Gen:         gen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ops != 400 {
		t.Fatalf("ops %d, want 400: nil and error replies are still completed ops", res.Ops)
	}
	if res.Errors != 0 {
		t.Fatalf("transport errors %d, want 0: a -ERR reply is not a transport failure", res.Errors)
	}
	if res.NilReplies != 100 {
		t.Fatalf("nil replies %d, want 100", res.NilReplies)
	}
	if res.ErrReplies != 100 {
		t.Fatalf("error replies %d, want 100", res.ErrReplies)
	}
	if res.ValueOps() != 200 {
		t.Fatalf("value ops %d, want 200", res.ValueOps())
	}
	if got := res.HitRatio(); got != 0.5 {
		t.Fatalf("hit ratio %v, want 0.5", got)
	}
	if res.ValueOpsPerSec() <= 0 || res.ValueOpsPerSec() >= res.OpsPerSec() {
		t.Fatalf("value ops/s %v should be positive and below total ops/s %v",
			res.ValueOpsPerSec(), res.OpsPerSec())
	}
}

// TestReadReplyKind pins the classification: data-bearing replies, RESP nulls,
// and server error replies each land in their own bucket, and none of them are
// transport errors.
func TestReadReplyKind(t *testing.T) {
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

	roundTrip := func(argv ...string) load.ReplyKind {
		t.Helper()
		cmd := make([][]byte, len(argv))
		for i, a := range argv {
			cmd[i] = []byte(a)
		}
		if err := cl.WriteCommand(cmd); err != nil {
			t.Fatal(err)
		}
		if err := cl.Flush(); err != nil {
			t.Fatal(err)
		}
		kind, err := cl.ReadReplyKind()
		if err != nil {
			t.Fatal(err)
		}
		return kind
	}

	if kind := roundTrip("SET", "k", "v"); kind != load.ReplyValue {
		t.Fatalf("+OK classified as %v, want ReplyValue", kind)
	}
	if kind := roundTrip("GET", "k"); kind != load.ReplyValue {
		t.Fatalf("bulk reply classified as %v, want ReplyValue", kind)
	}
	if kind := roundTrip("GET", "missing"); kind != load.ReplyNil {
		t.Fatalf("null bulk classified as %v, want ReplyNil", kind)
	}
	if kind := roundTrip("FAILME"); kind != load.ReplyErr {
		t.Fatalf("-ERR classified as %v, want ReplyErr", kind)
	}
	if kind := roundTrip("INCR", "n"); kind != load.ReplyValue {
		t.Fatalf("integer reply classified as %v, want ReplyValue", kind)
	}
}
