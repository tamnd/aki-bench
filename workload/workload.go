// Package workload defines the standard command mixes the benchmark drives.
// Each workload turns into a load.CommandGen that the runner calls once per
// operation. Workloads are parameterized by value size, key count (the key
// space), and for the mixed case a read/write ratio, so the same definition
// covers small-value latency runs and key counts large enough to exceed RAM and
// exercise larger-than-memory behavior.
package workload

import (
	"strconv"

	"github.com/tamnd/aki-bench/load"
)

// Spec parameterizes a workload.
type Spec struct {
	Name      string // workload name, for example "set" or "mixed-8020"
	ValueSize int    // payload size in bytes for write commands
	KeyCount  int    // size of the key space; keys cycle 0..KeyCount-1
	ReadRatio int    // percent of operations that are reads, mixed workload only
}

func (s Spec) withDefaults() Spec {
	if s.ValueSize <= 0 {
		s.ValueSize = 16
	}
	if s.KeyCount <= 0 {
		s.KeyCount = 100000
	}
	return s
}

// value builds a deterministic payload of the configured size. The content does
// not matter to the server, only the length, so a repeated byte keeps allocation
// cheap and reproducible.
func value(size int) []byte {
	v := make([]byte, size)
	for i := range v {
		v[i] = 'x'
	}
	return v
}

// key returns the key for a given sequence number within the key space.
// Keys are spread across the space by the sequence so a run touches the whole
// keyspace evenly rather than hammering one slot.
func key(prefix string, seq int64, keyCount int) []byte {
	idx := seq % int64(keyCount)
	return []byte(prefix + strconv.FormatInt(idx, 10))
}

// Registry returns the standard suite keyed by base name. Each entry is a
// constructor that applies the given value size and key count.
func Registry() map[string]func(Spec) load.CommandGen {
	return map[string]func(Spec) load.CommandGen{
		"get":   Get,
		"set":   Set,
		"incr":  Incr,
		"lpush": LPush,
		"rpush": RPush,
		"sadd":  SAdd,
		"zadd":  ZAdd,
		"hset":  HSet,
		"mset":  MSet,
		"mixed": Mixed,
	}
}

// Names lists the standard workload names in a stable order.
func Names() []string {
	return []string{"get", "set", "incr", "lpush", "rpush", "sadd", "zadd", "hset", "mset", "mixed"}
}

// Get reads keys across the key space. It assumes the keys were populated, so a
// fair GET run should be preceded by a SET pass over the same key space.
func Get(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("GET")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, key("key:", seq, s.KeyCount)}
	}
}

// Set writes a fixed-size value across the key space.
func Set(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("SET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, key("key:", seq, s.KeyCount), val}
	}
}

// Incr increments counters across the key space.
func Incr(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("INCR")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, key("ctr:", seq, s.KeyCount)}
	}
}

// LPush prepends to lists across the key space.
func LPush(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("LPUSH")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, key("list:", seq, s.KeyCount), val}
	}
}

// RPush appends to lists across the key space.
func RPush(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("RPUSH")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, key("list:", seq, s.KeyCount), val}
	}
}

// SAdd adds members to sets across the key space. The member varies by sequence
// so sets actually grow rather than re-adding the same element.
func SAdd(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("SADD")
	return func(conn int, seq int64) [][]byte {
		member := []byte("m" + strconv.FormatInt(seq, 10))
		return [][]byte{cmd, key("set:", seq, s.KeyCount), member}
	}
}

// ZAdd adds scored members to sorted sets across the key space.
func ZAdd(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("ZADD")
	return func(conn int, seq int64) [][]byte {
		score := []byte(strconv.FormatInt(seq, 10))
		member := []byte("m" + strconv.FormatInt(seq, 10))
		return [][]byte{cmd, key("zset:", seq, s.KeyCount), score, member}
	}
}

// HSet sets a field on hashes across the key space.
func HSet(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("HSET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		field := []byte("f" + strconv.FormatInt(seq%64, 10))
		return [][]byte{cmd, key("hash:", seq, s.KeyCount), field, val}
	}
}

// MSet writes three keys per command across the key space.
func MSet(s Spec) load.CommandGen {
	s = s.withDefaults()
	cmd := []byte("MSET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{
			cmd,
			key("key:", seq*3, s.KeyCount), val,
			key("key:", seq*3+1, s.KeyCount), val,
			key("key:", seq*3+2, s.KeyCount), val,
		}
	}
}

// Mixed interleaves GET and SET at the configured read ratio. With ReadRatio 80
// it issues four reads for every write, the common cache-style profile.
func Mixed(s Spec) load.CommandGen {
	s = s.withDefaults()
	ratio := s.ReadRatio
	if ratio <= 0 || ratio >= 100 {
		ratio = 80
	}
	get := []byte("GET")
	set := []byte("SET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		if int(seq%100) < ratio {
			return [][]byte{get, key("key:", seq, s.KeyCount)}
		}
		return [][]byte{set, key("key:", seq, s.KeyCount), val}
	}
}

// Build returns the generator for a base workload name, or nil if unknown.
func Build(name string, s Spec) load.CommandGen {
	if ctor, ok := Registry()[name]; ok {
		return ctor(s)
	}
	return nil
}

// ValueSizeSweep is the default set of payload sizes to sweep.
func ValueSizeSweep() []int { return []int{16, 64, 256, 1024, 4096} }

// KeySweep is the default set of key counts to sweep. The last entry is
// deliberately large so a run with small values still produces a dataset that
// exceeds a modest RAM budget and exercises larger-than-memory paths.
func KeySweep() []int { return []int{1000, 100000, 1000000, 50000000} }
