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
	if pid, listening := listenerOwner(ip, 1); pid != 0 || !listening {
		t.Fatalf("an unreported port must be left to the mock path, got %d %v", pid, listening)
	}
	listeners.Note(uint32(os.Getpid()), 2)
	if pid, listening := listenerOwner(ip, 2); pid != os.Getpid() || !listening {
		t.Fatalf("owner = %d %v", pid, listening)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	listeners.Note(uint32(cmd.Process.Pid), 3)
	if pid, listening := listenerOwner(ip, 3); pid != 0 || listening {
		t.Fatalf("a port whose owner exited is not listening, got %d %v", pid, listening)
	}
	if !descends(os.Getpid(), os.Getppid()) || descends(os.Getppid(), os.Getpid()) {
		t.Fatal("descends must follow parents only")
	}
}
