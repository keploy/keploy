//go:build linux

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// mainThreadExitsEnv makes this test binary, started with it set to a file's
// path, a process whose main thread exits while another runs on, writing a
// count to that file as it runs: its stat says Z, as ps says Zl, and it runs.
const mainThreadExitsEnv = "KEPLOY_TEST_UTILS_MAIN_THREAD_EXITS"

func init() {
	beat := os.Getenv(mainThreadExitsEnv)
	if beat == "" {
		return
	}
	// init runs on the main thread: the goroutine stays on it.
	runtime.LockOSThread()
	go func() {
		for n := 1; ; n++ {
			_ = os.WriteFile(beat, []byte(strconv.Itoa(n)), 0o644)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for {
		if _, err := os.Stat(beat); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// The main thread alone exits (exit, not exit_group).
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
}

// startMainThreadExited starts this test binary as a process whose main
// thread has exited while another runs on, and returns it once its stat says
// Z and its other thread has written; it is killed, and waited for, when the
// test ends.
func startMainThreadExited(t *testing.T) *exec.Cmd {
	t.Helper()
	beat := filepath.Join(t.TempDir(), "beat")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	// No collection (a stop of the world would wait for ever on the main
	// thread, which holds a P it never gives back), and Ps to spare.
	cmd.Env = append(os.Environ(), mainThreadExitsEnv+"="+beat, "GOGC=off", "GOMAXPROCS=4")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(time.Minute) // turns a hang into a failure
	for {
		st, ok := ReadProcStat(cmd.Process.Pid)
		if b, err := os.ReadFile(beat); err == nil && len(b) > 0 && ok && st.State == 'Z' {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d's main thread did not exit with another running (stat ok %v, state %c)", cmd.Process.Pid, ok, st.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A process whose main thread exited while another runs on says Z in its
// stat line, yet runs: RunningThread finds the thread that runs, and Exited
// says it has not exited. Once every thread of it has exited it has, a
// zombie, and RunningThread finds none.
func TestRunningThreadTellsAProcessWhoseMainThreadExitedFromAZombie(t *testing.T) {
	cmd := startMainThreadExited(t)
	pid := cmd.Process.Pid
	tid, runs, err := RunningThread("/proc", pid)
	if err != nil || !runs || tid == pid || tid == 0 {
		t.Fatalf("RunningThread = %d, %v, %v; want a thread of %d other than its main one, running", tid, runs, err, pid)
	}
	if st, ok := ReadProcStat(pid); !ok || Exited(pid, st) {
		t.Fatalf("Exited(%d) with its other thread running", pid)
	}
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(time.Minute) // turns a hang into a failure
	for {
		_, runs, err := RunningThread("/proc", pid)
		if err == nil && !runs {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still has a thread running after its kill (%v)", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, ok := ReadProcStat(pid); !ok || !Exited(pid, st) {
		t.Fatalf("process %d not Exited once killed (a zombie its Wait has not reaped)", pid)
	}
}

// RunningThread reads each thread's stat for its state and start time: a
// thread being released (its parent and group gone, in any state: Z or X, or
// R or S where it was released between the kernel's reads) is none that
// runs, and one listed whose stat is gone exited meanwhile. A listing in which
// none runs stands only where the process's count of its threads holds no
// thread but those it found exited, still there and the same: else one that
// runs was left out, and the threads are listed again. A listing, a thread's
// stat or the process's, that cannot be read is an error, never a process
// taken for exited.
func TestRunningThreadSaysWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	thread := func(pid, tid int, line string) {
		dir := filepath.Join(root, strconv.Itoa(pid), "task", strconv.Itoa(tid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if line != "" {
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	line := func(tid int, state string) string {
		return fmt.Sprintf("%d (worker) %s 4242 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 2 0 780 0 0\n", tid, state)
	}
	thread(7, 7, line(7, "Z"))
	thread(7, 8, "8 (worker) Z 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n")
	thread(7, 9, "9 (worker) X 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n")
	thread(7, 10, "") // gone before its stat was read
	// The process's count of its threads: its main thread, 8 and 9, none of
	// them released.
	process := func(pid, threads int) {
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "stat"), []byte(fmt.Sprintf("%d (worker) Z 4242 %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 %d 0 780 0 0\n", pid, pid, pid, threads)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	process(7, 3)
	if tid, runs, err := RunningThread(root, 7); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, every thread exited or released", tid, runs, err)
	}
	// Threads released between the kernel's read of their state and of
	// their parent: R and S, their parent and group gone (ReleasedTask).
	thread(7, 13, "13 (worker) R 0 -1 -1 0 -1 4227404 100 0 0 0 0 0 0 0 20 0 1 0 6060367 0 0\n")
	thread(7, 14, "14 (worker) S 0 -1 -1 0 -1 4227404 100 0 0 0 0 0 0 0 20 0 1 0 6060368 0 0\n")
	if tid, runs, err := RunningThread(root, 7); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, threads 13 and 14 released as they were read", tid, runs, err)
	}
	for _, tid := range []string{"13", "14"} {
		if err := os.RemoveAll(filepath.Join(root, "7", "task", tid)); err != nil {
			t.Fatal(err)
		}
	}
	// What a process's threads come to between RunningThread's reads: a
	// stat that is a FIFO whose writer, as RunningThread opens it, moves the
	// threads on (put, take) and only then gives the line.
	put := func(tid int, line string) {
		dir := filepath.Join(root, "7", "task", strconv.Itoa(tid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Error(err)
		}
	}
	take := func(tid int) {
		if err := os.RemoveAll(filepath.Join(root, "7", "task", strconv.Itoa(tid))); err != nil {
			t.Error(err)
		}
	}
	count := func(threads int) string {
		return fmt.Sprintf("7 (worker) Z 4242 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 %d 0 780 0 0\n", threads)
	}
	processStat := filepath.Join(root, "7", "stat")
	leaderStat := filepath.Join(root, "7", "task", "7", "stat")
	zombie8 := "8 (worker) Z 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n"
	// A listing that left out a thread that runs: another was released as
	// it was taken, so the kernel's walk of the threads ended early. The
	// process counts that thread (4: its main thread, 8, 9 and 11), so the
	// threads are listed again, and it is found.
	stop := serveStat(t, processStat, func(int) string {
		put(11, line(11, "S"))
		return count(4)
	})
	if tid, runs, err := RunningThread(root, 7); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, left out of the first listing but counted, sleeping", tid, runs, err)
	}
	stop()
	take(11)
	// The same, where a thread the listing found exited was released before
	// the count was read (3: its main thread, 9 and 11): the count is held
	// against the exited threads still there, not those listed.
	stop = serveStat(t, processStat, func(int) string {
		take(8)
		put(11, line(11, "S"))
		return count(3)
	})
	if tid, runs, err := RunningThread(root, 7); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, left out of the first listing while 8 was released, sleeping", tid, runs, err)
	}
	stop()
	take(11)
	put(8, zombie8)
	// And where the kernel gave 8's tid, after the count was read, to
	// another thread, a zombie by the time it is read again: not the thread
	// the listing found exited (its start time is another), so not one the
	// count held.
	stopLeader := serveStat(t, leaderStat, func(n int) string {
		if n == 2 { // read again, after the count
			put(8, "8 (worker) Z 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060999 0 0\n")
		}
		return line(7, "Z")
	})
	stop = serveStat(t, processStat, func(int) string {
		take(8)
		put(11, line(11, "S"))
		return count(3)
	})
	if tid, runs, err := RunningThread(root, 7); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, its count not made up by another thread given 8's tid", tid, runs, err)
	}
	stop()
	stopLeader()
	take(11)
	put(8, zombie8)
	// A thread released after the count was read (9, as the main thread is
	// read again): the count holds a thread no longer there, so the threads
	// are listed again, and counted again (2: its main thread and 8), and
	// none runs.
	stopLeader = serveStat(t, leaderStat, func(n int) string {
		if n == 2 {
			take(9)
		}
		return line(7, "Z")
	})
	stop = serveStat(t, processStat, func(n int) string {
		if n == 1 {
			return count(3)
		}
		return count(2)
	})
	if tid, runs, err := RunningThread(root, 7); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, 9 released after the count", tid, runs, err)
	}
	stop()
	stopLeader()
	// A count of 0: the stat of a main thread released as it was read (here
	// a thread of it ran an exec and holds its pid now, found at the next
	// listing), never a count every exited thread makes up.
	stop = serveStat(t, processStat, func(n int) string {
		if n == 1 {
			put(11, line(11, "S"))
			return count(0)
		}
		return count(4)
	})
	if tid, runs, err := RunningThread(root, 7); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, a count of 0 taken for none", tid, runs, err)
	}
	stop()
	put(7, line(7, "Z"))
	put(8, zombie8)
	put(9, "9 (worker) X 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n")
	process(7, 4)
	put(11, line(11, "S"))
	if tid, runs, err := RunningThread(root, 7); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, sleeping", tid, runs, err)
	}
	if _, runs, err := RunningThread(root, 99); err != nil || runs {
		t.Fatalf("a process that is gone: runs %v, err %v; want neither", runs, err)
	}
	// A stat that is there and cannot be read, and one with no state.
	thread(12, 12, "")
	if err := os.MkdirAll(filepath.Join(root, "12", "task", "12", "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunningThread(root, 12); err == nil {
		t.Fatal("a thread's stat that could not be read read as no thread running")
	}
	thread(13, 13, "13 (worker")
	if _, _, err := RunningThread(root, 13); err == nil || !strings.Contains(err.Error(), "no state") {
		t.Fatalf("a stat with no state: err %v; want it said", err)
	}
	// A process's count of its threads that cannot be read, or is not in
	// its stat line: an error, never a listing in which none runs taken for
	// every thread there.
	thread(15, 15, line(15, "Z"))
	if err := os.MkdirAll(filepath.Join(root, "15", "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunningThread(root, 15); err == nil || !strings.Contains(err.Error(), "process 15's stat") {
		t.Fatalf("a process stat that could not be read: err %v; want it said", err)
	}
	thread(16, 16, line(16, "Z"))
	if err := os.WriteFile(filepath.Join(root, "16", "stat"), []byte("16 (worker) Z 4242 16 16 0 -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunningThread(root, 16); err == nil || !strings.Contains(err.Error(), "no thread count") {
		t.Fatalf("a process stat with no thread count: err %v; want it said", err)
	}
	// A listing of the threads that cannot be read.
	if err := os.MkdirAll(filepath.Join(root, "14"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "14", "task"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunningThread(root, 14); err == nil {
		t.Fatal("threads that could not be listed read as none running")
	}
}

// fakeThreads is a /proc of process 7 alone, its threads as given (tid to
// state), its stat counting count threads: what the tests of
// runningThreadIn read.
func fakeThreads(t *testing.T, count int, threads map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for tid, state := range threads {
		dir := filepath.Join(root, "7", "task", strconv.Itoa(tid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(fakeThreadLine(tid, state)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "7", "stat"), []byte(fakeCountLine(count)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func fakeThreadLine(tid int, state string) string {
	return fmt.Sprintf("%d (worker) %s 4242 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 2 0 780 0 0\n", tid, state)
}

func fakeCountLine(threads int) string {
	return fmt.Sprintf("7 (worker) Z 4242 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 %d 0 780 0 0\n", threads)
}

// Where the process can be asked whether its whole thread group has exited
// (its pidfd), that decides first; else a listing that the count says holds
// every thread, none of them running, stands only where that group is empty
// or an exited thread other than the main one stands in it: a thread that
// ran an exec takes over the main thread's tid and start time, so the main
// thread alone, read twice, may be two threads. With no one to ask, the count
// alone decides. Threads that never settle are an error after a bounded
// number of listings, not a wait.
func TestRunningThreadAsksWhetherTheGroupExited(t *testing.T) {
	notEmpty := func(int) (bool, bool) { return false, true }
	exited := func(int) (bool, bool) { return true, true }
	unknown := func(int) (bool, bool) { return false, false }
	// The group has exited: none runs, whatever a listing would say.
	root := fakeThreads(t, 2, map[int]string{7: "Z", 11: "S"})
	if tid, runs, err := runningThreadIn(root, 7, exited); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, its group exited", tid, runs, err)
	}
	// The main thread alone, counted alone, while the group is not empty:
	// a thread left out of the listing ran an exec, holds the main thread's
	// tid now, started thread 11 and exited its own as the main thread was
	// read again. The threads are listed again, and 11 found.
	root = fakeThreads(t, 1, map[int]string{7: "Z"})
	serveStat(t, filepath.Join(root, "7", "task", "7", "stat"), func(n int) string {
		if n == 2 {
			dir := filepath.Join(root, "7", "task", "11")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(fakeThreadLine(11, "S")), 0o644); err != nil {
				t.Error(err)
			}
		}
		return fakeThreadLine(7, "Z")
	})
	if tid, runs, err := runningThreadIn(root, 7, notEmpty); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, the main thread read twice not taken for one exited thread while the group is not empty", tid, runs, err)
	}
	// An exited thread other than the main one (a zombie a tracer holds)
	// stands in the listing: no exec came between the main thread's reads,
	// and none runs, though the group is not empty.
	root = fakeThreads(t, 2, map[int]string{7: "Z", 8: "Z"})
	if tid, runs, err := runningThreadIn(root, 7, notEmpty); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, 8 a zombie a tracer holds", tid, runs, err)
	}
	// The same, where the exec came as the main thread was read again: the
	// exec's thread waited for every other thread to be released first (8
	// here), so 8, read again after the main thread, is gone, stands for
	// nothing, and the threads are listed again. Read before the main
	// thread, 8 would have stood for an exited thread there throughout.
	root = fakeThreads(t, 2, map[int]string{7: "Z", 8: "Z"})
	serveStat(t, filepath.Join(root, "7", "task", "7", "stat"), func(n int) string {
		if n == 2 {
			if err := os.RemoveAll(filepath.Join(root, "7", "task", "8")); err != nil {
				t.Error(err)
			}
			dir := filepath.Join(root, "7", "task", "11")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(fakeThreadLine(11, "S")), 0o644); err != nil {
				t.Error(err)
			}
		}
		return fakeThreadLine(7, "Z")
	})
	if tid, runs, err := runningThreadIn(root, 7, notEmpty); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, 8 not read again before the main thread", tid, runs, err)
	}
	// A vhost worker (flags PF_USER_WORKER) is no such thread: an exec does
	// not wait for one to be released. Here nothing else stands, the group
	// is not empty, and the threads never settle: an error.
	root = fakeThreads(t, 2, map[int]string{7: "Z"})
	worker := filepath.Join(root, "7", "task", "8")
	if err := os.MkdirAll(worker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, "stat"), []byte("8 (vhost-7) Z 4242 7 7 0 -1 16384 0 0 0 0 0 0 0 0 20 0 2 0 780 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runningThreadIn(root, 7, notEmpty); err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("an exited vhost worker taken to rule out an exec: err %v; want the threads never settling said", err)
	}
	// A vhost worker seen in one listing keeps the rule off for the rest of
	// the read, though released (8 here, gone by the count's read): the
	// exec its release may have let through came all the same.
	root = fakeThreads(t, 3, map[int]string{7: "Z", 9: "Z"})
	vhost := filepath.Join(root, "7", "task", "8")
	if err := os.MkdirAll(vhost, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vhost, "stat"), []byte("8 (vhost-7) Z 4242 7 7 0 -1 16384 0 0 0 0 0 0 0 0 20 0 3 0 780 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	serveStat(t, filepath.Join(root, "7", "stat"), func(n int) string {
		if n == 1 {
			if err := os.RemoveAll(vhost); err != nil {
				t.Error(err)
			}
			return fakeCountLine(3)
		}
		return fakeCountLine(2)
	})
	if _, _, err := runningThreadIn(root, 7, notEmpty); err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("a vhost worker released after the first listing forgotten: err %v; want the threads never settling said", err)
	}
	// The main thread is read last in a listing: every other thread is read
	// before it, so one that stands for an exited thread was there before
	// the main thread's first read.
	root = fakeThreads(t, 2, map[int]string{7: "Z", 8: "Z"})
	var leaderReads atomic.Int32
	serveStat(t, filepath.Join(root, "7", "task", "7", "stat"), func(int) string {
		leaderReads.Add(1)
		return fakeThreadLine(7, "Z")
	})
	serveStat(t, filepath.Join(root, "7", "task", "8", "stat"), func(n int) string {
		if n == 1 && leaderReads.Load() != 0 {
			t.Error("the main thread was read before thread 8 in the listing")
		}
		return fakeThreadLine(8, "Z")
	})
	if tid, runs, err := runningThreadIn(root, 7, notEmpty); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, 8 a zombie a tracer holds", tid, runs, err)
	}
	// No one to ask: the count alone decides.
	root = fakeThreads(t, 1, map[int]string{7: "Z"})
	if tid, runs, err := runningThreadIn(root, 7, unknown); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, its main thread all it counts", tid, runs, err)
	}
	// Threads being released, the count lower at each listing, for more
	// listings than threadListings: no error, for each is progress; once
	// the count comes down to what the listing holds, none runs.
	root = fakeThreads(t, 2, map[int]string{7: "Z", 8: "Z"})
	serveStat(t, filepath.Join(root, "7", "stat"), func(n int) string {
		return fakeCountLine(max(2*threadListings+2-n, 2))
	})
	if tid, runs, err := runningThreadIn(root, 7, unknown); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, its threads released one by one", tid, runs, err)
	}
	// A count higher than any listing: an error after threadListings
	// listings with no thread released, never a wait for ever.
	root = fakeThreads(t, 3, map[int]string{7: "Z", 8: "Z"})
	if _, _, err := runningThreadIn(root, 7, unknown); err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("a count no listing makes up: err %v; want it said", err)
	}
	// A thread released as each listing is taken (listed, its stat gone
	// by its read: 20, then 21), while one that runs (11) is left out: the
	// threads are listed again until 11 is found.
	root = fakeThreads(t, 2, map[int]string{7: "Z"})
	mkdir := func(tid int) {
		if err := os.MkdirAll(filepath.Join(root, "7", "task", strconv.Itoa(tid)), 0o755); err != nil {
			t.Error(err)
		}
	}
	mkdir(20)
	serveStat(t, filepath.Join(root, "7", "stat"), func(n int) string {
		switch n {
		case 1:
			if err := os.RemoveAll(filepath.Join(root, "7", "task", "20")); err != nil {
				t.Error(err)
			}
			mkdir(21)
		case 2:
			if err := os.RemoveAll(filepath.Join(root, "7", "task", "21")); err != nil {
				t.Error(err)
			}
			mkdir(11)
			if err := os.WriteFile(filepath.Join(root, "7", "task", "11", "stat"), []byte(fakeThreadLine(11, "S")), 0o644); err != nil {
				t.Error(err)
			}
		}
		return fakeCountLine(2)
	})
	if tid, runs, err := runningThreadIn(root, 7, unknown); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, the listings that left it out not taken for alike", tid, runs, err)
	}
	// The main thread alone, the group not empty, and nothing else ever
	// listed: the same error, after threadListings listings.
	root = fakeThreads(t, 1, map[int]string{7: "Z"})
	if _, _, err := runningThreadIn(root, 7, notEmpty); err == nil || !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("a group not empty whose threads never list: err %v; want it said", err)
	}
}

