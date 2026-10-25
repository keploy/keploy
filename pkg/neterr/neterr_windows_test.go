//go:build windows

package neterr

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// The real-socket tests can only provoke WSAECONNRESET and WSAECONNREFUSED.
// The rest of the table is pinned here: without this, deleting an entry from
// neterr_windows.go compiles and every test still passes.
func TestWindowsErrnoTable(t *testing.T) {
	cases := []struct {
		name                                   string
		errno                                  syscall.Errno
		reset, aborted, brokenPipe, connRefuse bool
		vanished                               bool
	}{
		{"WSAECONNRESET", windows.WSAECONNRESET, true, false, false, false, false},
		{"WSAENETRESET", windows.WSAENETRESET, true, false, false, false, false},
		{"WSAESHUTDOWN", windows.WSAESHUTDOWN, false, false, true, false, false},
		{"WSAECONNREFUSED", windows.WSAECONNREFUSED, false, false, false, true, false},

		// Ambiguous on Windows (genuine abort OR data-transmission timeout),
		// so it maps to none of the first four. See the comment in
		// neterr_windows.go: their callers exclude timeouts deliberately,
		// and this is the only code that would smuggle one back in. Either
		// meaning leaves the connection dead, so it is a vanished peer.
		{"WSAECONNABORTED", windows.WSAECONNABORTED, false, false, false, false, true},

		// A connection Winsock gave up on is a vanished peer, and nothing
		// else: not a reset, an abort, a broken pipe or a refusal.
		{"WSAETIMEDOUT", windows.WSAETIMEDOUT, false, false, false, false, true},
		{"WSAEHOSTUNREACH", windows.WSAEHOSTUNREACH, false, false, false, false, true},
		{"WSAENETUNREACH", windows.WSAENETUNREACH, false, false, false, false, true},
		{"WSAEHOSTDOWN", windows.WSAEHOSTDOWN, false, false, false, false, true},
	}

	for _, c := range cases {
		// Wrapped the way net actually delivers it, so this exercises the
		// same errors.Is unwrap chain as production.
		err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", c.errno)}
		if got := IsConnReset(err); got != c.reset {
			t.Errorf("IsConnReset(%s/%d) = %v, want %v", c.name, uintptr(c.errno), got, c.reset)
		}
		if got := IsConnAborted(err); got != c.aborted {
			t.Errorf("IsConnAborted(%s/%d) = %v, want %v", c.name, uintptr(c.errno), got, c.aborted)
		}
		if got := IsBrokenPipe(err); got != c.brokenPipe {
			t.Errorf("IsBrokenPipe(%s/%d) = %v, want %v", c.name, uintptr(c.errno), got, c.brokenPipe)
		}
		if got := IsConnRefused(err); got != c.connRefuse {
			t.Errorf("IsConnRefused(%s/%d) = %v, want %v", c.name, uintptr(c.errno), got, c.connRefuse)
		}
		if got := IsPeerVanished(err); got != c.vanished {
			t.Errorf("IsPeerVanished(%s/%d) = %v, want %v", c.name, uintptr(c.errno), got, c.vanished)
		}
	}
}

// Pins the premise the whole package rests on: the portable constant does not
// match what the platform produces. If Go ever bridges these, this fails and
// the package can be reconsidered.
func TestPortableConstantDoesNotMatchWinsock(t *testing.T) {
	err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", windows.WSAECONNRESET)}
	if errors.Is(err, syscall.ECONNRESET) {
		t.Fatal("errors.Is(WSAECONNRESET, syscall.ECONNRESET) is now true on Windows; " +
			"Go bridged the APPLICATION_ERROR constants to the Winsock codes and this package's premise has changed")
	}
	if !IsConnReset(err) {
		t.Fatal("IsConnReset failed on a WSAECONNRESET that net would produce")
	}
}
