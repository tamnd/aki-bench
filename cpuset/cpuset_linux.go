//go:build linux

package cpuset

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// pinnedEnv guards against an infinite re-exec loop. The first pass sets it
// before exec'ing under taskset; the second pass sees it and skips re-exec.
const pinnedEnv = "AKIBENCH_CPU_PINNED"

// Available reports whether CPU partitioning can be applied here: this is Linux
// and the taskset tool is on PATH.
func Available() bool {
	_, err := exec.LookPath("taskset")
	return err == nil
}

// PinSelf confines the aki-bench process to the cores named by list and returns
// whether it re-exec'd to do so. On the first call it replaces the process image
// with "taskset -c <list> <self> <same args>", so every OS thread the Go runtime
// later spawns inherits the affinity mask. That call does not return on success.
// On the second pass, recognized by the guard environment variable, it sets
// GOMAXPROCS to the number of pinned cores so the scheduler does not oversubscribe
// the partition, and returns false.
//
// A re-exec failure is returned to the caller, which can carry on unpinned rather
// than abort the run.
func PinSelf(list string) (reexeced bool, err error) {
	if os.Getenv(pinnedEnv) != "" {
		if n, cerr := Count(list); cerr == nil && n > 0 {
			runtime.GOMAXPROCS(n)
		}
		return false, nil
	}
	taskset, err := exec.LookPath("taskset")
	if err != nil {
		return false, err
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	argv := append([]string{"taskset", "-c", list, self}, os.Args[1:]...)
	env := append(os.Environ(), pinnedEnv+"=1")
	// Exec replaces this image; on success it never returns.
	if err := syscall.Exec(taskset, argv, env); err != nil {
		return false, err
	}
	return true, nil
}

// ServerWrap rewrites a launch command so the server runs pinned to the cores
// named by list. It returns the taskset binary and the original command folded
// into its arguments. An empty list leaves the command untouched.
func ServerWrap(list, bin string, args []string) (string, []string) {
	if list == "" {
		return bin, args
	}
	if _, err := exec.LookPath("taskset"); err != nil {
		return bin, args
	}
	wrapped := append([]string{"-c", list, bin}, args...)
	return "taskset", wrapped
}
