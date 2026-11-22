//go:build darwin

package utils

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// buildJoiningApp compiles a stand-in app whose leader, at the first SIGINT
// it is sent, starts a child that joins its process group once it is ready
// to count SIGINTs: it starts in a group of its own, so that no signal can
// reach it before it counts, and then moves into the leader's (setpgid). Each
// of the two takes SIGINT (signal.Notify), prints its pid once it counts, and
// counts every SIGINT sent to it with kqueue's EVFILT_SIGNAL, which records
// each one, from the first until 300 ms pass with no other. Each then appends
// "pid count" to OUT and exits; the leader waits for its child first.
func buildJoiningApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("fmt";"os";"os/exec";"os/signal";"strconv";"syscall";"time")
func count(out string, ready, first func()) {
	signal.Notify(make(chan os.Signal, 8), syscall.SIGINT)
	kq, err := syscall.Kqueue()
	if err != nil { os.Exit(2) }
	ev := make([]syscall.Kevent_t, 1)
	syscall.SetKevent(&ev[0], int(syscall.SIGINT), syscall.EVFILT_SIGNAL, syscall.EV_ADD)
	if _, err := syscall.Kevent(kq, ev, nil, nil); err != nil { os.Exit(2) }
	ready()
	var n int64
	wait := syscall.NsecToTimespec(int64(60 * time.Second))
	for {
		got, err := syscall.Kevent(kq, nil, ev, &wait)
		if err == syscall.EINTR { continue }
		if err != nil { os.Exit(2) }
		if got == 0 { break }
		if n == 0 { first() }
		n += int64(ev[0].Data)
		wait = syscall.NsecToTimespec(int64(300 * time.Millisecond))
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil { os.Exit(2) }
	fmt.Fprintf(f, "%d %d\n", os.Getpid(), n)
	_ = f.Close()
}
func main() {
	out := os.Args[1]
	if len(os.Args) > 2 {
		pgid, err := strconv.Atoi(os.Args[2])
		if err != nil { os.Exit(4) }
		count(out, func() {
			if err := syscall.Setpgid(0, pgid); err != nil { os.Exit(4) }
			fmt.Println(os.Getpid())
		}, func() {})
		return
	}
	var joiner *exec.Cmd
	count(out, func() { fmt.Println(os.Getpid()) }, func() {
		joiner = exec.Command(os.Args[0], out, strconv.Itoa(syscall.Getpgrp()))
		joiner.Stdout = os.Stdout
		joiner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := joiner.Start(); err != nil { os.Exit(3) }
	})
	if joiner != nil { _ = joiner.Wait() }
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "joining-app")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build the joining app: %v\n%s", err, out)
	}
	return bin
}

// A process that joins an app's group after the stop signalled each process
// of it by its pid (forked as the app handles the SIGINT) is sent the SIGINT
// too, once, by the stop's wait for the group, and exits on it on its own:
// the stop kills nothing. Before, the wait waited for it to the grace's end,
// and the group's SIGKILL ended it with no signal it could act on.
func TestTheStopInterruptsAProcessThatJoinedTheGroupAfterItsSignalOnce(t *testing.T) {
	out := filepath.Join(t.TempDir(), "counts")
	app := exec.Command(buildJoiningApp(t), out)
	app.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	app.Stdout = w
	err = app.Start()
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	exited := make(chan struct{})
	go func() { _ = app.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-app.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	lines := bufio.NewReader(r)
	readPid := func(who string) int {
		l, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the joining app said %q, %v; want the pid of its %s", l, err, who)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(l))
		if err != nil {
			t.Fatalf("the joining app said %q; want a pid", l)
		}
		return pid
	}
	leader := readPid("leader")

	core, logs := observer.New(zap.DebugLevel)
	killed, err := InterruptProcessTreeKilled(zap.New(core), app.Process.Pid, syscall.SIGINT)
	if err != nil || killed != "" {
		t.Errorf("the stop answered %v and said it killed %q; want neither", err, killed)
	}
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("the stop logged an ERROR: %q %v", e.Message, e.ContextMap())
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the joining app had not exited 10s after the stop")
	}
	joiner := readPid("child that joined its group")
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if pid, n, ok := strings.Cut(l, " "); ok {
			p, _ := strconv.Atoi(pid)
			counts[p] = n
		}
	}
	for _, pid := range []int{leader, joiner} {
		if counts[pid] != "1" {
			t.Errorf("process %d of the app's group was sent SIGINT %q times; want once (leader %d, joiner %d, counts %v)", pid, counts[pid], leader, joiner, counts)
		}
	}
}

