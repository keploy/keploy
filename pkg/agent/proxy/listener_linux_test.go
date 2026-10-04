//go:build linux

package proxy

import (
	"net"
	"os"
	"testing"
)

func TestListenerOwnerFindsThisProcessListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	pid, ok := listenerOwner(net.ParseIP("127.0.0.1"), port)
	if !ok || pid != os.Getpid() {
		t.Fatalf("owner of :%d = %d %v, want %d", port, pid, ok, os.Getpid())
	}
	if !descends(os.Getpid(), os.Getppid()) || descends(os.Getppid(), os.Getpid()) {
		t.Fatal("descends must follow parents only")
	}
	l.Close()
	if _, ok := listenerOwner(net.ParseIP("127.0.0.1"), port); ok {
		t.Fatal("a closed port has no owner")
	}
}

func TestListenTableAndOwnerOfFindANewListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	for inode, p := range listenTable() {
		if p != port {
			continue
		}
		if pid, ok := treeOwnerOf(inode, os.Getppid()); ok && pid == os.Getpid() {
			return
		}
	}
	t.Fatalf("no listener of this process on :%d in the table", port)
}
