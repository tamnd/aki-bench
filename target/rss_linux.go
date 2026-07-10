//go:build linux

package target

import (
	"bytes"
	"os"
	"strconv"
)

// rssBytes reads a process's resident set size from /proc/<pid>/status VmRSS.
// It returns 0 when the process or the field cannot be read; the caller treats
// 0 as "not measured", never as an actual footprint.
func rssBytes(pid int) int64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		v, found := bytes.CutPrefix(line, []byte("VmRSS:"))
		if !found {
			continue
		}
		v = bytes.TrimSuffix(bytes.TrimSpace(v), []byte(" kB"))
		n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
		if err != nil {
			return 0
		}
		return n * 1024
	}
	return 0
}
