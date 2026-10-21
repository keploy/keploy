//go:build linux

// Package starvetest holds processes off the CPU in a test, as a loaded host
// does, or a cgroup whose CPU quota their siblings spent: they are runnable,
// and wait for a CPU they do not get. No privilege is needed.
//
// Each thread of a held process (and so each thread it starts, which inherits
// its scheduling) is moved to one CPU, at the idle policy (SCHED_IDLE, which
// an unprivileged process may take for its own), and hogs that always run are
// pinned to that CPU at the normal policy. A task at the idle policy is
// picked rarely there (its weight is 3 to their 1024 each), but it is picked:
// a hold slows a held process to a trickle, it does not freeze it. How fast
// the trickle runs is the scheduler's, not the test's: none in 5 s behind 32
// hogs on an idle kernel-7.1 host, but in a cgroup with a CPU quota (as CI
// jobs and the test boxes' slots run) the held thread is picked some ten
// times a second, a few microseconds each (7.1); on kernel 6.12 in a 4-CPU
// quota at load, a process held behind 32 hogs and then killed got out within
// 0.65 to 2.7 s in each of 100 tries, one with 32 threads within 1.1 to 2.1 s.
//
// So a held process whose way out a test judges must not be able to finish
// it on the turns a hold still gives it. Where it is the test's own, it
// spins on its way out until the test releases it (Spin, or a shell's
// `while [ ! -e released ]; do :; done`): it stays runnable, waiting for a
// CPU, and gets out only once released, however many turns it had; were the
// hold to fail, it would run on making no progress, which the stop judges.
// One that is killed cannot wait: its exit must take more CPU than the turns
// give it over the test's window (HeavyExit: 60000 mappings, 24 ms of CPU on
// that 6.12 host, where one so killed stayed held for 8 s in each of 15
// tries). A test shows the hold held by what the held processes ran
// meanwhile (Ran): far less than the stall bound it judges on.
//
// Released, the hogs end and the held threads may run on every CPU they
// could before, at the policy they had where the kernel lets them have it
// back: an unprivileged thread cannot leave the idle policy (the kernel asks
// CAP_SYS_NICE of one, or an RLIMIT_NICE that allows its nice, which the
// default of 0 does not), so there they keep it, and on a busy host get out
// only as fast as the CPU others leave them.
//
// The processes a test so holds, and judges on their way out, write the
// files they mark it with in MemDir, and run from programs there (ExecDir):
// on storage, a loaded host holds them in the kernel too, and a test of the
// CPU is then one of the disk.
package starvetest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// hogs is how many hogs hold the CPU (see above). They take that one CPU
// whatever their number.
const hogs = 32

// Hold holds processes pids off the CPU until release (idempotent; also run
// at the test's cleanup), after which they run again. It skips the test where
// the scheduler will not hold them (no CPU to pin to, or the calls refused,
// as a sandbox may).
func Hold(t *testing.T, pids ...int) (release func()) {
	t.Helper()
	if _, ok := Ran(os.Getpid()); !ok {
		t.Skip("this kernel keeps no account of a thread's time on a CPU (schedstat): a stop cannot tell a held process from a stalled one")
	}
	var mine unix.CPUSet
	if err := unix.SchedGetaffinity(0, &mine); err != nil {
		t.Skipf("cannot read this process's CPUs: %v", err)
	}
	cpu := -1
	for i := 0; i < len(mine)*64; i++ {
		if mine.IsSet(i) {
			cpu = i
		}
	}
	if cpu < 0 {
		t.Skip("this process may run on no CPU")
	}
	var one unix.CPUSet
	one.Set(cpu)

	var mu sync.Mutex
	var running []*exec.Cmd
	var held []*heldProcess
	stop := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, h := range running {
			_ = h.Process.Kill()
			_ = h.Wait()
		}
		running = nil
		for _, p := range held {
			p.let()
		}
		held = nil
	}
	t.Cleanup(stop)
	for range hogs {
		h := exec.Command("sh", "-c", "while :; do :; done")
		if err := h.Start(); err != nil {
			t.Fatalf("start a hog: %v", err)
		}
		mu.Lock()
		running = append(running, h)
		mu.Unlock()
		if err := unix.SchedSetaffinity(h.Process.Pid, &one); err != nil {
			t.Skipf("cannot pin a hog to CPU %d: %v", cpu, err)
		}
	}
	for _, pid := range pids {
		p := holdProcess(t, pid, &one)
		mu.Lock()
		held = append(held, p)
		mu.Unlock()
	}
	var once sync.Once
	return func() { once.Do(stop) }
}