// The pidfd of a process tells whether its whole thread group has exited:
// not while a thread of it runs on after its main thread exited, and so once
// every thread of it has. A /proc that cannot confirm what the pidfd names
// (one with no self/fdinfo) leaves it untold.
func TestGroupExitedAsksThePidfd(t *testing.T) {
	if fd, err := unix.PidfdOpen(os.Getpid(), 0); err != nil {
		// A kernel before 5.3, or a seccomp filter (a sandbox): untold,
		// which RunningThread reads its threads for.
		t.Skipf("pidfd_open refused here: %v", err)
	} else {
		_ = unix.Close(fd)
	}
	cmd := startMainThreadExited(t)
	if exited, known := groupExitedIn("/proc")(cmd.Process.Pid); !known || exited {
		t.Fatalf("a process whose main thread exited, another running: exited %v, known %v; want not exited, known", exited, known)
	}
	pid, _ := startZombie(t)
	if exited, known := groupExitedIn("/proc")(pid); !known || !exited {
		t.Fatalf("zombie %d: exited %v, known %v; want exited, known", pid, exited, known)
	}
	if exited, known := groupExitedIn(t.TempDir())(pid); known {
		t.Fatalf("zombie %d read from a /proc with no self/fdinfo: exited %v, known; want untold", pid, exited)
	}
	if _, known := groupExitedIn("/proc")(os.Getpid() + 1<<22 + 1); known {
		t.Fatal("a pid beyond pid_max: known; want untold")
	}
}

