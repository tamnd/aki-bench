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
	Name      string  // workload name, for example "set" or "mixed-8020"
	ValueSize int     // payload size in bytes for write commands
	KeyCount  int     // size of the key space; keys cycle 0..KeyCount-1
	ReadRatio int     // percent of operations that are reads, mixed workload only
	Members   int     // member space for a single-collection probe (sismember, hget, ...)
	Dist      string  // access pattern over the space: "uniform" (default) or "zipfian"
	ZipfS     float64 // zipfian skew exponent, used when Dist is "zipfian"; 0 means 0.99
}

func (s Spec) withDefaults() Spec {
	if s.ValueSize <= 0 {
		s.ValueSize = 16
	}
	if s.KeyCount <= 0 {
		s.KeyCount = 100000
	}
	if s.Members <= 0 {
		s.Members = s.KeyCount
	}
	if s.ZipfS <= 0 {
		s.ZipfS = 0.99
	}
	return s
}

// keySelector returns the access pattern over the top-level key space. Uniform is
// the default and the historical behavior; zipfian concentrates traffic on a hot
// head of keys, which is the shape a read cache and the F2 hot tier are built for.
func (s Spec) keySelector() Selector {
	if s.Dist == "zipfian" {
		return zipfianSelector(int64(s.KeyCount), s.ZipfS)
	}
	return uniformSelector(int64(s.KeyCount))
}

// memberSelector returns the access pattern over a single collection's member
// space, used by the collection point-read probes (sismember, hget, zscore,
// zrank). Same uniform-or-zipfian choice as keySelector, over Members rather than
// KeyCount.
func (s Spec) memberSelector() Selector {
	if s.Dist == "zipfian" {
		return zipfianSelector(int64(s.Members), s.ZipfS)
	}
	return uniformSelector(int64(s.Members))
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

// keyAt returns the key for a given key-space index. The index is produced by a
// Selector, so the access pattern (uniform or zipfian) lives in one place and the
// generators just format whatever slot the selector picks.
func keyAt(prefix string, idx int64) []byte {
	return []byte(prefix + strconv.FormatInt(idx, 10))
}

// Registry returns the standard suite keyed by base name. Each entry is a
// constructor that applies the given value size and key count.
func Registry() map[string]func(Spec) load.CommandGen {
	return map[string]func(Spec) load.CommandGen{
		"get":      Get,
		"getrange": GetRange,
		"set":      Set,
		"incr":     Incr,
		"lpush":    LPush,
		"rpush":    RPush,
		"sadd":     SAdd,
		"zadd":     ZAdd,
		"hset":     HSet,
		"mset":     MSet,
		"mixed":    Mixed,
	}
}

// Names lists the standard workload names in a stable order.
func Names() []string {
	return []string{"get", "getrange", "set", "incr", "lpush", "rpush", "sadd", "zadd", "hset", "mset", "mixed"}
}

// Get reads keys across the key space. It assumes the keys were populated, so a
// fair GET run should be preceded by a SET pass over the same key space.
func Get(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("GET")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, keyAt("key:", sel(seq))}
	}
}

// GetRange reads a bounded window out of a preloaded value with GETRANGE. It is
// the string range workload, and it only gets interesting in the larger-than-
// memory large-value case: when the value is bigger than the engine's inline
// limit, a windowed read must touch only the chunks the window spans, not
// materialize the whole value (see the string model spec). Run it with a large
// -value-size so the window sits inside a value that does not fit inline. The
// window is rangeWindow bytes and its start walks across the value by sequence so
// reads are spread over the whole value, not pinned to the inline-served head.
// It reads the same "key:" space Get and the get preload populate.
func GetRange(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("GETRANGE")
	w := int64(rangeWindow)
	maxStart := max(int64(s.ValueSize)-w, 0)
	return func(conn int, seq int64) [][]byte {
		var start int64
		if maxStart > 0 {
			start = (seq * 2654435761) % (maxStart + 1)
			if start < 0 {
				start += maxStart + 1
			}
		}
		end := start + w - 1
		return [][]byte{cmd, keyAt("key:", sel(seq)), intArg(start), intArg(end)}
	}
}

