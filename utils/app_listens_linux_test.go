package utils

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

const (
	listenChildEnv = "KEPLOY_TEST_LISTEN_CHILD"
	// listenChildAddrEnv is the address the child listens on, when not
	// 127.0.0.1:0.
	listenChildAddrEnv = "KEPLOY_TEST_LISTEN_CHILD_ADDR"
)

// TestListenChildHelper is the child process of
// TestAppListenPortsAreTheAppsAndItsChildrensListeningPorts: it listens on a
// port, prints it, and waits to be killed.
func TestListenChildHelper(t *testing.T) {
	if os.Getenv(listenChildEnv) == "" {
		t.Skip("the child process of another row")
	}
	addr := os.Getenv(listenChildAddrEnv)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		os.Exit(2)
	}
	fmt.Println(ln.Addr().(*net.TCPAddr).Port)
	time.Sleep(2 * time.Minute)
	os.Exit(0)
}

// The ports are those the application's process and its children listen on;
// a connection's own port is not one, nor is anything without a pid noted.
func TestAppListenPortsAreTheAppsAndItsChildrensListeningPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	own := ln.Addr().(*net.TCPAddr).Port
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	dialled := conn.LocalAddr().(*net.TCPAddr).Port
	// A listener on IPv6 is in /proc's tcp6 table, not its tcp one.
	wants := []int{own}
	if ln6, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		defer func() { _ = ln6.Close() }()
		wants = append(wants, ln6.Addr().(*net.TCPAddr).Port)
	} else {
		t.Logf("no IPv6 loopback here (%v): the tcp6 table is not checked", err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestListenChildHelper$")
	child.Env = append(os.Environ(), listenChildEnv+"=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the child did not say its port: %v", err)
	}
	kid, err := strconv.Atoi(line[:len(line)-1])
	if err != nil {
		t.Fatalf("the child said %q, not a port", line)
	}

	ctx := WithAppPID(context.Background())
	if got := AppListenPorts(ctx); got != nil {
		t.Fatalf("ports %v before any pid was noted", got)
	}
	NoteAppPID(ctx, os.Getpid())
	got := AppListenPorts(ctx)
	for _, want := range append(wants, kid) {
		if !slices.Contains(got, want) {
			t.Errorf("ports %v lack %d", got, want)
		}
	}
	if slices.Contains(got, dialled) {
		t.Errorf("ports %v hold %d, a connection's own port, not a listening one", got, dialled)
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("ports %v are not sorted, each once", got)
	}
	// The child's alone: its parent's listener is not its.
	NoteAppPID(ctx, child.Process.Pid)
	if kids := AppListenPorts(ctx); !slices.Contains(kids, kid) || slices.Contains(kids, own) {
		t.Errorf("the child's ports %v: want %d, not its parent's %d", kids, kid, own)
	}
	if got := AppListenPorts(context.Background()); got != nil {
		t.Errorf("ports %v on a context with no room for a pid", got)
	}
	// A room made again on a context that has one is the same room.
	if AppPID(WithAppPID(ctx)) != child.Process.Pid {
		t.Error("WithAppPID on a context with a room made a new, empty one")
	}
}

// A child in a network namespace of its own, under a parent that holds no
// socket: its listening port is read from its own namespace's table, where
// its socket is, not from its parent's.
func TestAppListenPortsReadAChildsPortInItsOwnNetworkNamespace(t *testing.T) {
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("no unprivileged network namespace here (%v: %s)", err, out)
	}
	// sh forks unshare (the "; true" keeps sh from exec'ing it), and unshare
	// execs the child in a new user and network namespace, where loopback is
	// down: it listens on every address of that namespace.
	parent := exec.Command("sh", "-c", `unshare -rn "$0" -test.run='^TestListenChildHelper$'; true`, os.Args[0])
	parent.Env = append(os.Environ(), listenChildEnv+"=1", listenChildAddrEnv+"=:0")
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-parent.Process.Pid, syscall.SIGKILL)
		_ = parent.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the child did not say its port: %v", err)
	}
	kid, err := strconv.Atoi(line[:len(line)-1])
	if err != nil {
		t.Fatalf("the child said %q, not a port", line)
	}
	ctx := WithAppPID(context.Background())
	NoteAppPID(ctx, parent.Process.Pid)
	if got := AppListenPorts(ctx); !slices.Contains(got, kid) {
		t.Errorf("ports %v lack %d, the port the child listens on in its own network namespace", got, kid)
	}
}

// ExecuteCommand keeps the pid of a native application command it starts,
// and not a docker command's (that is docker's client, not the application).
func TestExecuteCommandNotesANativeAppsPidOnly(t *testing.T) {
	noCancel := func(*exec.Cmd) func() error { return nil }
	native := WithAppPID(context.Background())
	if res := ExecuteCommand(native, zap.NewNop(), "sh -c 'exit 0'", Native, noCancel, time.Second, nil, nil); res.Err != nil {
		t.Fatalf("the command did not run: %v", res.Err)
	}
	if pid := AppPID(native); pid <= 0 || pid == os.Getpid() {
		t.Errorf("pid %d kept for a native command, want its own", pid)
	}
	docker := WithAppPID(context.Background())
	_ = ExecuteCommand(docker, zap.NewNop(), "sh -c 'exit 0'", DockerRun, noCancel, time.Second, nil, nil)
	if pid := AppPID(docker); pid != 0 {
		t.Errorf("pid %d kept for a docker command, want none", pid)
	}
}
