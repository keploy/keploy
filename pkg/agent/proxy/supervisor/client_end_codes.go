package supervisor

import "syscall"

// The codes MarkClientEndError keeps an errno as, in the top byte of
// peerEnds, and the errors ClientEndError gives back for them. Each OS uses
// one pair (keptClientEndCode and clientEndErrorOf, in client_end_windows.go
// and client_end_others.go); both pairs build on every OS, so their rows run
// on every OS too.

// errnoCode, off Windows: the errno itself, from 1 to 255; 0 (keep nothing)
// for 0 and for a larger one.
func errnoCode(errno syscall.Errno) uint32 {
	if errno > 255 {
		return 0
	}
	return uint32(errno)
}

// errnoError gives back the errno errnoCode kept: nil for 0.
func errnoError(code uint32) error {
	if code == 0 {
		return nil
	}
	return syscall.Errno(code)
}

// The Windows errors a socket gives, numbered as winerror.h numbers them
// (their names in golang.org/x/sys/windows; syscall names the first four).
// errnoCode would keep the three under 256 as themselves, errnos no parser
// reads as a reset or a broken pipe, and none of the others; Go's own
// syscall.ECONNRESET and syscall.EPIPE, which the parsers ask for, are
// invented ones on Windows (from syscall.APPLICATION_ERROR up), which the
// socket never gives.
const (
	winErrorNetnameDeleted    = syscall.Errno(64)    // ERROR_NETNAME_DELETED: a reset, as an overlapped read or write completes
	winErrorBrokenPipe        = syscall.Errno(109)   // ERROR_BROKEN_PIPE
	winErrorNoData            = syscall.Errno(232)   // ERROR_NO_DATA: the pipe is being closed
	winErrorConnectionAborted = syscall.Errno(1236)  // ERROR_CONNECTION_ABORTED: an abort, as an overlapped call completes
	winWSAEConnAborted        = syscall.Errno(10053) // WSAECONNABORTED
	winWSAEConnReset          = syscall.Errno(10054) // WSAECONNRESET
	winWSAEShutdown           = syscall.Errno(10058) // WSAESHUTDOWN: a send after the socket's sending side was shut down
)

// The two codes winsockCode keeps.
const (
	winsockReset uint32 = 1 + iota
	winsockPipe
)

// winsockCode, on Windows: winsockReset for a socket error that says the
// connection was reset or aborted (WSAECONNRESET and WSAECONNABORTED, as a
// call returns them; ERROR_NETNAME_DELETED and ERROR_CONNECTION_ABORTED, as
// an overlapped call completes with them; Go's net reads the WSA pair as a
// connection error, net/error_windows.go), winsockPipe for one that says a
// write went past the connection's end (WSAESHUTDOWN, and ERROR_BROKEN_PIPE
// and ERROR_NO_DATA, the pair Go's os/exec takes for Unix's EPIPE,
// os/exec/exec_windows.go), and Go's own syscall.ECONNRESET and
// syscall.EPIPE as those. 0 (keep nothing) for any other: the parsers ask
// only whether the client reset the connection.
func winsockCode(errno syscall.Errno) uint32 {
	switch errno {
	case winWSAEConnReset, winWSAEConnAborted, winErrorNetnameDeleted, winErrorConnectionAborted, syscall.ECONNRESET:
		return winsockReset
	case winWSAEShutdown, winErrorBrokenPipe, winErrorNoData, syscall.EPIPE:
		return winsockPipe
	}
	return 0
}

// winsockError gives back what winsockCode kept as Go names it, so a parser
// asks errors.Is(err, syscall.ECONNRESET) on every OS: nil for 0.
func winsockError(code uint32) error {
	switch code {
	case winsockReset:
		return syscall.ECONNRESET
	case winsockPipe:
		return syscall.EPIPE
	}
	return nil
}
