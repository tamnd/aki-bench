package load

import (
	"bytes"
	"fmt"
	"strconv"
	"time"
)

// ServerInfo is the identity a probe reads back from a running target, so a
// report records the exact server it measured rather than the version the
// operator assumed was on PATH. The distinction matters: a box can carry an old
// redis-server next to a new one, and a run that benches 7.4 while the report
// header says 8.8 is worse than no number at all.
type ServerInfo struct {
	Software string // redis, valkey, or empty when the server does not say
	Version  string // the version string, e.g. 8.8.0
}

// String renders the identity as "software version", or just the version when
// the software name is unknown, or "unknown" when neither is set.
func (s ServerInfo) String() string {
	switch {
	case s.Software != "" && s.Version != "":
		return s.Software + " " + s.Version
	case s.Version != "":
		return s.Version
	default:
		return "unknown"
	}
}

// ProbeServer connects to addr, asks for INFO server, and returns the server's
// self-reported identity. It is a one-shot diagnostic the harness runs once per
// target before the load, not part of the measured path, so it dials its own
// short-lived connection and does not touch the load Clients.
func ProbeServer(addr string, timeout time.Duration) (ServerInfo, error) {
	c, err := Dial(addr, timeout)
	if err != nil {
		return ServerInfo{}, err
	}
	defer c.Close()

	if timeout > 0 {
		_ = c.conn.SetDeadline(time.Now().Add(timeout))
	}
	if err := c.WriteCommand([][]byte{[]byte("INFO"), []byte("server")}); err != nil {
		return ServerInfo{}, err
	}
	if err := c.Flush(); err != nil {
		return ServerInfo{}, err
	}
	reply, err := c.ReadReplyValue()
	if err != nil {
		return ServerInfo{}, err
	}
	body, ok := reply.([]byte)
	if !ok {
		return ServerInfo{}, fmt.Errorf("INFO server returned %T, want a bulk string", reply)
	}
	return parseServerInfo(body), nil
}

// ProbeUsedMemory connects to addr, asks for INFO memory, and returns the
// server's self-reported used_memory in bytes. It is the F14 memory column's
// apples-to-apples half: used_memory is the server's own accounting, reported
// next to RSS so allocator slack cannot hide behind a clean number. Servers
// that do not serve INFO (f3srv in M0) return an error, and the caller leaves
// the column empty rather than faking it.
func ProbeUsedMemory(addr string, timeout time.Duration) (int64, error) {
	c, err := Dial(addr, timeout)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	if timeout > 0 {
		_ = c.conn.SetDeadline(time.Now().Add(timeout))
	}
	if err := c.WriteCommand([][]byte{[]byte("INFO"), []byte("memory")}); err != nil {
		return 0, err
	}
	if err := c.Flush(); err != nil {
		return 0, err
	}
	reply, err := c.ReadReplyValue()
	if err != nil {
		return 0, err
	}
	body, ok := reply.([]byte)
	if !ok {
		return 0, fmt.Errorf("INFO memory returned %T, want a bulk string", reply)
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if v, found := bytes.CutPrefix(line, []byte("used_memory:")); found {
			n, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("bad used_memory %q: %w", v, err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("INFO memory reply has no used_memory field")
}

// parseServerInfo pulls the software name and version out of an INFO server
// payload. The block is CRLF-joined "field:value" lines. redis_version is
// present on Redis and Valkey alike (Valkey kept the field for drop-in
// compatibility), so the software name comes from the dedicated field when the
// server exposes one and falls back to redis otherwise.
func parseServerInfo(body []byte) ServerInfo {
	var info ServerInfo
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := string(line[:colon])
		value := string(line[colon+1:])
		switch field {
		case "redis_version":
			if info.Version == "" {
				info.Version = value
			}
			if info.Software == "" {
				info.Software = "redis"
			}
		case "valkey_version":
			// Valkey ships valkey_version alongside redis_version; prefer it so
			// the report names valkey, not the compatibility-shim redis_version.
			info.Version = value
			info.Software = "valkey"
		case "server_name":
			// Some forks announce themselves here (e.g. valkey); trust it over
			// the redis default but not over an explicit valkey_version.
			if value != "" && info.Software != "valkey" {
				info.Software = value
			}
		}
	}
	return info
}
