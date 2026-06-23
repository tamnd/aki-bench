package load_test

import (
	"bufio"
	"net"
	"strconv"
	"sync"
)

// fakeServer is a tiny in-process RESP server used by the unit tests so they run
// with no real Redis, Valkey, or aki present. It understands just enough of the
// protocol to parse a multibulk command and answer the handful of commands the
// workloads and smoke check send. It keeps a string keyspace so GET, SET, INCR,
// and EXPIRE return believable replies.
type fakeServer struct {
	ln   net.Listener
	wg   sync.WaitGroup
	mu   sync.Mutex
	data map[string]string
}

func newFakeServer() (*fakeServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &fakeServer{ln: ln, data: map[string]string{}}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) close() {
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *fakeServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		argv, err := readCommand(r)
		if err != nil {
			return
		}
		if len(argv) == 0 {
			continue
		}
		s.reply(w, argv)
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// readCommand parses one RESP multibulk command frame.
func readCommand(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(header) == 0 || header[0] != '*' {
		return nil, errBadFrame
	}
	n, err := strconv.Atoi(trimCRLF(header[1:]))
	if err != nil {
		return nil, err
	}
	argv := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lenLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(lenLine) == 0 || lenLine[0] != '$' {
			return nil, errBadFrame
		}
		blen, err := strconv.Atoi(trimCRLF(lenLine[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, blen+2)
		if _, err := readFull(r, buf); err != nil {
			return nil, err
		}
		argv = append(argv, string(buf[:blen]))
	}
	return argv, nil
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

func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errBadFrame = fakeErr("bad frame")

func (s *fakeServer) reply(w *bufio.Writer, argv []string) {
	switch up(argv[0]) {
	case "PING":
		writeSimple(w, "PONG")
	case "SET", "MSET":
		s.mu.Lock()
		for i := 1; i+1 < len(argv); i += 2 {
			s.data[argv[i]] = argv[i+1]
		}
		s.mu.Unlock()
		writeSimple(w, "OK")
	case "GET":
		s.mu.Lock()
		v, ok := s.data[argv[1]]
		s.mu.Unlock()
		if !ok {
			writeNull(w)
		} else {
			writeBulk(w, v)
		}
	case "INCR":
		s.mu.Lock()
		n, _ := strconv.ParseInt(s.data[argv[1]], 10, 64)
		n++
		s.data[argv[1]] = strconv.FormatInt(n, 10)
		s.mu.Unlock()
		writeInt(w, n)
	case "EXPIRE":
		writeInt(w, 1)
	case "LPUSH", "RPUSH", "SADD", "ZADD", "HSET":
		// These return an integer count in real Redis; one is good enough here.
		writeInt(w, 1)
	default:
		writeSimple(w, "OK")
	}
}

func up(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}

func writeSimple(w *bufio.Writer, s string) { _, _ = w.WriteString("+" + s + "\r\n") }
func writeInt(w *bufio.Writer, n int64) {
	_, _ = w.WriteString(":" + strconv.FormatInt(n, 10) + "\r\n")
}
func writeNull(w *bufio.Writer) { _, _ = w.WriteString("$-1\r\n") }
func writeBulk(w *bufio.Writer, s string) {
	_, _ = w.WriteString("$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n")
}
