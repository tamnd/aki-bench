// Package smoke is a tiny compatibility check, not the deep compat suite.
// It runs a handful of round-trips (PING, SET, GET, INCR, EXPIRE) against every
// configured target and checks that the replies agree, so the bench repo can gate
// on basic wire compatibility before it trusts a throughput number. The full
// behavioral compatibility suite lives in tamnd/aki-compat; this is only enough
// to catch a target that is plainly wrong.
package smoke

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/tamnd/aki-bench/load"
)

// Check is one smoke probe against one target.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Result is the smoke outcome for one target.
type Result struct {
	Target string
	Checks []Check
}

// Pass reports whether every check on the target passed.
func (r Result) Pass() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Run executes the smoke probes against addr and returns the result. A dial or
// transport failure surfaces as a single failing check rather than an error, so
// the caller can render it in the same table as the rest.
func Run(targetName, addr string) Result {
	res := Result{Target: targetName}
	cl, err := load.Dial(addr, 3*time.Second)
	if err != nil {
		res.Checks = append(res.Checks, Check{Name: "connect", OK: false, Detail: err.Error()})
		return res
	}
	defer cl.Close()

	// Use a unique key prefix so repeated runs and parallel targets do not
	// collide on shared servers.
	prefix := "smoke:" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":"

	res.Checks = append(res.Checks,
		probe(cl, "PING", [][]byte{[]byte("PING")}, isSimple("PONG")),
		probe(cl, "SET", [][]byte{[]byte("SET"), []byte(prefix + "k"), []byte("v1")}, isSimple("OK")),
		probe(cl, "GET", [][]byte{[]byte("GET"), []byte(prefix + "k")}, isBulk("v1")),
		probe(cl, "INCR", [][]byte{[]byte("INCR"), []byte(prefix + "n")}, isInt(1)),
		probe(cl, "INCR-again", [][]byte{[]byte("INCR"), []byte(prefix + "n")}, isInt(2)),
		probe(cl, "EXPIRE", [][]byte{[]byte("EXPIRE"), []byte(prefix + "k"), []byte("100")}, isInt(1)),
	)
	return res
}

// RunStrings executes a strings-only smoke against addr. It exists for the f3
// engine, whose server (f3srv) serves the M0 string surface only: SET, GET, the
// INCR family, APPEND, SETRANGE/GETRANGE, DEL, EXISTS, STRLEN, TYPE, PING, and
// ECHO. The default Run probes EXPIRE, which f3srv does not serve yet, so an f3
// smoke needs a probe set that stays inside the served surface. The probes still
// exercise real round-trips with checked replies, not just PING, so a server
// that parses but mis-executes fails here rather than in a throughput number.
func RunStrings(targetName, addr string) Result {
	res := Result{Target: targetName}
	cl, err := load.Dial(addr, 3*time.Second)
	if err != nil {
		res.Checks = append(res.Checks, Check{Name: "connect", OK: false, Detail: err.Error()})
		return res
	}
	defer cl.Close()

	prefix := "smoke:" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":"

	res.Checks = append(res.Checks,
		probe(cl, "PING", [][]byte{[]byte("PING")}, isSimple("PONG")),
		probe(cl, "ECHO", [][]byte{[]byte("ECHO"), []byte("hi")}, isBulk("hi")),
		probe(cl, "SET", [][]byte{[]byte("SET"), []byte(prefix + "k"), []byte("v1")}, isSimple("OK")),
		probe(cl, "GET", [][]byte{[]byte("GET"), []byte(prefix + "k")}, isBulk("v1")),
		probe(cl, "APPEND", [][]byte{[]byte("APPEND"), []byte(prefix + "k"), []byte("v2")}, isInt(4)),
		probe(cl, "STRLEN", [][]byte{[]byte("STRLEN"), []byte(prefix + "k")}, isInt(4)),
		probe(cl, "GETRANGE", [][]byte{[]byte("GETRANGE"), []byte(prefix + "k"), []byte("2"), []byte("3")}, isBulk("v2")),
		probe(cl, "INCR", [][]byte{[]byte("INCR"), []byte(prefix + "n")}, isInt(1)),
		probe(cl, "INCR-again", [][]byte{[]byte("INCR"), []byte(prefix + "n")}, isInt(2)),
		probe(cl, "TYPE", [][]byte{[]byte("TYPE"), []byte(prefix + "k")}, isSimple("string")),
		probe(cl, "EXISTS", [][]byte{[]byte("EXISTS"), []byte(prefix + "k")}, isInt(1)),
		probe(cl, "DEL", [][]byte{[]byte("DEL"), []byte(prefix + "k")}, isInt(1)),
	)
	return res
}

// matcher decides whether a reply value is the expected one.
type matcher func(v any) (bool, string)

func probe(cl *load.Client, name string, argv [][]byte, want matcher) Check {
	if err := cl.WriteCommand(argv); err != nil {
		return Check{Name: name, OK: false, Detail: "write: " + err.Error()}
	}
	if err := cl.Flush(); err != nil {
		return Check{Name: name, OK: false, Detail: "flush: " + err.Error()}
	}
	v, err := cl.ReadReplyValue()
	if err != nil {
		return Check{Name: name, OK: false, Detail: "read: " + err.Error()}
	}
	ok, detail := want(v)
	return Check{Name: name, OK: ok, Detail: detail}
}

func isSimple(want string) matcher {
	return func(v any) (bool, string) {
		s, ok := v.(string)
		if !ok {
			return false, fmt.Sprintf("want simple %q, got %T", want, v)
		}
		if s != want {
			return false, fmt.Sprintf("want %q, got %q", want, s)
		}
		return true, ""
	}
}

func isBulk(want string) matcher {
	return func(v any) (bool, string) {
		b, ok := v.([]byte)
		if !ok {
			return false, fmt.Sprintf("want bulk %q, got %T", want, v)
		}
		if !bytes.Equal(b, []byte(want)) {
			return false, fmt.Sprintf("want %q, got %q", want, string(b))
		}
		return true, ""
	}
}

func isInt(want int64) matcher {
	return func(v any) (bool, string) {
		n, ok := v.(int64)
		if !ok {
			return false, fmt.Sprintf("want int %d, got %T", want, v)
		}
		if n != want {
			return false, fmt.Sprintf("want %d, got %d", want, n)
		}
		return true, ""
	}
}
