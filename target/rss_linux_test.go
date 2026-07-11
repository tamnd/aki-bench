//go:build linux

package target

import (
	"os"
	"testing"
)

// TestStatusFieldReadsSelf validates the /proc/<pid>/status parser against this
// test process, which certainly has a non-zero resident set and a peak at least
// as large. It runs only on Linux, the platform of the gate box, so the VmHWM
// column the LTM protocol relies on is exercised where it is actually read.
func TestStatusFieldReadsSelf(t *testing.T) {
	pid := os.Getpid()

	rss := rssBytes(pid)
	if rss <= 0 {
		t.Fatalf("VmRSS for self = %d, want > 0", rss)
	}
	hwm := hwmBytes(pid)
	if hwm <= 0 {
		t.Fatalf("VmHWM for self = %d, want > 0", hwm)
	}
	// The peak can never sit below the current resident set.
	if hwm < rss {
		t.Fatalf("VmHWM %d < VmRSS %d, impossible", hwm, rss)
	}
}

func TestStatusFieldUnknownPid(t *testing.T) {
	// A pid that does not exist yields 0, never a bogus footprint. -1 is never a
	// live pid, so /proc has no directory for it.
	if got := statusField(-1, "VmRSS:"); got != 0 {
		t.Fatalf("statusField(-1) = %d, want 0", got)
	}
	if got := statusField(os.Getpid(), "NoSuchField:"); got != 0 {
		t.Fatalf("statusField(missing field) = %d, want 0", got)
	}
}
