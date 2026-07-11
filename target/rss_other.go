//go:build !linux

package target

// rssBytes has no portable source off Linux; the report leaves the RSS column
// empty rather than deriving a number from a platform-specific approximation.
// The gate box is Linux (WSL2), so the column is populated where it gates.
func rssBytes(pid int) int64 { return 0 }

// hwmBytes likewise has no portable off-Linux source for the peak resident set,
// so the peak column stays empty everywhere but the gate box.
func hwmBytes(pid int) int64 { return 0 }
