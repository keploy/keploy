//go:build windows

package neterr

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// The Winsock codes net actually surfaces on Windows. Go's own syscall.E*
// constants live in the APPLICATION_ERROR space and never match these — see
// the package comment.
var (
	connResetErrnos = []syscall.Errno{
		windows.WSAECONNRESET, // 10054 - peer sent RST
		windows.WSAENETRESET,  // 10052 - connection broken, keep-alive detected failure
	}

	// Deliberately empty. The obvious candidate is WSAECONNABORTED (10053),
	// and it is deliberately NOT here: Windows returns it both for a genuine
	// abort and for a TCP data-transmission timeout, whereas every caller of
	// this package excludes timeouts on purpose — a slow upstream is not a
	// dead connection, and re-sending would double-charge an upstream that
	// may already have processed the request. Mapping an ambiguous code would
	// quietly relax that contract on one platform only. Under-detecting costs
	// a 502, which is exactly the behaviour today; over-detecting costs a
	// duplicate side effect.
	connAbortedErrnos []syscall.Errno

	// Windows has no EPIPE for sockets. A send after the local side has shut
	// the socket down is WSAESHUTDOWN. WSAECONNABORTED is excluded here for
	// the reason above.
	brokenPipeErrnos = []syscall.Errno{
		windows.WSAESHUTDOWN, // 10058
	}

	connRefusedErrnos = []syscall.Errno{
		windows.WSAECONNREFUSED, // 10061 - "connectex: ... actively refused it"
	}
)
