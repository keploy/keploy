//go:build linux

package proxy

import (
	"net"
	"os"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
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
