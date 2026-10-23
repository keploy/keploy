//go:build linux

// Package nsswitchtest runs a test's processes on a host of their own: a mount
// namespace whose /etc/nsswitch.conf is a file the test gives, so that a test
// can judge, as root, what code does to the host's /etc/nsswitch.conf, and
// what an application keploy starts sees of it, without touching the
// machine's. The machine's file is checked to be byte for byte as it was once
// the test is over.
//
// A test that uses it runs as three kinds of process, each a run of the same
// test binary with the same -test.run, told apart by Role: the one go test
// started (""), which starts the host and judges how it ended; the host
// ("host"), in the namespace, which runs the test's body (Host); and the
// processes the body starts in the host's namespace (Spawn), each in a role
// the test names, which do what the test has them do there (keploy's part)
// and are judged by the host.
//
// Nothing it starts outlives the test: every process is started with a
// parent-death signal (SIGKILL) and in a process group of its own, which
// t.Cleanup kills.
package nsswitchtest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/ownthread"
	"golang.org/x/sys/unix"
)

// Path is the file a test's host has of its own.
const Path = "/etc/nsswitch.conf"

const (
	roleEnv = "KEPLOY_NSSWITCHTEST_ROLE"
	confEnv = "KEPLOY_NSSWITCHTEST_CONF"
)

// Role is the role this process of the test runs in: "" for the one go test
// started, "host" for the host, or the role Spawn gave it.
func Role() string { return os.Getenv(roleEnv) }

// Host runs body in a host of its own whose /etc/nsswitch.conf is conf. Called
// by the test go test started, it skips the test unless it is root, runs the
// test again as the host, fails the test if the host's run did, and fails it
// if the machine's /etc/nsswitch.conf is not byte for byte as it was. Called
// by the host, it mounts conf over the host's /etc/nsswitch.conf and runs
// body. A process of another role must not call it.
func Host(t *testing.T, conf []byte, body func(t *testing.T)) {
	t.Helper()
	switch Role() {
	case "":
	case "host":
		// What a process the host started leaves behind once it is killed
		// becomes the host's to reap (Reap), whatever this machine's pid 1
		// does with orphans.
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			t.Fatalf("make the host a subreaper: %v", err)
		}
		src := os.Getenv(confEnv)
		if err := unix.Mount(src, Path, "", unix.MS_BIND, ""); err != nil {
			t.Fatalf("mount the host's own %s: %v", Path, err)
		}
		if got, err := os.ReadFile(Path); err != nil || !bytes.Equal(got, conf) {
			t.Fatalf("the host's %s is %q (%v), want %q", Path, got, err, conf)
		}
		body(t)
		return
	default:
		t.Fatalf("Host called in role %q", Role())
	}

	if err := CanMakeMountNamespaces(); err != nil {
		t.Skipf("the host is a mount namespace of its own, which this process cannot make (it needs CAP_SYS_ADMIN, and unshare let through): %v", err)
	}
	machine, err := os.ReadFile(Path)
	if err != nil {
		t.Skipf("this machine has no %s to mount the host's over: %v", Path, err)
	}
	src := filepath.Join(t.TempDir(), "host-nsswitch.conf")
	if err := os.WriteFile(src, conf, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := self(t, "host", confEnv+"="+src)
	cmd.Stdout, cmd.Stderr = &out, &out
	// os/exec makes the namespace, and every mount in it private, in the
	// child before it runs the test: nothing the host mounts reaches the
	// machine.
	cmd.SysProcAttr.Unshareflags = syscall.CLONE_NEWNS
	// Waited for here, so a cleanup could only find it reaped (and its
	// group's id maybe another's); should the test binary die first, the
	// host's parent-death signal ends it, and each process it started has
	// one of its own.
	err = run(cmd)
	t.Logf("the host's run:\n%s", out.String())
	if err != nil {
		t.Errorf("the host's run failed: %v", err)
	} else if !strings.Contains(out.String(), "--- PASS: "+t.Name()) {
		t.Errorf("the host's run did not pass %s", t.Name())
	}
	if after, err := os.ReadFile(Path); err != nil || !bytes.Equal(after, machine) {
		t.Errorf("this machine's %s changed while the test ran: %q (%v), was %q", Path, after, err, machine)
	}
}

// Proc is a process Spawn started.
type Proc struct {
	Cmd *exec.Cmd
	// In is its standard input's other end: it reads EOF once In is
	// closed, or the host has exited.
	In *os.File
	// Done is closed once it has exited and been reaped; Err is then how it
	// exited.
	Done <-chan struct{}
	Err  error
	out  string
}

// Output is what it has written so far.
func (p *Proc) Output() string { return Read(p.out) }

// Spawn starts, from the host, the test again in role, in the host's mount
// namespace, with env added to its environment. Its output goes to a file, not
// a pipe, so that its exit is all Done waits for: a process it starts may hold
// its output open long after (the application keploy starts, which writes to
// keploy's standard output). t.Cleanup kills its process group.
func Spawn(t *testing.T, role string, env ...string) *Proc {
	t.Helper()
	return Start(t, Command(t, role, env...))
}

// Command is the command Spawn starts: the test binary, running this test
// alone, in role, with env added to its environment, in a process group of
// its own and with a parent-death signal. A test may change it (its
// credentials, its binary) before it starts it with Start.
func Command(t *testing.T, role string, env ...string) *exec.Cmd {
	return self(t, role, env...)
}

