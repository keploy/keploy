//go:build darwin

package utils

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// buildSignalCounter compiles a stand-in app that counts the SIGINTs it is
// sent: its leader starts a child in its own process group, and each of the
// two takes SIGINT (signal.Notify), prints its pid once it counts, and, from
// the first SIGINT until 300 ms pass with no other, counts every SIGINT sent
// to it with kqueue's EVFILT_SIGNAL, which records each one, also two the
// kernel delivers as one. Each then appends "pid count" to OUT and exits;
// the leader waits for its child first.
func buildSignalCounter(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("fmt";"os";"os/exec";"os/signal";"syscall";"time")
func count(out string) {
	signal.Notify(make(chan os.Signal, 8), syscall.SIGINT)
	kq, err := syscall.Kqueue()
	if err != nil { os.Exit(2) }
	ev := make([]syscall.Kevent_t, 1)
	syscall.SetKevent(&ev[0], int(syscall.SIGINT), syscall.EVFILT_SIGNAL, syscall.EV_ADD)
	if _, err := syscall.Kevent(kq, ev, nil, nil); err != nil { os.Exit(2) }
	fmt.Println(os.Getpid())
	var n int64
	wait := syscall.NsecToTimespec(int64(60 * time.Second))
	for {
		got, err := syscall.Kevent(kq, nil, ev, &wait)
		if err == syscall.EINTR { continue }
		if err != nil { os.Exit(2) }
		if got == 0 { break }
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
	if len(os.Args) > 2 { count(out); return }
	child := exec.Command(os.Args[0], out, "child")
	child.Stdout = os.Stdout
	if err := child.Start(); err != nil { os.Exit(3) }
	count(out)
	_ = child.Wait()
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "signal-counter")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build the signal counter: %v\n%s", err, out)
	}
	return bin
}

// The stop sends each process of an app's group its SIGINT once, by its pid,
// as Linux's one signal to the group does. Sending the group the SIGINT as
// well reached each process twice, and an app that takes a second SIGINT as
// "stop now" (Go's signal.NotifyContext once its stop is called) cuts its own
// graceful exit short.
func TestTheStopInterruptsEachProcessOfAGroupOnce(t *testing.T) {
	out := filepath.Join(t.TempDir(), "counts")
	app := exec.Command(buildSignalCounter(t), out)
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
	var pids []int
	for len(pids) < 2 {
		l, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the signal counter said %q, %v; want the pids of its leader and its child", l, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(l))
		if err != nil {
			t.Fatalf("the signal counter said %q; want a pid", l)
		}
		pids = append(pids, pid)
	}

	core, logs := observer.New(zap.DebugLevel)
	killed, err := InterruptProcessTreeKilled(zap.New(core), app.Process.Pid, syscall.SIGINT)
	if err != nil || killed != "" {
		t.Fatalf("the stop answered %v and said it killed %q; want neither", err, killed)
	}
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("the stop logged an ERROR: %q %v", e.Message, e.ContextMap())
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the signal counter had not exited 10s after the stop")
	}
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
	for _, pid := range pids {
		if counts[pid] != "1" {
			t.Errorf("process %d of the app's group was sent SIGINT %q times; want once (counts %v)", pid, counts[pid], counts)
		}
	}
}

// startSleep starts a sleep in process group pgid (0: a group of its own),
// and reaps it as it ends (ended); its cleanup kills it by its pid.
func startSleep(t *testing.T, pgid int) (pid int, ended chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended = make(chan struct{})
	go func() { _ = cmd.Wait(); close(ended) }()
	t.Cleanup(func() {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		<-ended
	})
	return cmd.Process.Pid, ended
}

func endsWithin(ended chan struct{}, d time.Duration) bool {
	select {
	case <-ended:
		return true
	case <-time.After(d):
		return false
	}
}

// stubProcessReads has the stop's reads of the process table answer list for
// group listings and, for a pid in records, that record; other pids are read
// from the table.
func stubProcessReads(t *testing.T, list func(pgid int) ([]int, error), records map[int]procsock.Proc) {
	t.Helper()
	realList, realRecord := groupPIDs, procRecord
	t.Cleanup(func() { groupPIDs, procRecord = realList, realRecord })
	if list != nil {
		groupPIDs = list
	}
	procRecord = func(pid int) (procsock.Proc, error) {
		if p, ok := records[pid]; ok {
			return p, nil
		}
		return realRecord(pid)
	}
}

