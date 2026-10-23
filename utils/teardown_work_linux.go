//go:build linux

package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// platformProgressSeen: the kernel's account of a thread shows its progress.
const platformProgressSeen = true

// platformWorkProbe watches a stage of teardown work through the thread it
// runs on: the calling goroutine is locked to its thread until the stage ends
// (threadProbe.release), so the thread the kernel accounts for is the one
// doing the work -- the one in the removal's system call, or waiting on the
// CA tool it started.
func platformWorkProbe() workProbe {
	runtime.LockOSThread()
	tid := unix.Gettid()
	p := &threadProbe{tid: tid, dir: "/proc/self/task/" + strconv.Itoa(tid)}
	// The processes the thread had already started, for other goroutines
	// that ran on it before, are not the stage's: their running is no
	// progress of its. Each is known by its pid and start time, so a pid
	// reused meanwhile is the stage's. Known only where the kernel lists a
	// thread's own children; otherwise every child of the agent counts
	// (threadChildren).
	if !childrenFiles() {
		return p // nothing to snapshot: every child of the agent counts
	}
	if pids, own := threadChildren(p.dir); own {
		for _, pid := range pids {
			if start, ok := processStart(pid); ok {
				if p.before == nil {
					p.before = map[int]uint64{}
				}
				p.before[pid] = start
			}
		}
	}
	return p
}

// threadProbe is a stage of teardown work's thread, and what it has seen of
// it.
type threadProbe struct {
	tid    int
	dir    string         // the thread's /proc/self/task/<tid>
	before map[int]uint64 // the thread's children when the stage began: pid -> start time
	last   schedMark
	seen   bool
}

// schedMark is the kernel's account of a set of tasks: the work's thread, and
// every thread of every process it runs (and theirs). Any change in it is
// progress: a task scheduled, a process started or reaped.
type schedMark struct {
	onCPU, waited, slices uint64 // schedstat: ns run, ns waited to run, times run
	tasks                 int
	runnable              bool // a task is running, or waiting to run, now
}

func (p *threadProbe) release() {
	// Only from the goroutine that locked it (BeginTeardownWork): another
	// goroutine's unlock would let go of a thread that one locked itself.
	if unix.Gettid() == p.tid {
		runtime.UnlockOSThread()
	}
}

func (p *threadProbe) moved() bool {
	var m schedMark
	if !markThread(&m, p.dir) {
		return true // cannot tell: not taken for no progress
	}
	seen := map[int]bool{}
	children, _ := threadChildren(p.dir)
	for _, pid := range children {
		if start, ok := p.before[pid]; ok {
			if now, ok := processStart(pid); !ok || now == start {
				continue // the thread's from before the stage, or gone
			}
		}
		markProcess(&m, pid, seen)
	}
	runnable := m.runnable
	m.runnable = false // compared by what the kernel has counted alone
	moved := !p.seen || runnable || m != p.last
	p.last, p.seen = m, true
	return moved
}

// ProcessProgress returns a function that reports whether process pid, or a
// process it runs (and theirs), has made progress since the function was last
// called, by the rule a stage of teardown work is judged by: one of their
// tasks scheduled, a process of them started or reaped, or one running or
// waiting to run now. The first call, and one that cannot tell, report true.
// It is how a process that keeps others waiting while a tool it started runs
// (the trust-store lock's holder) tells them whether the tool is getting on
// or wedged.
func ProcessProgress(pid int) (moved func() bool) {
	var last schedMark
	seen := false
	return func() bool {
		var m schedMark
		markProcess(&m, pid, map[int]bool{})
		if m.tasks == 0 && !seen {
			return true // cannot tell
		}
		runnable := m.runnable
		m.runnable = false // compared by what the kernel has counted alone
		moved := !seen || runnable || m != last
		last, seen = m, true
		return moved
	}
}

// describe reads how the stage's thread stands: its state (stat), and the
// kernel function it sleeps in (wchan), which the kernel gives as "0" for a
// thread that runs, and to a reader it does not let see kernel addresses.
func (p *threadProbe) describe() (state, wait string) {
	if b, err := os.ReadFile(p.dir + "/stat"); err == nil {
		if st, _, _, ok := parseTaskStat(b); ok {
			state = string(st)
		}
	}
	if b, err := os.ReadFile(p.dir + "/wchan"); err == nil {
		if w := strings.TrimSpace(string(b)); w != "" && w != "0" {
			wait = w
		}
	}
	return state, wait
}

// markThread adds the task at dir (a /proc/<pid>/task/<tid> directory) to m,
// and reports whether it could be read.
func markThread(m *schedMark, dir string) bool {
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return false
	}
	state, utime, stime, ok := parseTaskStat(stat)
	if !ok {
		return false
	}
	m.tasks++
	if state == 'R' {
		m.runnable = true
	}
	if onCPU, waited, slices, ok := readSchedstat(dir + "/schedstat"); ok {
		m.onCPU += onCPU
		m.waited += waited
		m.slices += slices
	} else {
		// A kernel that keeps no schedstat: CPU time in ticks, and the
		// context switches the status file counts.
		m.onCPU += utime + stime
		m.slices += contextSwitches(dir + "/status")
	}
	return true
}

