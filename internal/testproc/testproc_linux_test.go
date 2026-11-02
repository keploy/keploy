package testproc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// diesEnv names how TestDyingTestHelper ends, and the directory it writes
// the pids of what it started to.
const diesEnv = "KEPLOY_TESTPROC_DIES"

// TestDyingTestHelper starts a group (a shell that spins on a sleep, as the
// stop tests' apps do, and, for a test that fails, a sleep the shell started)
// and, once the test that runs it says so on its stdin, ends as diesEnv says:
// "fails" fails a subtest that started the group, then waits for its stdin's
// end; "panics" panics off the test's goroutine, as a test binary's timeout
// does; "killed" waits to be killed, as at a job's end.
func TestDyingTestHelper(t *testing.T) {
	how, dir, ok := strings.Cut(os.Getenv(diesEnv), " ")
	if !ok {
		t.Skip("a process of TestATestThatEndsEarlyLeavesNothingOfItsGroup")
	}
	stdin := bufio.NewReader(os.Stdin)
	run := func(t *testing.T) {
		script := `while :; do sleep 0.01; done`
		if how == "fails" {
			script = `sleep 600 & echo $! > "$1/member"; ` + script
		}
		cmd := exec.Command("sh", "-c", script, "sh", dir)
		Start(t, cmd)
		pids := strconv.Itoa(cmd.Process.Pid)
		if how == "fails" {
			for {
				b, err := os.ReadFile(filepath.Join(dir, "member"))
				if err == nil && strings.HasSuffix(string(b), "\n") {
					pids += " " + strings.TrimSpace(string(b))
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		// Whole once there: written, then renamed.
		if err := os.WriteFile(filepath.Join(dir, "pids.part"), []byte(pids), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "pids.part"), filepath.Join(dir, "pids")); err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.ReadString('\n'); err != nil {
			t.Fatalf("no word to end: %v", err)
		}
		switch how {
		case "fails":
			t.Fatal("the test fails before its end")
		case "panics":
			time.AfterFunc(0, func() { panic("the test timed out") })
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if how != "fails" {
		run(t)
		return
	}
	t.Run("early", run)
	_, _ = io.Copy(io.Discard, stdin)
}

// A test that ends before its end leaves nothing of a group it started:
// one that fails has the group killed by its cleanup, which leaves a
// process the leader started no more than the leader; one whose process
// dies with no cleanup run (its timeout's panic, a kill at a job's end) has
// the leader killed by the kernel. The stop tests' apps were started in a
// group the cleanup killed, with no death signal: a test binary that timed
// out, or was killed as its job ended, left an app's shell waking every
// 10 ms for hours after.
func TestATestThatEndsEarlyLeavesNothingOfItsGroup(t *testing.T) {
	for _, how := range []string{"fails", "panics", "killed"} {
		t.Run(how, func(t *testing.T) {
			dir := t.TempDir()
			// Its output to a file: a pipe would wait on what it leaves.
			out, err := os.Create(filepath.Join(dir, "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestDyingTestHelper$", "-test.v")
			cmd.Env = append(os.Environ(), diesEnv+"="+how+" "+dir)
			cmd.Stdout, cmd.Stderr = out, out
			word, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer word.Close()
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			helper := cmd.Process.Pid
			reaped := make(chan struct{})
			go func() { _ = cmd.Wait(); close(reaped) }()
			t.Cleanup(func() { _ = syscall.Kill(-helper, syscall.SIGKILL); <-reaped })
			said := func() string { b, _ := os.ReadFile(filepath.Join(dir, "out")); return string(b) }

			var pids []int
			for expired := beforeTheDeadline(t); pids == nil; {
				if b, err := os.ReadFile(filepath.Join(dir, "pids")); err == nil {
					for _, f := range strings.Fields(string(b)) {
						pid, err := strconv.Atoi(f)
						if err != nil {
							t.Fatalf("pids %q", b)
						}
						pids = append(pids, pid)
					}
					break
				}
				select {
				case <-reaped:
					t.Fatalf("the helper ended before it started its group:\n%s", said())
				case <-expired:
					t.Fatalf("the helper did not start its group:\n%s", said())
				case <-time.After(10 * time.Millisecond):
				}
			}
			// Each is held by a pidfd before the helper is let end, so that a
			// pid reused once it is gone reads as gone; and what is left is
			// killed through it.
			fds := make([]int, len(pids))
			for i, pid := range pids {
				fd, err := unix.PidfdOpen(pid, 0)
				if err != nil {
					t.Fatalf("pid %d, still waiting on the helper's word: %v", pid, err)
				}
				fds[i] = fd
				t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
			}
			if _, err := word.Write([]byte("end\n")); err != nil {
				t.Fatal(err)
			}
			if how == "killed" {
				_ = syscall.Kill(helper, syscall.SIGKILL)
			}
			if how != "fails" {
				<-reaped
			}
			// A test that failed is checked while its process runs on: its
			// cleanup, not its end, ended the group.
			bound := beforeTheDeadline(t)
			for i, fd := range fds {
				if !exits(t, fd, bound) {
					t.Fatalf("process %d of the group (of %v) outlived its test, which %s:\n%s", pids[i], pids, how, said())
				}
			}
			select {
			case <-reaped:
				if how == "fails" {
					t.Fatalf("the helper ended before its stdin's end:\n%s", said())
				}
			default:
			}
			_ = word.Close()
			<-reaped
			if how == "killed" {
				if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
					t.Fatalf("the helper ended %v, not killed:\n%s", cmd.ProcessState, said())
				}
				return
			}
			want := map[string]string{"fails": "--- FAIL: TestDyingTestHelper/early", "panics": "panic: the test timed out"}[how]
			if !strings.Contains(said(), want) {
				t.Fatalf("the helper did not end as one that %s (%q):\n%s", how, want, said())
			}
		})
	}
}

// exits reports whether the process behind pidfd exits before bound fires;
// the bound only turns a hang into a failure.
func exits(t *testing.T, pidfd int, bound <-chan time.Time) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, 10)
		if n > 0 {
			return true
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatalf("poll pidfd: %v", err)
		}
		select {
		case <-bound:
			return false
		default:
		}
	}
}

// A command that cannot start fails its test, and leaves no cleanup.
func TestACommandThatCannotStartFailsItsTest(t *testing.T) {
	ft := &fatalTB{TB: t}
	absent := filepath.Join(t.TempDir(), "absent")
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(ft, exec.Command(absent))
	}()
	<-done
	if !strings.Contains(ft.fatal, "absent") {
		t.Fatalf("the start failed as %q; want its failure", ft.fatal)
	}
	if ft.cleanups != 0 {
		t.Fatalf("a start that failed left %d cleanups", ft.cleanups)
	}
}

// fatalTB records its Fatalf and Cleanup, and ends its goroutine on Fatalf.
type fatalTB struct {
	testing.TB
	fatal    string
	cleanups int
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fatalTB) Cleanup(func()) { f.cleanups++ }

// beforeTheDeadline fires a quarter of the way to the test binary's
// deadline, and never with none.
func beforeTheDeadline(t *testing.T) <-chan time.Time {
	t.Helper()
	dl, ok := t.Deadline()
	if !ok {
		return nil
	}
	timer := time.NewTimer(time.Until(dl) / 4)
	t.Cleanup(func() { timer.Stop() })
	return timer.C
}