// The stop's signal to a process group on macOS (signalGroupByMembers),
// sent to each process of the group by its pid: what it reaches, and that
// it answers no refusal and logs no ERROR for a process that exited, a group
// of zombies only, or a process keploy may not signal beside one it reached.
func TestTheStopsSignalToAGroupReachesEachProcessWithNoFalseRefusal(t *testing.T) {
	quiet := func(t *testing.T) (*zap.Logger, func()) {
		core, logs := observer.New(zap.DebugLevel)
		return zap.New(core), func() {
			for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
				t.Errorf("the signal logged an ERROR: %q %v", e.Message, e.ContextMap())
			}
		}
	}

	t.Run("its SIGKILL reaches a process that joined the group after its list was read", func(t *testing.T) {
		leader, leaderEnded := startSleep(t, 0)
		late, lateEnded := startSleep(t, leader)
		stubProcessReads(t, func(pgid int) ([]int, error) {
			pids, err := procsock.GroupPIDs(pgid)
			return slices.DeleteFunc(pids, func(p int) bool { return p == late }), err
		}, nil)
		logger, noError := quiet(t)
		if err := signalGroupByMembers(logger, -leader, syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL of group %d answered %v; want none", leader, err)
		}
		noError()
		if !endsWithin(leaderEnded, 5*time.Second) || !endsWithin(lateEnded, 5*time.Second) {
			t.Fatalf("after the group %d's SIGKILL, its leader or the process %d its list missed runs on", leader, late)
		}
	})

	t.Run("a group whose list cannot be read is signalled as a group", func(t *testing.T) {
		leader, leaderEnded := startSleep(t, 0)
		member, memberEnded := startSleep(t, leader)
		stubProcessReads(t, func(int) ([]int, error) { return nil, syscall.EIO }, nil)
		if err := signalGroupByMembers(zap.NewNop(), -leader, syscall.SIGINT); err != nil {
			t.Fatalf("SIGINT of group %d answered %v; want none", leader, err)
		}
		if !endsWithin(leaderEnded, 5*time.Second) || !endsWithin(memberEnded, 5*time.Second) {
			t.Fatalf("after the SIGINT of group %d, whose list cannot be read, its leader or member %d runs on", leader, member)
		}
	})

	t.Run("a group of zombies only answers its SIGKILL with no refusal", func(t *testing.T) {
		zombie := exec.Command("/usr/bin/true")
		zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := zombie.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = zombie.Wait() })
		z := zombie.Process.Pid
		for deadline := time.Now().Add(10 * time.Second); !exitedUnreaped(z); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d never became a zombie", z)
			}
		}
		logger, noError := quiet(t)
		if err := signalGroupByMembers(logger, -z, syscall.SIGKILL); err != nil {
			t.Errorf("SIGKILL of group %d, a zombie's only, answered %v; want none", z, err)
		}
		noError()
	})

	t.Run("a process that exited after the list was read is no refusal", func(t *testing.T) {
		// No process has this pid: XNU's pids stop at 99999 (PID_MAX).
		const g = 1_000_000
		if err := syscall.Kill(g, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("signal 0 to pid %d answered %v; want ESRCH", g, err)
		}
		stubProcessReads(t, func(int) ([]int, error) { return []int{g}, nil },
			map[int]procsock.Proc{g: {PID: g, PPID: 1, PGID: g}})
		logger, noError := quiet(t)
		if err := signalGroupByMembers(logger, -g, syscall.SIGINT); err != nil {
			t.Errorf("SIGINT of group %d, whose only process exited after its list was read, answered %v; want none", g, err)
		}
		noError()
	})

	// Signal 0 only: launchd (pid 1), which keploy may not signal, is
	// never sent a signal that acts.
	t.Run("a process keploy may not signal is no refusal when the signal reached another", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Fatal("run as root, launchd can be signalled: this row needs a user that may not")
		}
		app, _ := startSleep(t, 0)
		stubProcessReads(t, func(int) ([]int, error) { return []int{1, app}, nil },
			map[int]procsock.Proc{1: {PID: 1, PPID: 0, PGID: app}})
		logger, noError := quiet(t)
		if err := signalGroupByMembers(logger, -app, 0); err != nil {
			t.Errorf("signal 0 to group %d answered %v; want none: it reached %d, though not launchd", app, err, app)
		}
		noError()
	})
}