// markProcess adds every thread of process pid to m, and then, once each,
// the processes it started and theirs. A process that exits meanwhile is
// simply not counted.
func markProcess(m *schedMark, pid int, seen map[int]bool) {
	if seen[pid] {
		return
	}
	seen[pid] = true
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, t := range tasks {
		markThread(m, dir+"/"+t.Name())
	}
	for _, child := range processChildren(pid, dir, tasks) {
		markProcess(m, child, seen)
	}
}

// childrenFiles says whether this kernel lists a thread's own children
// (/proc/<pid>/task/<tid>/children, CONFIG_PROC_CHILDREN): known once, so a
// thread that exits between a listing and a read is not taken for a kernel
// without them.
var childrenFiles = sync.OnceValue(func() bool {
	_, err := os.Stat("/proc/thread-self/children")
	return err == nil
})

// threadChildren returns the processes the thread at dir started and has not
// yet reaped, from its children file (own: true); on a kernel without one,
// every process whose parent is the thread's process (own: false) -- more
// than the thread's own, which can only make a stage seen as progressing that
// is not, never the reverse.
func threadChildren(dir string) (pids []int, own bool) {
	if childrenFiles() {
		b, err := os.ReadFile(dir + "/children")
		if err != nil {
			return nil, true
		}
		return parsePids(b), true
	}
	parent, err := processOf(dir)
	if err != nil {
		return nil, false
	}
	return childrenByParent(parent), false
}

// processChildren returns the processes process pid started and has not yet
// reaped: the union of its threads' children files (tasks, read from dir), or
// on a kernel without them, every process whose parent is pid.
func processChildren(pid int, dir string, tasks []os.DirEntry) []int {
	if !childrenFiles() {
		return childrenByParent(pid)
	}
	var pids []int
	for _, t := range tasks {
		b, err := os.ReadFile(dir + "/" + t.Name() + "/children")
		if err != nil {
			continue // the thread exited meanwhile
		}
		pids = append(pids, parsePids(b)...)
	}
	return pids
}

// childrenByParent returns every process whose parent is parent, by each
// process's stat file.
func childrenByParent(parent int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if ppid, ok := parseParent(b); ok && ppid == parent {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processStart returns process pid's start time (stat field 22, in clock
// ticks since boot): with its pid, what tells one process from a later one
// given the same pid.
func processStart(pid int) (uint64, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, false
	}
	f := bytes.Fields(b[i+1:])
	// starttime is field 22 of the file; the fields here begin at 3.
	if len(f) < 20 {
		return 0, false
	}
	return parseUint(f[19]), true
}

func parsePids(b []byte) []int {
	var pids []int
	for _, f := range bytes.Fields(b) {
		if pid, err := strconv.Atoi(string(f)); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processOf returns the id of the process the task at dir belongs to:
// /proc/self/task/<tid> is this process's, /proc/<pid>/task/<tid> pid's.
func processOf(dir string) (int, error) {
	proc := filepath.Dir(filepath.Dir(dir))
	if filepath.Base(proc) == "self" {
		return os.Getpid(), nil
	}
	return strconv.Atoi(filepath.Base(proc))
}

// readSchedstat reads a task's schedstat file: ns on a CPU, ns waited to run,
// times run. A kernel that keeps no such account either has no file or
// writes zeros in it (a kernel booted without delay accounting): both are no
// account, and ok is false.
func readSchedstat(path string) (onCPU, waited, slices uint64, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, false
	}
	return parseSchedstat(b)
}

// parseSchedstat parses a task's schedstat file (readSchedstat): zeros, or a
// file too short, are no account, and ok is false.
func parseSchedstat(b []byte) (onCPU, waited, slices uint64, ok bool) {
	f := bytes.Fields(b)
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	onCPU, waited, slices = parseUint(f[0]), parseUint(f[1]), parseUint(f[2])
	return onCPU, waited, slices, onCPU != 0 || waited != 0 || slices != 0
}

// parseTaskStat reads a task's state and its user and system CPU time (in
// ticks) from its stat file. The command name, in parentheses, may itself
// hold spaces and parentheses: the fields are counted from the last ')'.
func parseTaskStat(b []byte) (state byte, utime, stime uint64, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, 0, false
	}
	f := bytes.Fields(b[i+1:])
	// state is field 3 of the file, utime 14, stime 15.
	if len(f) < 13 || len(f[0]) == 0 {
		return 0, 0, 0, false
	}
	return f[0][0], parseUint(f[11]), parseUint(f[12]), true
}

// parseParent reads a process's parent id from its stat file.
func parseParent(b []byte) (int, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, false
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(string(f[1]))
	return ppid, err == nil
}

// contextSwitches sums the voluntary and involuntary context switches a
// task's status file counts.
func contextSwitches(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var n uint64
	for _, line := range bytes.Split(b, []byte("\n")) {
		k, v, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue
		}
		switch string(k) {
		case "voluntary_ctxt_switches", "nonvoluntary_ctxt_switches":
			n += parseUint(bytes.TrimSpace(v))
		}
	}
	return n
}

func parseUint(b []byte) uint64 {
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}