// Set writes a fixed-size value across the key space.
func Set(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("SET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, keyAt("key:", sel(seq)), val}
	}
}

// Incr increments counters across the key space.
func Incr(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("INCR")
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, keyAt("ctr:", sel(seq))}
	}
}

// LPush prepends to lists across the key space.
func LPush(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("LPUSH")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, keyAt("list:", sel(seq)), val}
	}
}

// RPush appends to lists across the key space.
func RPush(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("RPUSH")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{cmd, keyAt("list:", sel(seq)), val}
	}
}

// SAdd adds members to sets across the key space. The member varies by sequence
// so sets actually grow rather than re-adding the same element.
func SAdd(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("SADD")
	return func(conn int, seq int64) [][]byte {
		member := []byte("m" + strconv.FormatInt(seq, 10))
		return [][]byte{cmd, keyAt("set:", sel(seq)), member}
	}
}

// ZAdd adds scored members to sorted sets across the key space.
func ZAdd(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("ZADD")
	return func(conn int, seq int64) [][]byte {
		score := []byte(strconv.FormatInt(seq, 10))
		member := []byte("m" + strconv.FormatInt(seq, 10))
		return [][]byte{cmd, keyAt("zset:", sel(seq)), score, member}
	}
}

// HSet sets a field on hashes across the key space.
func HSet(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("HSET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		field := []byte("f" + strconv.FormatInt(seq%64, 10))
		return [][]byte{cmd, keyAt("hash:", sel(seq)), field, val}
	}
}

// MSet writes three keys per command across the key space.
func MSet(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	cmd := []byte("MSET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		return [][]byte{
			cmd,
			keyAt("key:", sel(seq*3)), val,
			keyAt("key:", sel(seq*3+1)), val,
			keyAt("key:", sel(seq*3+2)), val,
		}
	}
}

// Mixed interleaves GET and SET at the configured read ratio. With ReadRatio 80
// it issues four reads for every write, the common cache-style profile.
func Mixed(s Spec) load.CommandGen {
	s = s.withDefaults()
	sel := s.keySelector()
	ratio := s.ReadRatio
	if ratio <= 0 || ratio >= 100 {
		ratio = 80
	}
	get := []byte("GET")
	set := []byte("SET")
	val := value(s.ValueSize)
	return func(conn int, seq int64) [][]byte {
		if int(seq%100) < ratio {
			return [][]byte{get, keyAt("key:", sel(seq))}
		}
		return [][]byte{set, keyAt("key:", sel(seq)), val}
	}
}

// Build returns the generator for a base workload name, or nil if unknown.
func Build(name string, s Spec) load.CommandGen {
	if ctor, ok := Registry()[name]; ok {
		return ctor(s)
	}
	return nil
}

// PreloadFor returns a write generator that populates the key space a flat read
// workload needs before the timed run, the number of preload ops, and true; or
// ok=false for workloads that create their own keys (writes) and so need no
// preload. GET, GETRANGE, and the mixed profile read keys that must already
// exist: without a preload every read is a miss that short-circuits before
// touching storage, so
// the comparison would measure the miss path, not a real read. The preload
// writes one SET per key over the whole key space; driven by a single connection
// it walks seq 0..KeyCount-1 so every key is written exactly once, the same
// coverage rule the collection plans use.
func PreloadFor(name string, s Spec) (load.CommandGen, int64, bool) {
	s = s.withDefaults()
	switch name {
	case "get", "getrange", "mixed":
		set := []byte("SET")
		val := value(s.ValueSize)
		n := int64(s.KeyCount)
		return func(conn int, seq int64) [][]byte {
			return [][]byte{set, keyAt("key:", seq%n), val}
		}, n, true
	default:
		return nil, 0, false
	}
}

// ValueSizeSweep is the default set of payload sizes to sweep.
func ValueSizeSweep() []int { return []int{16, 64, 256, 1024, 4096} }

// KeySweep is the default set of key counts to sweep. The last entry is
// deliberately large so a run with small values still produces a dataset that
// exceeds a modest RAM budget and exercises larger-than-memory paths.
func KeySweep() []int { return []int{1000, 100000, 1000000, 50000000} }
