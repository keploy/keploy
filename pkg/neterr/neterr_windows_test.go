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
	}{
		{"WSAECONNRESET", windows.WSAECONNRESET, true, false, false, false},
		{"WSAENETRESET", windows.WSAENETRESET, true, false, false, false},
		{"WSAESHUTDOWN", windows.WSAESHUTDOWN, false, false, true, false},
		{"WSAECONNREFUSED", windows.WSAECONNREFUSED, false, false, false, true},

		// Ambiguous on Windows (genuine abort OR data-transmission timeout),
		// so it maps to nothing. See the comment in neterr_windows.go: every
		// caller excludes timeouts deliberately, and this is the only code
		// that would smuggle one back in.
		{"WSAECONNABORTED", windows.WSAECONNABORTED, false, false, false, false},

		// Timeouts and unrelated Winsock failures must classify as nothing.
		{"WSAETIMEDOUT", windows.WSAETIMEDOUT, false, false, false, false},
		{"WSAEHOSTUNREACH", windows.WSAEHOSTUNREACH, false, false, false, false},
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