// heldProcess is a process Hold holds: by thread, how it ran before (its
// CPUs and scheduling), and how its first thread moved did, for one started
// since.
type heldProcess struct {
	pid    int
	before map[int]threadSched
	first  threadSched
}

// threadSched is how a thread was scheduled: the CPUs it could run on, and
// its policy and nice (nil where they could not be read).
type threadSched struct {
	cpus unix.CPUSet
	attr *unix.SchedAttr
}

// let lets every thread of p run as it did before, a thread started since as
// its process's first did: on its CPUs, and at its policy where the kernel
// lets it have that back (see above). A thread gone meanwhile is let be.
func (p *heldProcess) let() {
	tasks, err := os.ReadDir("/proc/" + strconv.Itoa(p.pid) + "/task")
	if err != nil {
		return // gone
	}
	for _, task := range tasks {
		tid, err := strconv.Atoi(task.Name())
		if err != nil {
			continue
		}
		was, ok := p.before[tid]
		if !ok {
			was = p.first
		}
		_ = unix.SchedSetaffinity(tid, &was.cpus)
		if was.attr != nil {
			_ = unix.SchedSetAttr(tid, was.attr, 0)
		}
	}
}

// MemDir is a directory on a memory filesystem (tmpfs: /dev/shm, or the
// temp directory where it is one), removed at the test's end, for the files
// processes write on a way out that a test's stop judges. A file written on
// storage waits on it (the directory locked, its blocks read: an
// uninterruptible sleep, D): keploy's stop takes that time for a wait while
// the host's disks serve I/O (utils.ExitClock), but not where the host can
// reach storage its disks do not show (a network filesystem), and a test of
// the CPU is not one of the disk. Where no memory filesystem can be had it is
// t.TempDir(), and says so.
func MemDir(t *testing.T) string {
	t.Helper()
	return memDirFor(t, false, 0)
}

// ExecDir is MemDir for programs the test's processes run from: on a memory
// filesystem that lets them run (not mounted noexec, as /dev/shm often is),
// and with room for size bytes of them; else t.TempDir(), and says so.
func ExecDir(t *testing.T, size int64) string {
	t.Helper()
	return memDirFor(t, true, size)
}