// The group's wait signals only what joined the group after the stop's
// signal by pid named the processes it reached (sent), and nothing where
// that signal went to the group as one: its list could not be read, so what
// it reached is not known, and a second signal would reach them all again.
func TestTheStopsWaitSignalsOnlyWhatJoinedTheGroupAfterItsSignal(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	joinedSignals := func() int {
		return logs.FilterMessage("signalled a process that joined the process group after its signal").Len()
	}
	reads := 0
	stubProcessReads(t, func(pgid int) ([]int, error) {
		reads++
		if reads == 1 {
			return nil, errors.New("the list cannot be read")
		}
		return procsock.GroupPIDs(pgid)
	}, nil)
	pid, ended := startSleep(t, 0)
	// The first signal's list cannot be read: it goes to the group as one,
	// and names nothing it reached.
	sent, err := signalGroupByMembers(logger, -pid, syscall.SIGCONT)
	if err != nil || sent != nil {
		t.Fatalf("a signal to a group whose list cannot be read answered %v, %v; want nil, nil", sent, err)
	}
	if signalJoined == nil {
		t.Fatal("no signal to what joins a group after its signal is set on macOS")
	}
	// With no sent, and named, the sleep is not signalled again; unnamed, it
	// is, once.
	for _, s := range []map[int]bool{nil, {pid: true}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the wait's signal, sent %v, panicked: %v", s, r)
				}
			}()
			signalJoinedMembers(logger, pid, syscall.SIGTERM, s)
		}()
		if n := joinedSignals(); n != 0 || endsWithin(ended, 300*time.Millisecond) {
			t.Fatalf("the wait, sent %v, signalled %d processes; want none", s, n)
		}
	}
	sent = map[int]bool{}
	signalJoinedMembers(logger, pid, syscall.SIGTERM, sent)
	signalJoinedMembers(logger, pid, syscall.SIGTERM, sent)
	if n := joinedSignals(); n != 1 || !sent[pid] {
		t.Errorf("the wait signalled what joined the group %d times (sent %v); want once, and it named", n, sent)
	}
	if !endsWithin(ended, 5*time.Second) {
		t.Error("the sleep that joined the group after its signal was not sent the SIGTERM")
	}
}

// A kill that waits in the kernel for a held posix_spawn has reached its
// process, as one that returned has: beside a process keploy may not signal,
// the group's signal is no refusal, and logs no ERROR. Counted as a refusal,
// the stop of a group whose other process refuses said it could not signal
// the group, and let it run on.
func TestAKillThatWaitsOnAHeldSpawnReachesItsProcess(t *testing.T) {
	const pgid, refuses, held = 100, 101, 102
	stubProcessReads(t, func(int) ([]int, error) { return []int{refuses, held}, nil }, map[int]procsock.Proc{
		refuses: {PID: refuses, PPID: 1, PGID: pgid},
		held:    {PID: held, PPID: refuses, PGID: pgid},
	})
	realKill := killMember
	t.Cleanup(func() { killMember = realKill })
	killMember = func(_ *zap.Logger, pid int, sig syscall.Signal, _ time.Duration) error {
		switch pid {
		case refuses:
			return syscall.EPERM
		case held:
			return errKillWaits
		}
		t.Errorf("the stop signalled %d (%v); want only %d and %d", pid, sig, refuses, held)
		return nil
	}
	core, logs := observer.New(zap.DebugLevel)
	sent, err := signalGroupByMembers(zap.New(core), -pgid, syscall.SIGINT)
	if err != nil {
		t.Errorf("the group's signal, its kill of %d waiting on a held spawn, answered %v; want no refusal", held, err)
	}
	if !sent[refuses] || !sent[held] {
		t.Errorf("the group's signal named %v as sent; want %d and %d", sent, refuses, held)
	}
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("the signal logged an ERROR: %q %v", e.Message, e.ContextMap())
	}
}

