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
	return statusField(pid, "VmRSS:")
}

// hwmBytes reads a process's peak resident set size from /proc/<pid>/status
// VmHWM (high-water mark). Peak matters as much as steady state for the LTM
// pitch: aki exists to serve more data with less memory, and a peak above the
// rival's during load defeats that even when the settled RSS is lower. Returns
// 0 when it cannot be read.
func hwmBytes(pid int) int64 {
	return statusField(pid, "VmHWM:")
}

// statusField reads one "Name: N kB" line out of /proc/<pid>/status and returns
// N in bytes, or 0 when the process or the field is unreadable. VmRSS and VmHWM
// share this parse, since /proc reports them in the same kB form.
func statusField(pid int, prefix string) int64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		v, found := bytes.CutPrefix(line, []byte(prefix))
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
