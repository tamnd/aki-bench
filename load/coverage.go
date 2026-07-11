package load

import (
	"math/rand"
	"strconv"
	"time"
)

// RetrievabilitySpec parameterizes a post-run dataset-coverage probe over a flat
// string keyspace. The probe draws Sample random key indices from the full
// 0..KeyCount-1 space, GETs each, and checks the reply is a value of exactly
// ValueSize bytes all equal to Fill. It is the LTM fairness check the M0 gate
// lacked: a rival capped at maxmemory with an allkeys eviction policy answers
// nil for every key it dropped, so a run that posts high ops/s can have silently
// lost half its dataset. Sampling the whole keyspace after the run makes that
// loss a number instead of an assumption (tamnd/aki#542).
type RetrievabilitySpec struct {
	Addr      string        // host:port of the server to probe
	KeyPrefix string        // key format prefix, "key:" for the flat string workloads
	KeyCount  int           // full keyspace size; indices are drawn from 0..KeyCount-1
	ValueSize int           // expected byte length of every stored value
	Fill      byte          // expected repeated content byte the writer used
	Sample    int           // number of random keys to draw and check
	Seed      int64         // RNG seed for a reproducible sample; 0 uses the wall clock
	Timeout   time.Duration // per-probe deadline; 0 leaves the connection with no deadline
}

// RetrievabilityResult is the outcome of a coverage probe. Sampled is the number
// of keys actually checked; the other four partition it. Only Retrievable keys
// came back correct; the rest are the dataset the server no longer serves the way
// it was written.
type RetrievabilityResult struct {
	Sampled     int // keys checked
	Retrievable int // present, right length, right content
	Missing     int // nil reply: the key is gone
	WrongLength int // present but not ValueSize bytes: truncated or grown
	Corrupt     int // right length, wrong content byte, or a non-value reply
}

// Fraction returns the share of the sample that came back retrievable, the
// dataset-coverage figure. An empty sample reports 0.
func (r RetrievabilityResult) Fraction() float64 {
	if r.Sampled <= 0 {
		return 0
	}
	return float64(r.Retrievable) / float64(r.Sampled)
}

// coverageBatch is how many GETs the probe pipelines before reading their
// replies. A random sample over a million-key space is otherwise one
// round-trip per key, which makes a 10k-key probe take thousands of serial
// RTTs; batching keeps the probe short without changing what it measures.
const coverageBatch = 256

// ProbeRetrievability samples Sample random keys from the flat string keyspace
// and reports how many are still retrievable with the exact length and content
// the workload wrote. It runs on its own short-lived connection, off any
// measured path, so it never perturbs a throughput number. A sample larger than
// the keyspace is capped at the keyspace, and a keyspace of zero or a sample of
// zero returns an empty result with no error.
func ProbeRetrievability(spec RetrievabilitySpec) (RetrievabilityResult, error) {
	if spec.KeyCount <= 0 || spec.Sample <= 0 {
		return RetrievabilityResult{}, nil
	}
	sample := spec.Sample
	if sample > spec.KeyCount {
		sample = spec.KeyCount
	}
	seed := spec.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	rng := rand.New(rand.NewSource(seed))

	cl, err := Dial(spec.Addr, 5*time.Second)
	if err != nil {
		return RetrievabilityResult{}, err
	}
	defer cl.Close()
	if spec.Timeout > 0 {
		_ = cl.conn.SetDeadline(time.Now().Add(spec.Timeout))
	}

	get := []byte("GET")
	res := RetrievabilityResult{Sampled: sample}
	for done := 0; done < sample; {
		n := min(coverageBatch, sample-done)
		for i := 0; i < n; i++ {
			idx := rng.Int63n(int64(spec.KeyCount))
			key := []byte(spec.KeyPrefix + strconv.FormatInt(idx, 10))
			if err := cl.WriteCommand([][]byte{get, key}); err != nil {
				return res, err
			}
		}
		if err := cl.Flush(); err != nil {
			return res, err
		}
		for i := 0; i < n; i++ {
			reply, err := cl.ReadReplyValue()
			if err != nil {
				return res, err
			}
			classify(&res, reply, spec.ValueSize, spec.Fill)
		}
		done += n
	}
	return res, nil
}

// classify buckets one GET reply into the result. A nil reply is a dropped key;
// a bulk value is checked for exact length and exact content; anything else (an
// error reply, an integer, an array) is not the string the workload stored and
// counts as corrupt so a misbehaving server cannot inflate the retrievable
// share.
func classify(res *RetrievabilityResult, reply any, valueSize int, fill byte) {
	if reply == nil {
		res.Missing++
		return
	}
	v, ok := reply.([]byte)
	if !ok {
		res.Corrupt++
		return
	}
	if len(v) != valueSize {
		res.WrongLength++
		return
	}
	for _, b := range v {
		if b != fill {
			res.Corrupt++
			return
		}
	}
	res.Retrievable++
}
