//go:build linux

package linux

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// Hooks that are not loaded mark nothing and count nothing: each says so,
// never a count of 0.
func TestProxyConnsBeforeLoadAreErrors(t *testing.T) {
	h := &Hooks{}
	if n, err := h.ProxyConnsEstablished(context.Background()); err == nil {
		t.Fatalf("unloaded hooks counted %d connections, want an error", n)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.MarkProxyListener(rc); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("unloaded hooks marked a listener: %v, want a not-loaded error", err)
	}
	if p, err := h.ProxyConnPair(rc); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("unloaded hooks read a pairing %+v: %v, want a not-loaded error", p, err)
	}
	// The application's side, before any mark: unknown, never 0.
	if n, err := h.AppConnsEstablished(context.Background()); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("AppConnsEstablished before a mark = %d, %v; want unknown", n, err)
	}
	if n, err := h.AppConnsSettled(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("AppConnsSettled before a mark = %d, %v; want unknown", n, err)
	}
	if err := h.WaitAppConnsSettled(context.Background(), 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("WaitAppConnsSettled before a mark: %v, want unknown", err)
	}
	if lost := h.CaptureLosses(); lost != nil {
		t.Fatalf("CaptureLosses before a mark: %+v, want none", lost)
	}
}

// An MPTCP socket is refused before the map is touched: the kernel never
// copies its mark into the connections' sockets, so its count would stay 0.
func TestAnMPTCPListenerIsRefused(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_MPTCP)
	if err != nil {
		t.Skipf("no MPTCP in this kernel: %v", err)
	}
	defer unix.Close(fd)
	err = markListenerFD(nil, fd) // nil map: reaching it would panic
	if err == nil || !strings.Contains(err.Error(), "not plain TCP") {
		t.Fatalf("an MPTCP socket: %v, want a not-plain-TCP refusal", err)
	}
}