// killWithin keeps at most one in-flight kill(2) goroutine per target pid:
// a repeat kill to a pid whose earlier kill is still blocked in the kernel
// logs a warning and returns errKillWaits without spawning a second goroutine,
// kills to any other pid (including the by-pid kill to the process that made a
// held spawn, even with many unrelated pids stuck) always go out and are never
// refused, and once a blocked kill finishes its pid entry is released cleanly.
func TestKillWithinDeduplicatesPerPIDWithoutBlockingOtherTargets(t *testing.T) {
	realSyscall := killSyscall
	release := make(chan struct{})
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }

	waitEmpty := func(d time.Duration) int {
		for deadline := time.Now().Add(d); ; time.Sleep(5 * time.Millisecond) {
			killWaitersMu.Lock()
			n := len(killWaiters)
			killWaitersMu.Unlock()
			if n == 0 || time.Now().After(deadline) {
				return n
			}
		}
	}

	t.Cleanup(func() {
		unblock()
		if n := waitEmpty(5 * time.Second); n != 0 {
			t.Errorf("killWaiters has %d entries after cleanup; want 0", n)
		}
		killSyscall = realSyscall
	})

	const (
		makerPID = 2001
		heldPID  = 2002
	)
	var mu sync.Mutex
	calls := map[int]int{}
	entered := make(chan int, 64)
	killSyscall = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		calls[pid]++
		mu.Unlock()
		if pid == makerPID || pid == -makerPID {
			// A by-pid kill to the spawn's maker returns at once.
			return nil
		}
		entered <- pid
		<-release
		return nil
	}
	callCount := func(pid int) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[pid]
	}

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	// 1. First kill to heldPID blocks in the kernel and returns errKillWaits.
	if err := killWithin(logger, heldPID, syscall.SIGINT, 20*time.Millisecond); !errors.Is(err, errKillWaits) {
		t.Fatalf("first kill of %d answered %v; want errKillWaits", heldPID, err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("first kill of %d never entered killSyscall", heldPID)
	}
	if got := logs.FilterLevelExact(zap.WarnLevel).Len(); got != 0 {
		t.Fatalf("logged %d warnings on first stuck kill; want 0", got)
	}

	// 2. Repeat kill to the same stuck pid spawns no second goroutine, logs a
	// warning, and returns errKillWaits (counted as reached).
	if err := killWithin(logger, heldPID, syscall.SIGKILL, 20*time.Millisecond); !errors.Is(err, errKillWaits) {
		t.Fatalf("repeat kill of %d answered %v; want errKillWaits", heldPID, err)
	}
	if got := callCount(heldPID); got != 1 {
		t.Fatalf("killSyscall ran %d times for stuck pid %d; want 1", got, heldPID)
	}
	warns := logs.FilterLevelExact(zap.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("logged %d warnings on duplicate stuck kill; want 1", len(warns))
	}
	if ctx := warns[0].ContextMap(); ctx["pid"] != int64(heldPID) {
		t.Errorf("duplicate kill warning fields = %v; want pid=%d", ctx, heldPID)
	}

	// 3. Even with 40 other unrelated pids stuck in flight, a kill to another
	// target (here, the held spawn's maker via signalGroupByMembers) always
	// goes out and is never refused.
	for pid := 3001; pid <= 3040; pid++ {
		if err := killWithin(logger, pid, syscall.SIGINT, 5*time.Millisecond); !errors.Is(err, errKillWaits) {
			t.Fatalf("stuck kill of unrelated pid %d answered %v; want errKillWaits", pid, err)
		}
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("stuck kill of unrelated pid %d never entered killSyscall", pid)
		}
	}

	stubProcessReads(t, func(int) ([]int, error) { return []int{makerPID, heldPID}, nil }, map[int]procsock.Proc{
		makerPID: {PID: makerPID, PPID: 1, PGID: makerPID},
		heldPID:  {PID: heldPID, PPID: makerPID, PGID: makerPID},
	})
	sent, err := signalGroupByMembers(logger, -makerPID, syscall.SIGKILL)
	if err != nil {
		t.Fatalf("SIGKILL of group %d with stuck waiters answered %v; want nil", makerPID, err)
	}
	if !sent[makerPID] || !sent[heldPID] {
		t.Fatalf("SIGKILL of group %d marked sent=%v; want %d and %d", makerPID, sent, makerPID, heldPID)
	}
	if got := callCount(makerPID); got != 1 {
		t.Fatalf("by-pid kill to spawn maker %d ran %d times; want 1", makerPID, got)
	}
	if got := callCount(heldPID); got != 1 {
		t.Fatalf("killSyscall ran %d times for already-stuck child %d; want 1", got, heldPID)
	}
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("the group kill logged an ERROR: %q %v", e.Message, e.ContextMap())
	}

	// 4. Once the stuck kills finish, their pid entries are released cleanly
	// and a subsequent kill to heldPID runs normally.
	unblock()
	if n := waitEmpty(2 * time.Second); n != 0 {
		t.Fatalf("killWaiters still has %d entries after unblocking; want 0", n)
	}
	killSyscall = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		calls[pid]++
		mu.Unlock()
		return nil
	}
	if err := killWithin(logger, heldPID, syscall.SIGINT, 200*time.Millisecond); err != nil {
		t.Fatalf("kill of %d after release answered %v; want nil", heldPID, err)
	}
	if got := callCount(heldPID); got != 2 {
		t.Fatalf("killSyscall ran %d times for %d after release; want 2", got, heldPID)
	}
	if n := waitEmpty(time.Second); n != 0 {
		t.Fatalf("killWaiters has %d entries after normal kill; want 0", n)
	}
}
