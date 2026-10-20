//go:build linux

// Package starvetest holds processes off the CPU in a test, as a loaded host
// does, or a cgroup whose CPU quota their siblings spent: they are runnable,
// and wait for a CPU they do not get. No privilege is needed.
//
// Each thread of a held process (and so each thread it starts, which inherits
// its scheduling) is moved to one CPU, at the idle policy (SCHED_IDLE, which
// an unprivileged process may take for its own), and hogs that always run are
// pinned to that CPU at the normal policy. A task at the idle policy is
// picked only once the normal tasks of its CPU have run on for long (its
// weight is 3 to their 1024 each), and the more hogs there, the longer: a
// held process woken by a signal acted on it within 0.4 to 2 s behind three
// hogs on an idle host (kernel 7.1), and within 2 to 5 s behind eight on a
// host at load 40 (kernel 7.2); not within 10 s behind 32 there.
//
// The processes a test so holds, and judges on their way out, write the
// files they mark it with in MemDir: on storage, a loaded host holds them
// in the kernel too, and a test of the CPU is then one of the disk.
package starvetest

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"

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
	stop := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, h := range running {
			_ = h.Process.Kill()
			_ = h.Wait()
		}
		running = nil
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
		holdProcess(t, pid, &one)
	}
	var once sync.Once
	return func() { once.Do(stop) }
}

// MemDir is a directory on a memory filesystem (tmpfs: /dev/shm, or the
// temp directory where it is one), removed at the test's end, for the files
// processes write on a way out that a test's stop judges. A file written on
// storage waits on it (the directory locked, its blocks read: an
// uninterruptible sleep, D), and keploy's stop counts that time toward a stall
// like any the process could run (utils.ExitClock): on a host whose storage is
// loaded, a process was then killed for want of progress within a test's
// short bound, whatever the stop did with the time it could not run. Where no
// memory filesystem can be had it is t.TempDir(), and says so.
func MemDir(t *testing.T) string {
	t.Helper()
	for _, root := range []string{"/dev/shm", os.TempDir()} {
		var fs unix.Statfs_t
		if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.TMPFS_MAGIC {
			continue
		}
		dir, err := os.MkdirTemp(root, "keploy-test-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	t.Log("no memory filesystem to write the test's files on: its processes' way out may wait on storage")
	return t.TempDir()
}

// holdProcess moves every thread of pid to cpus at the idle policy, again
// until a pass finds no thread it had not moved: a thread started meanwhile
// by one not yet moved would run on as it was.
func holdProcess(t *testing.T, pid int, cpus *unix.CPUSet) {
	t.Helper()
	held := map[int]bool{}
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
			return
		}
	}
}
