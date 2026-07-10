//go:build !linux

package target

// rssBytes has no portable source off Linux; the report leaves the RSS column
// empty rather than deriving a number from a platform-specific approximation.
// The gate box is Linux (WSL2), so the column is populated where it gates.
func rssBytes(pid int) int64 { return 0 }
