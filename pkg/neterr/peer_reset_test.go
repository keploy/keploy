package neterr

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
)

// winsockOp wraps errno the way net delivers a Windows socket error.
func winsockOp(errno syscall.Errno) error {
	return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", errno)}
}

// Each Windows error IsPeerReset takes beyond IsConnReset's is in the table,
// and nothing else is: the table builds on every OS, so this runs on Linux
// CI too.
func TestWindowsPeerResetErrnosAreTheResetConnectionsOwn(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		in    bool
	}{
		{"ERROR_NETNAME_DELETED", 64, true},
		{"ERROR_CONNECTION_ABORTED", 1236, true},
		{"WSAECONNABORTED", 10053, true},
		{"WSAETIMEDOUT", 10060, false},
		{"WSAESHUTDOWN", 10058, false},
		{"WSAECONNREFUSED", 10061, false},
		{"ERROR_BROKEN_PIPE", 109, false},
	}
	for _, c := range cases {
		if got := isAny(winsockOp(c.errno), windowsPeerResetErrnos); got != c.in {
			t.Errorf("%s (%d) in windowsPeerResetErrnos = %v, want %v", c.name, uintptr(c.errno), got, c.in)
		}
	}
}

// IsPeerReset takes IsConnReset's resets on every OS, the table's errors on
// Windows alone, and no other error.
func TestIsPeerResetTakesResetsAsEachOSGivesThem(t *testing.T) {
	onWindows := runtime.GOOS == "windows"
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"EOF", io.EOF, false},
		{"an unrelated error", errors.New("boom"), false},
		{"syscall.ECONNRESET", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{"syscall.EPIPE", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, false},
		{"WSAECONNRESET", winsockOp(10054), onWindows},
		{"ERROR_NETNAME_DELETED", winsockOp(64), onWindows},
		{"ERROR_CONNECTION_ABORTED", winsockOp(1236), onWindows},
		{"WSAECONNABORTED", winsockOp(10053), onWindows},
		{"WSAETIMEDOUT", winsockOp(10060), false},
	}
	for _, c := range cases {
		if got := IsPeerReset(c.err); got != c.want {
			t.Errorf("IsPeerReset(%s) = %v, want %v on %s", c.name, got, c.want, runtime.GOOS)
		}
	}
	// IsConnReset, which callers that retry ask, is not widened.
	if IsConnReset(winsockOp(10053)) {
		t.Error("IsConnReset(WSAECONNABORTED) = true, want false: a retrying caller excludes timeouts")
	}
}
