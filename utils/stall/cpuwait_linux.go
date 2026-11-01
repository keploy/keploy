//go:build linux

package stall

import (
	"bytes"
	"os"
	"strconv"
	"time"
)

// taskDir holds a directory per thread of the process.
const taskDir = "/proc/self/task"

// threadWaits reads how long the process's threads waited for a CPU, from
// the kernel's run delay of each (/proc/self/task/TID/schedstat: time spent
// runnable on a run queue, or in a throttled cgroup, not running). A thread's
// wait is added to it as the thread gets the CPU, so a read cannot see the
// one in progress. Not safe for concurrent use: the sampler reads it under
// its lock.
type threadWaits struct {
	last map[string]time.Duration // each thread's run delay at the last read
}

// longest is the most any one thread waited for a CPU since the last call;
// 0 where the run delays cannot be read (no /proc, a kernel without
// CONFIG_SCHED_INFO).
func (w *threadWaits) longest() time.Duration {
	tasks, err := os.ReadDir(taskDir)
	if err != nil {
		return 0
	}
	seen := make(map[string]time.Duration, len(tasks))
	var most time.Duration
	for _, task := range tasks {
		b, err := os.ReadFile(taskDir + "/" + task.Name() + "/schedstat")
		if err != nil {
			continue // the thread has exited
		}
		d, ok := runDelay(b)
		if !ok {
			continue
		}
		seen[task.Name()] = d
		most = max(most, d-w.last[task.Name()])
	}
	w.last = seen
	return most
}

// runDelay is the second of schedstat's three fields: time on a CPU, run
// delay, times run; all counters.
func runDelay(schedstat []byte) (time.Duration, bool) {
	f := bytes.Fields(schedstat)
	if len(f) < 2 {
		return 0, false
	}
	ns, err := strconv.ParseInt(string(f[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(ns), true
}
