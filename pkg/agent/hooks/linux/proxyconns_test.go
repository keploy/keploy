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

// A drop the kernel counts ends every wait on the application's connections
// at once, as unknown: the reader checks the count after each record, so a
// wait parked on a number whose resolution was dropped does not stay parked
// until something else reads the counts. A count that cannot be read is
// unknown too; none dropped leaves the count as it was.
func TestAnAppConnDropEndsTheWaits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dropped uint64
		err     error
		ok      bool
	}{
		{name: "none dropped", ok: true},
		{name: "one dropped", dropped: 1},
		{name: "the count unreadable", err: errors.New("read: bad file descriptor")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hooks{}
			h.proxyMarked.Store(true)
			h.app.settled.Add(1)
			parked := make(chan error, 1)
			go func() { parked <- h.WaitAppConnsSettled(context.Background(), 2) }()
			ok := h.appConnDropsNone(func() (uint64, error) { return tc.dropped, tc.err })
			if ok != tc.ok {
				t.Fatalf("appConnDropsNone = %v, want %v", ok, tc.ok)
			}
			if tc.ok {
				if n, err := h.AppConnsSettled(); err != nil || n != 1 {
					t.Fatalf("AppConnsSettled = %d, %v; want 1, still known", n, err)
				}
				h.app.settled.Add(2) // its resolution, which ends the wait
				if err := <-parked; err != nil {
					t.Fatalf("the wait for 2: %v, want it settled", err)
				}
				return
			}
			if err := <-parked; !errors.Is(err, models.ErrProxyConnsUnsupported) {
				t.Fatalf("the wait parked on 2 ended with %v, want unknown", err)
			}
			if _, err := h.AppConnsSettled(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
				t.Fatalf("AppConnsSettled after the drop: %v, want unknown", err)
			}
		})
	}
}
