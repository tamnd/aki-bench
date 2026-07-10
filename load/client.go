// Package load is a native, zero-dependency RESP client and load generator.
// It opens N concurrent connections to a Redis-protocol server, drives a workload
// at a configurable pipeline depth in either closed-loop or open-loop mode, and
// records per-operation latency into an HdrHistogram so the report layer can read
// throughput and tail percentiles back out.
package load

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Client is a minimal synchronous RESP client over a single TCP connection.
// It speaks just enough of the protocol to send a command and read the reply
// frame, which is all a load generator needs. It is not safe for concurrent use;
// the load generator gives each connection its own Client.
type Client struct {
	conn *countingConn
	r    *bufio.Reader
	w    *bufio.Writer
}

// countingConn wraps the TCP connection and counts the bytes that actually
// cross it in each direction. Wire bytes are the CF20 readback: on giant-value
// rows both servers can sit at the box's copy ceiling and the ops/s ratio
// compresses toward 1x regardless of software quality, so every row records
// bytes/s next to ops/s and a bandwidth-bound cell is judged on p99 and RSS
// instead of being declared a throughput tie. Counting at the conn, under the
// bufio layers, measures what hit the socket rather than what the generator
// intended. The counters are plain fields because a Client is single-goroutine
// by contract and the runner reads the totals only after its workers join.
type countingConn struct {
	net.Conn
	read    int64
	written int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written += int64(n)
	return n, err
}

// BytesOnWire returns the bytes this client has read from and written to the
// socket so far.
func (c *Client) BytesOnWire() (read, written int64) {
	return c.conn.read, c.conn.written
}

// Dial connects to addr (host:port) with the given timeout and returns a ready
// Client.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	cc := &countingConn{Conn: conn}
	return &Client{
		conn: cc,
		r:    bufio.NewReaderSize(cc, 64*1024),
		w:    bufio.NewWriterSize(cc, 64*1024),
	}, nil
}

// Close shuts the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// WriteCommand frames argv as a RESP multibulk and buffers it. Nothing reaches
// the socket until Flush, so a pipeline of commands batches into one write.
func (c *Client) WriteCommand(argv [][]byte) error {
	if _, err := c.w.WriteString("*" + strconv.Itoa(len(argv)) + "\r\n"); err != nil {
		return err
	}
	for _, a := range argv {
		if _, err := c.w.WriteString("$" + strconv.Itoa(len(a)) + "\r\n"); err != nil {
			return err
		}
		if _, err := c.w.Write(a); err != nil {
			return err
		}
		if _, err := c.w.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// Flush pushes any buffered commands to the socket.
func (c *Client) Flush() error { return c.w.Flush() }

// ReadReply reads and discards exactly one complete reply frame, returning an
// error only on a protocol or transport failure. The load generator cares about
// timing and error status, not reply contents, so the bytes are not retained.
func (c *Client) ReadReply() error {
	_, err := c.ReadReplyValue()
	return err
}

// ReplyKind classifies one reply frame for the runner's counters. The split
// exists because "the server answered" and "the server returned the data" are
// different events, and a benchmark that counts them the same lets an engine
// that evicted half its keyspace outscore one that kept it: the eviction
// victim answers nil at RAM speed while the retaining engine pays the disk
// read. The f3 M0 LTM cell measured exactly that (tamnd/aki#542).
type ReplyKind int

const (
	// ReplyValue is a reply that carries data or a real acknowledgement: a
	// bulk string, simple string, integer, double, bool, or aggregate.
	ReplyValue ReplyKind = iota
	// ReplyNil is a RESP null: the server answered, but with no data. On a
	// read workload over a populated keyspace this is a miss.
	ReplyNil
	// ReplyErr is a server error reply (-ERR ..., -OOM ...): the command was
	// refused. A SET that bounces off maxmemory under noeviction is not a
	// completed write.
	ReplyErr
)

// ReadReplyKind reads exactly one reply frame, discards the contents, and
// returns its classification. Transport and protocol failures come back as the
// error, exactly like ReadReply; a server error reply is a ReplyErr result,
// not an error, because the connection is still healthy.
func (c *Client) ReadReplyKind() (ReplyKind, error) {
	v, err := c.ReadReplyValue()
	if err != nil {
		return ReplyValue, err
	}
	switch v.(type) {
	case nil:
		return ReplyNil, nil
	case replyError:
		return ReplyErr, nil
	}
	return ReplyValue, nil
}

// ReadReplyValue reads one reply frame and returns it as a decoded value. It
// understands the RESP2 and the common RESP3 type bytes so a target running in
// either mode parses cleanly. The smoke check uses it to compare replies; the
// load generator uses the lighter ReadReply.
func (c *Client) ReadReplyValue() (any, error) {
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, errors.New("empty reply line")
	}
	switch line[0] {
	case '+': // simple string
		return string(line[1:]), nil
	case '-': // error
		return replyError(string(line[1:])), nil
	case ':': // integer
		return strconv.ParseInt(string(line[1:]), 10, 64)
	case ',': // RESP3 double
		return string(line[1:]), nil
	case '#': // RESP3 bool
		return len(line) > 1 && line[1] == 't', nil
	case '_': // RESP3 null
		return nil, nil
	case '(': // RESP3 big number
		return string(line[1:]), nil
	case '$', '=', '!': // bulk string, verbatim, bulk error
		return c.readBulk(line[1:])
	case '*', '~', '>': // array, set, push
		return c.readAggregate(line[1:])
	case '%': // map: 2n following values
		return c.readMap(line[1:])
	default:
		return nil, fmt.Errorf("unexpected reply type byte %q", line[0])
	}
}

// replyError marks a server-returned error reply so callers can distinguish it
// from a transport error.
type replyError string

func (e replyError) Error() string { return string(e) }

func (c *Client) readBulk(lenField []byte) (any, error) {
	n, err := strconv.ParseInt(string(lenField), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad bulk length: %w", err)
	}
	if n < 0 {
		return nil, nil // null bulk
	}
	buf := make([]byte, n+2) // payload plus trailing CRLF
	if _, err := readFull(c.r, buf); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (c *Client) readAggregate(lenField []byte) (any, error) {
	n, err := strconv.ParseInt(string(lenField), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad aggregate length: %w", err)
	}
	if n < 0 {
		return nil, nil // null array
	}
	out := make([]any, 0, n)
	for i := int64(0); i < n; i++ {
		v, err := c.ReadReplyValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *Client) readMap(lenField []byte) (any, error) {
	n, err := strconv.ParseInt(string(lenField), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad map length: %w", err)
	}
	out := make([]any, 0, n*2)
	for i := int64(0); i < n*2; i++ {
		v, err := c.ReadReplyValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// readLine reads up to and including a CRLF and returns the line without it.
func (c *Client) readLine() ([]byte, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(line) >= 2 && line[len(line)-2] == '\r' {
		return line[:len(line)-2], nil
	}
	return line[:len(line)-1], nil
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
