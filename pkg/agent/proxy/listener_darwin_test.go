//go:build darwin

package proxy

import (
	"net"
	"os"
	"os/exec"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/listeners"
)

func TestListenerOwnerOnMacUsesReportedListens(t *testing.T) {
	ip := net.ParseIP("127.0.0.1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	if pid, listening := listenerOwner(ip, port); pid != 0 || !listening {
		t.Fatalf("a live port nobody reported must be left to the mock path, got %d %v", pid, listening)
	}
	listeners.Note(uint32(os.Getpid()), uint16(port))
	if pid, listening := listenerOwner(ip, port); pid != os.Getpid() || !listening {
		t.Fatalf("owner = %d %v", pid, listening)
	}
	_ = ln.Close()
	if pid, listening := listenerOwner(ip, port); pid != 0 || listening {
		t.Fatalf("a closed port is not listening whatever the table says, got %d %v", pid, listening)
	}
	if _, ok := listeners.Owner(uint16(port)); ok {
		t.Fatal("the stale owner of a closed port must be forgotten")
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	listeners.Note(uint32(cmd.Process.Pid), uint16(ln2.Addr().(*net.TCPAddr).Port))
	if pid, listening := listenerOwner(ip, uint32(ln2.Addr().(*net.TCPAddr).Port)); pid != 0 || !listening {
		t.Fatalf("a live port whose reported owner exited is someone else's, got %d %v", pid, listening)
	}
	if _, ok := listeners.Owner(uint16(ln2.Addr().(*net.TCPAddr).Port)); ok {
		t.Fatal("the owner that exited must be forgotten")
	}
	if !descends(os.Getpid(), os.Getppid()) || descends(os.Getppid(), os.Getpid()) {
		t.Fatal("descends must follow parents only")
	}
}
