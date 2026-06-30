// Package cpuset partitions a machine's CPU cores between the aki-bench load
// generator and the servers it launches, so a co-located run on a multi-core
// box does not let the client steal the cores the server needs.
//
// This addresses a real measurement flaw. When aki-bench launches a server and
// drives it from the same process on a loaded box, the Go client goroutines and
// the server threads fight for the same cores. The fight depresses the measured
// throughput and understates the ratio: a GET workload that clears 2x under
// redis-benchmark with separate client threads can read as 1.79x under a
// co-located aki-bench, purely because the client starved the server. Pinning
// the two sides to disjoint core sets removes the contention and makes the
// co-located number trustworthy, the same way redis-benchmark --threads N keeps
// its load threads off the server's cores.
//
// The clean fix is still to run the client on a separate box through connect
// mode (-aki-addr and friends). Partitioning is the fix for the common case
// where only one box is available.
package cpuset

import (
	"fmt"
	"strconv"
	"strings"
)

// Partition splits numCPU cores into a server set and a client set that do not
// overlap. The client gets clientCores cores taken from the high end of the
// range and the server gets the rest from the low end, so the two sides sit on
// separate cores the way redis-benchmark keeps its load threads off the server.
// Unlike redis-benchmark's light C threads, aki-bench's Go client needs a
// generous share to avoid becoming the bottleneck, so callers usually size
// clientCores from DefaultClientCores. Both returned values are taskset -c lists
// (for example "0-2" and "3-5").
//
// clientCores is clamped to at least one and to at most numCPU-1 so neither set
// is ever empty. Partition returns an error only when numCPU is below two, where
// there is nothing to split.
func Partition(numCPU, clientCores int) (server, client string, err error) {
	if numCPU < 2 {
		return "", "", fmt.Errorf("cpuset: need at least 2 cores to split, have %d", numCPU)
	}
	if clientCores < 1 {
		clientCores = 1
	}
	if clientCores > numCPU-1 {
		clientCores = numCPU - 1
	}
	serverCores := numCPU - clientCores
	server = rangeList(0, serverCores-1)
	client = rangeList(serverCores, numCPU-1)
	return server, client, nil
}

// DefaultClientCores picks how many cores to hand the load generator for a
// machine of numCPU cores. It gives the client half the box, with a floor of two
// and a ceiling that always leaves the server at least one core.
//
// Half, not a quarter: aki-bench's load generator is a Go client that does its
// own RESP encode, decode, and histogram work per reply, so it is far heavier
// per operation than redis-benchmark's C threads. A quarter of the box starves
// it, and a starved client cannot drive the server to saturation, which caps all
// three targets at the client's ceiling and collapses the measured ratio toward
// 1.0. On a quiet 6-core box a 3-core client lets aki saturate near 0.9M ops/s
// while a 2-core client strangles the same run below 0.65M, dragging a real >2x
// down to 1.3x. So the honest single-box default is a balanced split; the truly
// clean fix is still a separate client box through connect mode.
func DefaultClientCores(numCPU int) int {
	return min(max(numCPU/2, 2), numCPU-1)
}

// rangeList renders an inclusive core range as a taskset -c list. A single core
// renders as just its number rather than "n-n".
func rangeList(lo, hi int) string {
	if lo >= hi {
		return strconv.Itoa(lo)
	}
	return strconv.Itoa(lo) + "-" + strconv.Itoa(hi)
}

// Count returns how many cores a taskset -c list names. It accepts the comma and
// dash syntax taskset uses, for example "0-3,6" counts as five. It is used to
// size the client's GOMAXPROCS to the cores it was actually pinned to.
func Count(list string) (int, error) {
	if strings.TrimSpace(list) == "" {
		return 0, fmt.Errorf("cpuset: empty list")
	}
	total := 0
	for part := range strings.SplitSeq(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return 0, fmt.Errorf("cpuset: bad list %q: %w", list, err)
		}
		if !found {
			total++
			continue
		}
		b, err := strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return 0, fmt.Errorf("cpuset: bad list %q: %w", list, err)
		}
		if b < a {
			return 0, fmt.Errorf("cpuset: descending range %q", part)
		}
		total += b - a + 1
	}
	if total == 0 {
		return 0, fmt.Errorf("cpuset: list %q names no cores", list)
	}
	return total, nil
}
