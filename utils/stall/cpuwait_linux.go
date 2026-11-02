//go:build linux

package stall

import (
	"bytes"
	"encoding/binary"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
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
	reads uint64                // reads so far
	held  map[string]uint64     // each thread held off at the last read: the read that first found it held, this time
}

// threadStat is a thread's account: its CPU time, run delay and times run
// (schedstat), whether it is runnable, and the CPU it is on (stat).
type threadStat struct {
	exec, delay, runs int64
	runnable          bool
	cpu               string
}

// longest is the most any one thread was held off a CPU since the last call,
// and whether a thread is held off now (runnable, and not moved since the
// last call: no CPU time, no wait ended, not run): for how many calls in a
// row, this one included, the one held off longest has been, 0 if none is.
// 0 and 0 where the threads cannot be read (no /proc, a kernel without
// CONFIG_SCHED_INFO).
func (w *threadWaits) longest() (time.Duration, uint64) {
	return w.longestIn(taskDir, procStat)
}

// longestIn is longest, the threads read from their directories in tasksDir
// and the CPUs' steal from statFile (the tests' are not the kernel's).
func (w *threadWaits) longestIn(tasksDir, statFile string) (time.Duration, uint64) {
	tasks, err := os.ReadDir(tasksDir)
	if err != nil {
		return 0, 0
	}
	seen := make(map[string]threadStat, len(tasks))
	kept := false
	for _, task := range tasks {
		if st, ok := readThread(tasksDir, task.Name()); ok {
			seen[task.Name()] = st
			kept = kept || st.runs > 0
		} // else the thread has exited
	}
	if !kept {
		// No thread has run, the one reading included: the kernel keeps
		// no account and shows each as "0 0 0" (before 5.14, built without
		// schedstats and booted with nodelayacct). No thread would ever
		// move, so every runnable one would be held off at every read,
		// and a verdict waiting for it would never come. Nothing is read.
		seen = nil
	}
	var steal map[string]int64
	if b, err := os.ReadFile(statFile); err == nil {
		steal = parseSteal(b)
	}
	return w.account(seen, steal)
}

// account is longest's sum over the threads seen now and each CPU's steal,
// against the last read; it keeps them for the next.
func (w *threadWaits) account(seen map[string]threadStat, steal map[string]int64) (most time.Duration, heldFor uint64) {
	w.reads++
	held := make(map[string]uint64)
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
		if known && !moved && st.runnable {
			from, ok := w.held[tid]
			if !ok {
				from = w.reads
			}
			held[tid] = from
			heldFor = max(heldFor, w.reads-from+1)
		}
	}
	w.last, w.steal, w.held = seen, steal, held
	return most, heldFor
}

// sleepFor sleeps the calling thread in the kernel for d, and says whether
// stop closed first. The thread is woken where its sleep ends, so on a host
// that holds the process off its wait for a CPU from then on is its own run
// delay (threadRunDelay). It looks at stop only after a sleep: whoever closes
// stop wakes w after it, so the sleep takes that wake before the sampling
// ends, and the next sampling's first sleep does not end at once.
func sleepFor(d time.Duration, stop <-chan struct{}, w *waker) (stopped bool) {
	end := time.Now().Add(d)
	for {
		// Cut short by a signal, or by a wake left from an earlier
		// release, it sleeps what is left.
		w.sleep(time.Until(end))
		select {
		case <-stop:
			return true
		default:
		}
		if time.Until(end) <= 0 {
			return false
		}
	}
}

// waker ends the sampler's sleep (sleepFor) at the release of its last user,
// so the release returns once the sampling thread runs, not at its next
// look: an eventfd the release writes to and the sleep polls along with its
// timer. One per sampler, made at its first acquire and never closed, so no
// sleep ever polls a number the process has since given another file. fd < 0:
// the kernel made none, and the sleep wakes a Tick at a time to look.
type waker struct{ fd int }

// newWaker makes a sampler's waker.
func newWaker() *waker {
	fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return &waker{fd: -1}
	}
	return &waker{fd: fd}
}

// wake ends the sleep in progress, or the next one. Without an eventfd it
// writes to none (EBADF).
func (w *waker) wake() {
	var one [8]byte
	binary.NativeEndian.PutUint64(one[:], 1)
	_, _ = unix.Write(w.fd, one[:])
}

// sleep sleeps the calling thread in the kernel for d at most (none if d is
// not positive: the kernel refuses a negative time), and ends at a wake,
// which it takes. Without an eventfd it sleeps a Tick at most.
func (w *waker) sleep(d time.Duration) {
	if w.fd < 0 {
		ts := unix.NsecToTimespec(int64(min(d, Tick)))
		_ = unix.Nanosleep(&ts, nil)
		return
	}
	ts := unix.NsecToTimespec(int64(d))
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	if n, _ := unix.Ppoll(fds, &ts, nil); n > 0 {
		var b [8]byte
		_, _ = unix.Read(w.fd, b[:])
	}
}

// threadRunDelay is the calling thread's run delay so far (schedstat); false
// if the kernel keeps none.
func threadRunDelay() (time.Duration, bool) {
	b, err := os.ReadFile("/proc/thread-self/schedstat")
	if err != nil {
		return 0, false
	}
	_, delay, _, ok := parseSchedstat(b)
	return time.Duration(delay), ok
}

// readThread reads the account of thread tid, in tasksDir; false if it has
// exited or the kernel keeps none.
func readThread(tasksDir, tid string) (threadStat, bool) {
	var st threadStat
	b, err := os.ReadFile(tasksDir + "/" + tid + "/schedstat")
	if err != nil {
		return st, false
	}
	var ok bool
	if st.exec, st.delay, st.runs, ok = parseSchedstat(b); !ok {
		return st, false
	}
	if b, err = os.ReadFile(tasksDir + "/" + tid + "/stat"); err != nil {
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
