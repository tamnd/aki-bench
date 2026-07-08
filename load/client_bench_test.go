package load

import (
	"bufio"
	"io"
	"testing"
)

// These are the permanent reference numbers for the load generator's own RESP
// cost: how many nanoseconds it spends framing a command and parsing a reply,
// the work that sits between the workload and the socket on every operation. A
// benchmark result is only trustworthy if the harness is not the bottleneck, so
// these are committed to be re-run on the target box:
//
//	go test ./load/ -run x -bench BenchmarkClient -benchmem
//
// If WriteCommand or ReadReplyValue ever drifts up into the microseconds, a
// "slow" target might just be a slow client, and this is how that gets caught.

// discardWriter is a Client wired to a Writer that throws bytes away, so
// WriteCommand measures only the framing cost with no socket or syscall.
func discardClient() *Client {
	return &Client{w: bufio.NewWriterSize(io.Discard, 64*1024)}
}

// loopReader serves the same canned bytes forever, so a parse benchmark reads a
// fresh reply every iteration without re-dialing or re-allocating the source.
type loopReader struct {
	buf []byte
	pos int
}

func (l *loopReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if l.pos == len(l.buf) {
			l.pos = 0
		}
		c := copy(p[n:], l.buf[l.pos:])
		l.pos += c
		n += c
	}
	return n, nil
}

func parseClient(reply []byte) *Client {
	return &Client{r: bufio.NewReaderSize(&loopReader{buf: reply}, 64*1024)}
}

// BenchmarkClientWriteSet frames a SET of a 64-byte value, the common write the
// gate measures, and discards it. This is the per-op send cost.
func BenchmarkClientWriteSet(b *testing.B) {
	c := discardClient()
	key := []byte("bench:key:0001")
	val := make([]byte, 64)
	argv := [][]byte{[]byte("SET"), key, val}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.WriteCommand(argv); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClientWriteGet frames a GET, the smallest common command, so the
// fixed per-command framing overhead is visible apart from the payload.
func BenchmarkClientWriteGet(b *testing.B) {
	c := discardClient()
	argv := [][]byte{[]byte("GET"), []byte("bench:key:0001")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.WriteCommand(argv); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClientReadBulk parses a 64-byte bulk-string reply, the GET path. The
// allocation here is the value buffer the client returns, which is the floor a
// read workload pays per op.
func BenchmarkClientReadBulk(b *testing.B) {
	val := make([]byte, 64)
	reply := append([]byte("$64\r\n"), append(val, '\r', '\n')...)
	c := parseClient(reply)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadReplyValue(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClientReadInteger parses an integer reply, the INCR and *PUSH path,
// which returns no buffer and is the cheapest reply to decode.
func BenchmarkClientReadInteger(b *testing.B) {
	c := parseClient([]byte(":1\r\n"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadReplyValue(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClientReadArray parses a small multibulk reply, the LRANGE and HGETALL
// shape, where the per-element decode and the slice growth show up.
func BenchmarkClientReadArray(b *testing.B) {
	reply := []byte("*3\r\n$3\r\nfoo\r\n$3\r\nbar\r\n$3\r\nbaz\r\n")
	c := parseClient(reply)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReadReplyValue(); err != nil {
			b.Fatal(err)
		}
	}
}