// serveStat makes path, a stat in a test's /proc, a FIFO whose writer, on
// each open of it in turn, calls line with the open's number (from 1) and
// gives what it returns: what line does to the /proc first is done between
// a reader's reads. Past 100 opens it gives a line that does not parse,
// failing the test. Each open is given a FIFO of its own: the writer puts a
// new one at path before it closes the one it wrote, so a reader reads one
// line to its end, and the next open waits for the next. stop, which the
// test's end calls too, ends the writer and leaves no file at path.
func serveStat(t *testing.T, path string, line func(n int) string) (stop func()) {
	t.Helper()
	fifo := func() error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return unix.Mkfifo(path, 0o600)
	}
	if err := fifo(); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for n := 1; ; n++ {
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			if done.Load() {
				_ = f.Close()
				return
			}
			l := line(n)
			if n > 100 {
				// RunningThread reading on and on: a line no stat has ends
				// it, with an error, rather than the test's time.
				t.Errorf("%s read %d times", path, n)
				l = "garbled\n"
			}
			_, werr := f.WriteString(l)
			ferr := fifo()
			_ = f.Close()
			if werr != nil || ferr != nil {
				t.Error(werr, ferr)
				return
			}
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			done.Store(true)
			// A reader that takes nothing frees the writer from its open.
			r, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Error(err)
				return
			}
			<-ended
			_ = r.Close()
			_ = os.Remove(path)
		})
	}
	t.Cleanup(stop)
	return stop
}