// Start starts cmd, a Command, as Spawn does.
func Start(t *testing.T, cmd *exec.Cmd) *Proc {
	t.Helper()
	// Its role is the last of cmd's environment, as os/exec keeps the last of
	// a name (the host's own comes first).
	role := "proc"
	for _, e := range cmd.Env {
		if v, ok := strings.CutPrefix(e, roleEnv+"="); ok {
			role = v
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	p := &Proc{Cmd: cmd, In: w, out: filepath.Join(t.TempDir(), role+".out")}
	out, err := os.Create(p.out)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	p.Cmd.Stdin, p.Cmd.Stdout, p.Cmd.Stderr = r, out, out
	done := make(chan struct{})
	p.Done = done
	started := make(chan error, 1)
	// exited is set, under mu, once it has exited and before it is reaped:
	// until then its group's id is its own, and the cleanup below, under mu
	// too, may kill the group.
	var mu sync.Mutex
	exited := false
	// Started, and waited for, from a thread kept for as long as it runs: a
	// parent-death signal is sent when the thread that started the process
	// exits, not the process.
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := p.Cmd.Start()
		started <- err
		if err == nil {
			var info unix.Siginfo
			_ = unix.Waitid(unix.P_PID, p.Cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			mu.Lock()
			exited = true
			mu.Unlock()
			p.Err = p.Cmd.Wait()
		}
		close(done)
	}()
	if err := <-started; err != nil {
		t.Fatalf("start the %s: %v", role, err)
	}
	pid := p.Cmd.Process.Pid
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if !exited {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	r.Close()
	return p
}

// Exited waits for p to exit, and fails t if it has not a minute on.
func (p *Proc) Exited(t *testing.T) {
	t.Helper()
	select {
	case <-p.Done:
	case <-time.After(time.Minute):
		t.Fatalf("waited a minute for %s to exit; its output:\n%s", p.Cmd.Args, p.Output())
	}
}

// self is the test binary, running this test alone, in role.
func self(t *testing.T, role string, env ...string) *exec.Cmd {
	levels := strings.Split(t.Name(), "/")
	for i, l := range levels {
		levels[i] = "^" + regexp.QuoteMeta(l) + "$"
	}
	// Its own bound: a run that hangs ends with its goroutines' stacks in its
	// output, which Host logs, instead of holding the test for good.
	cmd := exec.Command(os.Args[0], "-test.run="+strings.Join(levels, "/"), "-test.v", "-test.count=1", "-test.timeout=5m")
	cmd.Env = append(append(os.Environ(), roleEnv+"="+role), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return cmd
}

// KillGroupUnless kills, at t's cleanup, the process group of the process
// pid leads, unless gone says by then that it may have been reaped: its
// group's id may then be another's, which a SIGKILL from root would reach.
// gone must say so before the reap, not after it.
func KillGroupUnless(t *testing.T, pid int, gone func() bool) {
	t.Cleanup(func() {
		if !gone() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

// CanMakeMountNamespaces says why this process cannot make a mount namespace
// and mount in it as keploy's view does (no CAP_SYS_ADMIN, a seccomp profile
// that refuses unshare, an LSM that refuses mount), or nil: a test that needs
// one skips where it cannot, as a non-root one skips, rather than fail for
// the machine's sake. It tries on a thread it then discards
// (ownthread.Run).
func CanMakeMountNamespaces() error {
	errc := make(chan error, 1)
	probe := func() {
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			errc <- fmt.Errorf("unshare: %w", err)
			return
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
			errc <- fmt.Errorf("make / a slave: %w", err)
			return
		}
		dir := filepath.Dir(Path)
		if err := unix.Mount("nsswitchtest-probe", dir, "tmpfs", 0, "size=4k"); err != nil {
			errc <- fmt.Errorf("mount a tmpfs over %s: %w", dir, err)
			return
		}
		errc <- unix.Unmount(dir, unix.MNT_DETACH)
	}
	ownthread.Run(probe)
	return <-errc
}

// run runs cmd from a thread kept for as long as it runs (see Spawn).
func run(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- cmd.Run()
	}()
	return <-done
}

// Await waits for cond to hold, checking every 10 ms, and fails t once what
// is waited for (what) is not there after a minute: a bound for a test that
// would otherwise hang, far past what any of these waits takes.
func Await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited a minute for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Gone reports whether the process pid has exited and been reaped.
func Gone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// Reap waits for pid, a process the host was left (a killed process's
// child), to exit, and reaps it; it fails t once that has not happened a
// minute on.
func Reap(t *testing.T, pid int) {
	t.Helper()
	Await(t, fmt.Sprintf("process %d to exit", pid), func() bool {
		var ws unix.WaitStatus
		got, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		return got == pid || errors.Is(err, unix.ECHILD) && Gone(pid)
	})
}

// ReadIn is the content of path as the process pid sees it, in its mount
// namespace, or a description of why it has none. Its symlinks resolve in its
// root (openat2's RESOLVE_IN_ROOT), as they do for it: through
// /proc/PID/root alone, an absolute one (Fedora's /etc/nsswitch.conf, a link
// to /etc/authselect/nsswitch.conf) resolves in the reader's.
func ReadIn(pid int, path string) string {
	root, err := unix.Open(fmt.Sprintf("/proc/%d/root", pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	defer unix.Close(root)
	fd, err := unix.Openat2(root, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT})
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(f); err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return b.String()
}

// Read is the content of path, or a description of why it has none.
func Read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return string(b)
}
