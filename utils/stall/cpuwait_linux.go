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

// procStat is the kernel's per-CPU times, steal among them.
const procStat = "/proc/stat"

// userHZ is the unit of /proc/stat's times: USER_HZ, 100 on every Linux.
const userHZ = 100

// threadWaits reads how long the process's threads were held off a CPU: the
// kernel's run delay of each (/proc/self/task/TID/schedstat: time spent
// runnable on a run queue, or in a throttled cgroup, not running), and the
// time the hypervisor took the virtual CPU the thread runs on (steal, in
// /proc/stat): a thread running on a virtual CPU the host has not scheduled
// does not run, and neither its CPU time nor its run delay grows. The kernel
// adds a wait to the thread's run delay as the thread gets the CPU, and the
// steal to the CPU's at its first tick back, so a read cannot see either
// while it lasts; what it can see is a runnable thread that has not moved
// since the last read (held). Not safe for concurrent use: the sampler reads
// it under its lock.
type threadWaits struct {
	last  map[string]threadStat // each thread at the last read
	steal map[string]int64      // each CPU's steal at the last read, in USER_HZ ticks
}

// threadStat is a thread's account: its CPU time, run delay and times run
// (schedstat), whether it is runnable, and the CPU it is on (stat).
type threadStat struct {
	exec, delay, runs int64
	runnable          bool
	cpu               string
}

// longest is the most any one thread was held off a CPU since the last call,
// and whether a thread is held off now: runnable, and not moved since the last
// call (no CPU time, no wait ended, not run). 0 and false where the threads
// cannot be read (no /proc, a kernel without CONFIG_SCHED_INFO).
func (w *threadWaits) longest() (time.Duration, bool) {
	tasks, err := os.ReadDir(taskDir)
	if err != nil {
		return 0, false
	}
	seen := make(map[string]threadStat, len(tasks))
	for _, task := range tasks {
		if st, ok := readThread(task.Name()); ok {
			seen[task.Name()] = st
		} // else the thread has exited
	}
	var steal map[string]int64
	if b, err := os.ReadFile(procStat); err == nil {
		steal = parseSteal(b)
	}
	return w.account(seen, steal)
}

// account is longest's sum over the threads seen now and each CPU's steal,
// against the last read; it keeps them for the next.
func (w *threadWaits) account(seen map[string]threadStat, steal map[string]int64) (most time.Duration, held bool) {
	for tid, st := range seen {
		// A thread the last read did not see started since: all of its
		// account is since then, and none of it is a wait in progress.
		was, known := w.last[tid]
		moved := !known || st.exec != was.exec || st.delay != was.delay || st.runs != was.runs
		wait := time.Duration(st.delay - was.delay)
		if moved || st.runnable {
			// It ran or waited to since the last read: the steal of the
			// CPU it is on held it off too. A thread that slept through
			// it was not.
			if s, ok := steal[st.cpu]; ok {
				wait += time.Duration(max(s-w.steal[st.cpu], 0)) * (time.Second / userHZ)
			}
		}
		most = max(most, wait)
		held = held || (known && !moved && st.runnable)
	}
	w.last, w.steal = seen, steal
	return most, held
}

// readThread reads a thread's account; false if it has exited or the kernel
// keeps none.
func readThread(tid string) (threadStat, bool) {
	var st threadStat
	b, err := os.ReadFile(taskDir + "/" + tid + "/schedstat")
	if err != nil {
		return st, false
	}
	var ok bool
	if st.exec, st.delay, st.runs, ok = parseSchedstat(b); !ok {
		return st, false
	}
	if b, err = os.ReadFile(taskDir + "/" + tid + "/stat"); err != nil {
		return st, false
	}
	st.runnable, st.cpu, ok = parseStat(b)
	return st, ok
}

// parseSchedstat reads schedstat's three fields: time on a CPU, run delay,
// times run; all counters.
func parseSchedstat(b []byte) (exec, delay, runs int64, ok bool) {
	f := bytes.Fields(b)
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	var ok1, ok2, ok3 bool
	exec, ok1 = parseInt(f[0])
	delay, ok2 = parseInt(f[1])
	runs, ok3 = parseInt(f[2])
	return exec, delay, runs, ok1 && ok2 && ok3
}

// parseStat reads a thread's stat, "TID (comm) STATE ...": whether it is
// runnable (R), and the CPU it last ran on (the 39th field), by its
// /proc/stat name ("cpu3"). comm may hold spaces and parentheses, so the
// fields count from its last ')'.
func parseStat(b []byte) (runnable bool, cpu string, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return false, "", false
	}
	g := bytes.Fields(b[i+1:])
	if len(g) < 37 {
		return false, "", false
	}
	if _, ok := parseInt(g[36]); !ok {
		return false, "", false
	}
	return string(g[0]) == "R", "cpu" + string(g[36]), true
}

// parseSteal reads each CPU's steal so far from /proc/stat, by its name
// ("cpu3"), in USER_HZ ticks.
func parseSteal(b []byte) map[string]int64 {
	steal := make(map[string]int64)
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		// cpuN user nice system idle iowait irq softirq steal ...
		f := bytes.Fields(line)
		if len(f) < 9 || !bytes.HasPrefix(f[0], []byte("cpu")) || len(f[0]) == 3 {
			continue
		}
		if s, ok := parseInt(f[8]); ok {
			steal[string(f[0])] = s
		}
	}
	return steal
}

func parseInt(b []byte) (int64, bool) {
	n, err := strconv.ParseInt(string(b), 10, 64)
	return n, err == nil
}