// startZombie starts a child that exits at once and is not waited for, and
// returns its pid and stat once it reads as a zombie; it is waited for when
// the test ends.
func startZombie(t *testing.T) (int, ProcStat) {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	deadline := time.Now().Add(time.Minute) // turns a hang into a failure
	for {
		if st, ok := ReadProcStat(cmd.Process.Pid); ok && st.State == 'Z' {
			return cmd.Process.Pid, st
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d did not become a zombie", cmd.Process.Pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// threadsUnreadable makes every read of a process's threads (runningThread)
// fail until the test ends.
func threadsUnreadable(t *testing.T) {
	t.Helper()
	read := runningThread
	t.Cleanup(func() { runningThread = read })
	runningThread = func(pid int) (int, bool, error) {
		return 0, false, fmt.Errorf("list process %d's threads: %w", pid, syscall.EACCES)
	}
}

// A process whose stat says Z, and whose threads could not be read, has not
// exited for Exited: keploy goes on treating as running what it cannot tell
// has ended. Read, the same zombie has.
func TestExitedTakesAProcessWhoseThreadsCouldNotBeReadForRunning(t *testing.T) {
	pid, st := startZombie(t)
	if !Exited(pid, st) {
		t.Fatalf("zombie %d, its threads read: not exited; want exited", pid)
	}
	threadsUnreadable(t)
	if Exited(pid, st) {
		t.Fatalf("zombie %d, its threads not read: exited; want running: the read failed, so it is not known to have ended", pid)
	}
}
