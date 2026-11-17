// Package neterr classifies socket errors in a way that works on every
// platform keploy runs on.
//
// The portable syscall.E* constants are not portable. On Windows, Go's syscall
// package defines ECONNRESET, ECONNABORTED, ECONNREFUSED and EPIPE as invented
// values in the APPLICATION_ERROR space (syscall/zerrors_windows.go), which
// Winsock never returns: a reset socket surfaces as syscall.Errno(10054)
// (WSAECONNRESET), a refused connect as 10061 (WSAECONNREFUSED). syscall.Errno's
// Is method (syscall/syscall_windows.go) maps only the oserror sentinels —
// permission/exist/notexist/unsupported — so there is no bridge between the two
// sets. errors.Is(err, syscall.ECONNRESET) is therefore *always false* on
// Windows, however the error was produced.
//
// Measured on windows/amd64 (Windows 11, binary built with Go 1.27) against a
// real peer RST:
//
//	err                              = read tcp4 ...: wsarecv: An existing connection was forcibly closed by the remote host.
//	unwrapped errno                  = 10054
//	errors.Is(err, syscall.ECONNRESET) = false
//
// and on linux/amd64 for the same reproducer, errno 104 and true. Predicates
// written against the portable constants alone are silently inert on Windows,
// which turns "retry this dead connection" into a user-visible failure.
//
// Use these helpers instead of comparing against syscall constants directly.
package neterr

import (
	"errors"
	"syscall"
)

func isAny(err error, errnos []syscall.Errno) bool {
	for _, e := range errnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// IsConnReset reports whether err is a connection reset by the peer.
func IsConnReset(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) || isAny(err, connResetErrnos)
}

// IsConnAborted reports whether the connection was aborted locally — the
// host's own stack tore it down, typically mid-handshake or on timeout.
//
// On Windows this is effectively always false, deliberately. The Winsock code
// for it, WSAECONNABORTED, also covers a TCP data-transmission timeout, and
// callers here exclude timeouts on purpose; mapping it would relax that on one
// platform only. See neterr_windows.go. Do not use this predicate as the sole
// gate on anything that must work on Windows.
func IsConnAborted(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNABORTED) || isAny(err, connAbortedErrnos)
}

// IsBrokenPipe reports whether err is a write to a connection that is no
// longer writable because it has been shut down.
//
// The two platforms name different halves of that. POSIX EPIPE is raised on a
// write after the PEER closed. Winsock has no socket EPIPE: the peer-closed
// case arrives as 10054/10053 and is covered by IsConnReset, while
// WSAESHUTDOWN (10058) is a write after the LOCAL side called shutdown. Both
// mean "this connection will not carry another write", which is what every
// caller asks.
func IsBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EPIPE) || isAny(err, brokenPipeErrnos)
}

// IsConnRefused reports whether a connect attempt was actively refused —
// nothing is listening on the target port.
func IsConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || isAny(err, connRefusedErrnos)
}

// IsPeerReset reports whether err is what a socket's call ends in once its
// peer reset the connection: IsConnReset's errors, and on Windows also the
// one Winsock gives a reset that came after the peer's FIN. Measured on
// Windows 11 over loopback: once a peer that reset with no FIN before it (it
// closed with bytes unread, or aborted), a read, and a write and every call
// after it, end in WSAECONNRESET (IsConnReset's); once a peer that sent its
// FIN and then reset the bytes written to it after, they end in
// WSAECONNABORTED. An overlapped call can also complete with
// ERROR_NETNAME_DELETED or ERROR_CONNECTION_ABORTED for a reset or an abort,
// so those are taken too. For a reader that asks how its peer
// ended the connection, not whether to retry: unlike IsConnReset, it takes
// WSAECONNABORTED, which Windows also gives a connection its own stack timed
// out, so there a peer that stopped answering reads as one that reset (Linux
// and macOS give ETIMEDOUT for that, which is not a reset).
func IsPeerReset(err error) bool {
	return IsConnReset(err) || isAny(err, peerResetErrnos)
}

// windowsPeerResetErrnos are the Windows errors (winerror.h's numbers) that
// IsPeerReset takes beyond IsConnReset's on Windows. Named on every OS, so
// their row runs on every OS.
var windowsPeerResetErrnos = []syscall.Errno{
	64,    // ERROR_NETNAME_DELETED: an overlapped call completed by a reset
	1236,  // ERROR_CONNECTION_ABORTED: an overlapped call completed by an abort
	10053, // WSAECONNABORTED: every call once a reset that came after the peer's FIN is in
}
