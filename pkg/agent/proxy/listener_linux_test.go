//go:build linux

package proxy

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/internal/testnet"
)

func TestListenerOwnerFindsThisProcessListening(t *testing.T) {
	// The port stays held once the listener closes (testnet.SharedPort),
	// for the check that a closed port has no owner.
	l := testnet.ListenShared(t, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t))))
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	pid, ok := listenerOwner(net.ParseIP("127.0.0.1"), port)
	if !ok || pid != os.Getpid() {
		t.Fatalf("owner of :%d = %d %v, want %d", port, pid, ok, os.Getpid())
	}
	if !descends(os.Getpid(), os.Getppid()) || descends(os.Getppid(), os.Getpid()) {
		t.Fatal("descends must follow parents only")
	}
	l.Close()
	if pid, listening := listenerOwner(net.ParseIP("127.0.0.1"), port); listening || pid != 0 {
		t.Fatal("a closed port has no owner")
	}
}

// listenLocal is a listener at 127.0.0.1 on a port of the kernel's choosing,
// closed when the test ends, and its port.
func listenLocal(t *testing.T) (*net.TCPListener, uint32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.(*net.TCPListener), uint32(l.Addr().(*net.TCPAddr).Port)
}

// A connection to a listener reads the one fd its holder was found at, not
// every process's fd table: one walk per listener however many connections
// it takes, and one more for a second listener.
func TestListenerOwnerWalksTheProcessesOncePerListener(t *testing.T) {
	c := newOwnerCache()
	ip := net.ParseIP("127.0.0.1")
	_, first := listenLocal(t)
	_, second := listenLocal(t)
	for _, step := range []struct {
		port  uint32
		calls int
		walks int
	}{{first, 1, 1}, {first, 49, 1}, {second, 50, 2}, {first, 50, 2}} {
		for i := 0; i < step.calls; i++ {
			if pid, ok := c.owner(ip, step.port); !ok || pid != os.Getpid() {
				t.Fatalf("owner of :%d = %d %v, want %d", step.port, pid, ok, os.Getpid())
			}
		}
		if c.walks != step.walks {
			t.Fatalf("after %d more calls for :%d, %d walks of /proc, want %d", step.calls, step.port, c.walks, step.walks)
		}
	}
}

// A remembered holder that no longer holds the socket is not the answer:
// the next connection walks again and finds the process that holds it now.
func TestListenerOwnerWalksAgainOnceItsHolderLetsTheSocketGo(t *testing.T) {
	c := newOwnerCache()
	ip := net.ParseIP("127.0.0.1")
	l, port := listenLocal(t)
	if pid, ok := c.owner(ip, port); !ok || pid != os.Getpid() {
		t.Fatalf("owner of :%d = %d %v, want this process %d", port, pid, ok, os.Getpid())
	}
	sock, err := l.File()
	if err != nil {
		t.Fatal(err)
	}
	// cat holds the listening socket at its fd 3 and waits on stdin until
	// the test ends; this process then lets its own copies go.
	stdin, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cat")
	cmd.Stdin, cmd.ExtraFiles = stdin, []*os.File{sock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = hold.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	_ = stdin.Close()
	_ = sock.Close()
	_ = l.Close()
	for i := 0; i < 3; i++ {
		if pid, ok := c.owner(ip, port); !ok || pid != cmd.Process.Pid {
			t.Fatalf("owner of :%d once this process let it go = %d %v, want the child %d", port, pid, ok, cmd.Process.Pid)
		}
	}
	if c.walks != 2 {
		t.Fatalf("%d walks of /proc, want 2: one for this process, one once it let the socket go", c.walks)
	}
}

// An ownerCache remembers at most ownerCacheMax listeners: one more makes it
// forget the others.
func TestOwnerCacheForgetsItsListenersPastItsBound(t *testing.T) {
	ip := net.ParseIP("127.0.0.1")
	_, port := listenLocal(t)
	for _, step := range []struct{ before, after int }{{ownerCacheMax - 1, ownerCacheMax}, {ownerCacheMax, 1}} {
		c := newOwnerCache()
		for i := 0; i < step.before; i++ {
			c.held["gone-"+strconv.Itoa(i)] = heldAt{}
		}
		if pid, ok := c.owner(ip, port); !ok || pid != os.Getpid() {
			t.Fatalf("owner of :%d = %d %v, want %d", port, pid, ok, os.Getpid())
		}
		if len(c.held) != step.after {
			t.Fatalf("with %d listeners remembered, one more leaves %d, want %d", step.before, len(c.held), step.after)
		}
	}
}

// A listener's sockets name one entry whatever order a map hands them in.
func TestListenKeyIsTheSocketsInOrder(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := listenKey(map[string]bool{"7": true, "13": true, "9": true}); got != "13,7,9" {
			t.Fatalf("listenKey = %q, want 13,7,9", got)
		}
	}
}
