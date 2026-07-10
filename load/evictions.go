package load

import (
	"bytes"
	"fmt"
	"strconv"
	"time"
)

// EvictionStats is the server's own account of what happened to reads and to
// the keyspace under a memory cap: keyspace hits and misses, keys evicted, and
// the cap and policy in force. It exists for the larger-than-memory rows,
// where the client-side reply split (nil vs value) needs a server-side
// corroboration: a capped rival that evicted half the keyspace shows it here
// as a large evicted_keys and a miss count that matches the nils the client
// saw. The M0 LTM cell had no such readback and the eviction went unnoticed
// until the raw ab JSONs were re-read (tamnd/aki#542).
type EvictionStats struct {
	KeyspaceHits    int64  // reads answered with data
	KeyspaceMisses  int64  // reads answered with a nil
	EvictedKeys     int64  // keys dropped by the maxmemory policy
	Maxmemory       int64  // configured cap in bytes, 0 when uncapped
	MaxmemoryPolicy string // e.g. allkeys-lfu, noeviction
}

// Sub returns the counter deltas from an earlier snapshot, keeping the
// configuration fields from the later one. Counters are cumulative since
// server start, so a window is the difference of two snapshots.
func (s EvictionStats) Sub(earlier EvictionStats) EvictionStats {
	return EvictionStats{
		KeyspaceHits:    s.KeyspaceHits - earlier.KeyspaceHits,
		KeyspaceMisses:  s.KeyspaceMisses - earlier.KeyspaceMisses,
		EvictedKeys:     s.EvictedKeys - earlier.EvictedKeys,
		Maxmemory:       s.Maxmemory,
		MaxmemoryPolicy: s.MaxmemoryPolicy,
	}
}

// ProbeEvictionStats connects to addr, asks for the full INFO, and returns the
// eviction-relevant fields. Like the other probes it dials its own short-lived
// connection off the measured path. A server that does not serve INFO (f3srv
// in M0) returns an error and the caller leaves the columns empty.
func ProbeEvictionStats(addr string, timeout time.Duration) (EvictionStats, error) {
	c, err := Dial(addr, timeout)
	if err != nil {
		return EvictionStats{}, err
	}
	defer c.Close()

	if timeout > 0 {
		_ = c.conn.SetDeadline(time.Now().Add(timeout))
	}
	if err := c.WriteCommand([][]byte{[]byte("INFO")}); err != nil {
		return EvictionStats{}, err
	}
	if err := c.Flush(); err != nil {
		return EvictionStats{}, err
	}
	reply, err := c.ReadReplyValue()
	if err != nil {
		return EvictionStats{}, err
	}
	body, ok := reply.([]byte)
	if !ok {
		return EvictionStats{}, fmt.Errorf("INFO returned %T, want a bulk string", reply)
	}
	return parseEvictionStats(body), nil
}

// parseEvictionStats pulls the eviction fields out of an INFO payload.
// Unknown or absent fields stay zero; a malformed number is treated as absent
// rather than failing the probe, because these are diagnostics and a partial
// readback beats none.
func parseEvictionStats(body []byte) EvictionStats {
	var st EvictionStats
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := string(line[:colon])
		value := string(line[colon+1:])
		switch field {
		case "keyspace_hits":
			st.KeyspaceHits, _ = strconv.ParseInt(value, 10, 64)
		case "keyspace_misses":
			st.KeyspaceMisses, _ = strconv.ParseInt(value, 10, 64)
		case "evicted_keys":
			st.EvictedKeys, _ = strconv.ParseInt(value, 10, 64)
		case "maxmemory":
			st.Maxmemory, _ = strconv.ParseInt(value, 10, 64)
		case "maxmemory_policy":
			st.MaxmemoryPolicy = value
		}
	}
	return st
}
