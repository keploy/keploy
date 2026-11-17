package supervisor

import (
	"errors"
	"syscall"
)

// clientEndErrnoShift: peerEnds holds, in its top byte, from this bit, the
// code of the errno MarkClientEndError kept (keptClientEndCode): 0 while it
// has kept none.
const clientEndErrnoShift = 24

// MarkClientEndError keeps the errno of err, the error the client's socket
// gave after the relay's last read of it and last write to it
// (relay.Config.ClientEndError): a write to the client that failed, or the
// error the socket held as the relay ended (SO_ERROR). ECONNRESET or EPIPE
// there is a client that reset the bytes that reached it after its FIN, which
// PeerEnded does not say: the relay read the client's end as a FIN
// (PeerClosed), and the reset came after. The relay tells it before the
// streams end, so a parser that has read its stream to the end can ask
// ClientEndError. Only the first is kept, and only one this OS can keep: off
// Windows an errno from 1 to 255 (errnoCode); on Windows the socket errors
// that say the connection was reset, kept as ECONNRESET, or written past its
// end, kept as EPIPE (winsockCode). An error with no errno in it (a socket
// keploy closed) is not kept. Nil-safe.
func (s *Session) MarkClientEndError(err error) {
	var errno syscall.Errno
	_ = errors.As(err, &errno)       // where err holds none, errno stays 0
	code := keptClientEndCode(errno) // 0 for one this OS does not keep, which ORs in nothing
	if s == nil {
		return
	}
	for {
		old := s.peerEnds.Load()
		if old>>clientEndErrnoShift != 0 {
			return
		}
		if s.peerEnds.CompareAndSwap(old, old|code<<clientEndErrnoShift) {
			return
		}
	}
}

// ClientEndError reports the errno MarkClientEndError kept, as a
// syscall.Errno: nil while it has kept none. Nil-safe.
func (s *Session) ClientEndError() error {
	if s == nil {
		return nil
	}
	return clientEndErrorOf(s.peerEnds.Load() >> clientEndErrnoShift)
}