func memDirFor(t *testing.T, exec bool, size int64) string {
	t.Helper()
	for _, root := range []string{"/dev/shm", os.TempDir()} {
		var fs unix.Statfs_t
		if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.TMPFS_MAGIC {
			continue
		}
		if exec && fs.Flags&unix.ST_NOEXEC != 0 {
			continue
		}
		if size > 0 && int64(fs.Bavail)*fs.Bsize < 2*size {
			continue
		}
		dir, err := os.MkdirTemp(root, "keploy-test-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	t.Log("no memory filesystem for the test's files: its processes' way out may wait on storage")
	return t.TempDir()
}

// holdProcess moves every thread of pid to cpus at the idle policy, again
// until a pass finds no thread it had not moved: a thread started meanwhile
// by one not yet moved would run on as it was.
func holdProcess(t *testing.T, pid int, cpus *unix.CPUSet) *heldProcess {
	t.Helper()
	held := map[int]bool{}
	p := &heldProcess{pid: pid, before: map[int]threadSched{}}
	for {
		tasks, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
		if err != nil {
			t.Fatalf("read the threads of process %d: %v", pid, err)
		}
		moved := false
		for _, task := range tasks {
			tid, err := strconv.Atoi(task.Name())
			if err != nil || held[tid] {
				continue
			}
			var was threadSched
			if err := unix.SchedGetaffinity(tid, &was.cpus); err != nil {
				if errors.Is(err, unix.ESRCH) {
					continue // exited meanwhile
				}
				t.Skipf("cannot read the CPUs of thread %d of process %d: %v", tid, pid, err)
			}
			if attr, err := unix.SchedGetAttr(tid, 0); err == nil {
				was.attr = attr
			}
			if len(p.before) == 0 {
				p.first = was
			}
			p.before[tid] = was
			if err := unix.SchedSetaffinity(tid, cpus); err != nil {
				if errors.Is(err, unix.ESRCH) {
					continue // exited meanwhile
				}
				t.Skipf("cannot pin thread %d of process %d: %v", tid, pid, err)
			}
			attr := unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: unix.SCHED_IDLE}
			if err := unix.SchedSetAttr(tid, &attr, 0); err != nil {
				if errors.Is(err, unix.ESRCH) {
					continue
				}
				t.Skipf("cannot set thread %d of process %d to the idle policy: %v", tid, pid, err)
			}
			held[tid], moved = true, true
		}
		if !moved {
			return p
		}
	}
}

// Spin returns once the file at released exists, spinning until then: the
// way out of a held process that is the test's own, which a hold keeps
// waiting for a CPU and which cannot get out before the test releases it,
// whatever turns the hold gives it (see above). The file is on a memory
// filesystem (MemDir): its look waits on no storage.
func Spin(released string) {
	for {
		if _, err := os.Stat(released); err == nil {
			return
		}
	}
}

// Ran is how long the threads of process pid have run on a CPU (the sum of
// their schedstat), and whether the kernel keeps that account: one without
// schedstat shows no file, or zeros. A test reads it of the processes it
// holds before and after the window it judges, to show the hold held them.
func Ran(pid int) (time.Duration, bool) {
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var ran uint64
	for _, task := range tasks {
		b, err := os.ReadFile(dir + "/" + task.Name() + "/schedstat")
		if err != nil {
			continue
		}
		f := bytes.Fields(b)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(string(f[0]), 10, 64); err == nil {
			ran += n
		}
	}
	return time.Duration(ran), ran > 0
}

// heavyExitSink keeps HeavyExit's reads.
var heavyExitSink byte

// HeavyExit gives the calling process an exit that takes the kernel tens of
// milliseconds of CPU: it maps one page of a memory file n times, each its
// own mapping (none can merge with the next: each maps the file from its
// start), and reads each, so its exit tears down n mappings and their page
// table entries, each at a point where it may be preempted. A held process
// killed so must be given that CPU before it is gone, far more than the
// turns a hold gives it in a test's window (see above); one not held is gone
// in that time. n is held to what vm.max_map_count leaves room for.
func HeavyExit(n int) error {
	if limit, err := os.ReadFile("/proc/sys/vm/max_map_count"); err == nil {
		if m, err := strconv.Atoi(string(bytes.TrimSpace(limit))); err == nil {
			n = min(n, m-2000) // the runtime's own mappings, and its growth
		}
	}
	if n <= 0 {
		return fmt.Errorf("vm.max_map_count leaves no room for the mappings")
	}
	page := os.Getpagesize()
	fd, err := unix.MemfdCreate("starvetest-heavy-exit", 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd) // each mapping holds the file
	if err := unix.Ftruncate(fd, int64(page)); err != nil {
		return err
	}
	region, err := unix.Mmap(-1, 0, n*page, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_NORESERVE)
	if err != nil {
		return err
	}
	base := uintptr(unsafe.Pointer(&region[0]))
	for i := range n {
		addr := base + uintptr(i*page)
		got, _, errno := unix.Syscall6(unix.SYS_MMAP, addr, uintptr(page), unix.PROT_READ, unix.MAP_SHARED|unix.MAP_FIXED, uintptr(fd), 0)
		if errno != 0 {
			return fmt.Errorf("map page %d of %d: %w", i, n, errno)
		}
		if got != addr {
			return fmt.Errorf("map page %d of %d: mapped at %#x, not %#x", i, n, got, addr)
		}
		heavyExitSink += region[i*page]
	}
	return nil
}
