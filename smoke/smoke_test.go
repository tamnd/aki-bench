package smoke_test

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tamnd/aki-bench/smoke"
)

// tinyServer answers the smoke probes correctly so the happy path is covered
// without a real Redis. A second mode answers PING wrong so a failing check is
// exercised too.
type tinyServer struct {
	ln       net.Listener
	wg       sync.WaitGroup
	brokenPI bool
	mu       sync.Mutex
	data     map[string]string
}

func newTinyServer(brokenPing bool) (*tinyServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &tinyServer{ln: ln, brokenPI: brokenPing, data: map[string]string{}}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *tinyServer) addr() string { return s.ln.Addr().String() }
func (s *tinyServer) close()       { _ = s.ln.Close(); s.wg.Wait() }

func (s *tinyServer) serve() {
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

func (s *tinyServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		argv, err := readCmd(r)
		if err != nil {
			return
		}
		if len(argv) == 0 {
			continue
		}
		switch strings.ToUpper(argv[0]) {
		case "PING":
			if s.brokenPI {
				w.WriteString("+NOPE\r\n")
			} else {
				w.WriteString("+PONG\r\n")
			}
		case "SET":
			s.mu.Lock()
			s.data[argv[1]] = argv[2]
			s.mu.Unlock()
			w.WriteString("+OK\r\n")
		case "GET":
			s.mu.Lock()
			v, ok := s.data[argv[1]]
			s.mu.Unlock()
			if ok {
				w.WriteString("$" + strconv.Itoa(len(v)) + "\r\n" + v + "\r\n")
			} else {
				w.WriteString("$-1\r\n")
			}
		case "INCR":
			s.mu.Lock()
			n, _ := strconv.ParseInt(s.data[argv[1]], 10, 64)
			n++
			s.data[argv[1]] = strconv.FormatInt(n, 10)
			s.mu.Unlock()
			w.WriteString(":" + strconv.FormatInt(n, 10) + "\r\n")
		case "EXPIRE":
			w.WriteString(":1\r\n")
		default:
			w.WriteString("+OK\r\n")
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func readCmd(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(header) == 0 || header[0] != '*' {
		return nil, errFrame
	}
	n, err := strconv.Atoi(trim(header[1:]))
	if err != nil {
		return nil, err
	}
	argv := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lenLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		blen, err := strconv.Atoi(trim(lenLine[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, blen+2)
		total := 0
		for total < len(buf) {
			m, err := r.Read(buf[total:])
			total += m
			if err != nil {
				return nil, err
			}
		}
		argv = append(argv, string(buf[:blen]))
	}
	return argv, nil
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

type frameErr string

func (e frameErr) Error() string { return string(e) }

const errFrame = frameErr("bad frame")

func TestSmokePasses(t *testing.T) {
	srv, err := newTinyServer(false)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	res := smoke.Run("fake", srv.addr())
	if !res.Pass() {
		for _, c := range res.Checks {
			if !c.OK {
				t.Errorf("check %s failed: %s", c.Name, c.Detail)
			}
		}
		t.Fatal("expected all smoke checks to pass")
	}
}

func TestSmokeFailsOnBadPing(t *testing.T) {
	srv, err := newTinyServer(true)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	res := smoke.Run("fake", srv.addr())
	if res.Pass() {
		t.Fatal("expected smoke to fail on a wrong PING reply")
	}
}

func TestSmokeNoServer(t *testing.T) {
	res := smoke.Run("fake", "127.0.0.1:1")
	if res.Pass() {
		t.Fatal("expected smoke to fail when nothing is listening")
	}
}
