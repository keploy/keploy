//go:build linux

package utils

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func ownName(t *testing.T, pid int) string {
	t.Helper()
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("pid %d (%s)", pid, strings.TrimSpace(string(comm)))
}

// PortHolder names the process that holds a port, by the socket's inode in
// the kernel's TCP and UDP tables (IPv4 and IPv6), and no process for a port
// none of its sockets is bound to.
func TestPortHolderNamesTheProcessOnThePort(t *testing.T) {
	me := ownName(t, os.Getpid())
	tcp4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp4.Close() }()
	tcp6, err := net.Listen("tcp", ":0") // dual-stack: the tcp6 table
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp6.Close() }()
	udp4, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp4.Close() }()
	udp6, err := net.ListenPacket("udp", ":0") // dual-stack: the udp6 table
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp6.Close() }()
	for name, port := range map[string]int{
		"tcp": tcp4.Addr().(*net.TCPAddr).Port, "tcp6": tcp6.Addr().(*net.TCPAddr).Port,
		"udp": udp4.LocalAddr().(*net.UDPAddr).Port, "udp6": udp6.LocalAddr().(*net.UDPAddr).Port,
	} {
		if got := PortHolder(uint32(port)); got != me {
			t.Fatalf("a %s socket on port %d: holder %q, want %q", name, port, got, me)
		}
		if got := PortTaken(uint32(port)); got != fmt.Sprintf("port %d is taken by %s", port, me) {
			t.Fatalf("PortTaken(%d) = %q", port, got)
		}
	}
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	if got := PortHolder(uint32(port)); got != "" {
		t.Fatalf("port %d, which no socket holds: holder %q", port, got)
	}
}

// A socket that many processes hold (inherited): the first three by pid
// are named, then how many more.
func TestPortHolderNamesThreeHoldersThenCounts(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	pids := []int{os.Getpid()}
	for i := 0; i < 4; i++ {
		cmd := exec.Command("sleep", "60")
		cmd.ExtraFiles = []*os.File{f}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = cmd.Wait() })
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	var want []string
	for _, pid := range pids[:3] {
		want = append(want, ownName(t, pid))
	}
	want = append(want, "2 more")
	if got := PortHolder(uint32(ln.Addr().(*net.TCPAddr).Port)); got != strings.Join(want, ", ") {
		t.Fatalf("five holders: %q, want %q", got, strings.Join(want, ", "))
	}
}
