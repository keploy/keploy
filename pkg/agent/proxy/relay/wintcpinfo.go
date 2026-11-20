package relay

import (
	"encoding/binary"
	"errors"
	"syscall"
)

// What a Windows TCP socket says of its connection, from its TCP_INFO_v0
// (WSAIoctl SIO_TCP_INFO, version 0: Windows 10 1703 and later), which
// client_socket_windows.go asks for. The reading and the judging are here,
// built on every OS, so their rows run wherever the package's tests run; only
// the call is Windows'.
//
// Windows keeps no error on a socket for a reset: SO_ERROR holds 0 after one
// in every state (measured on Windows 11, loopback). What Linux's SO_ERROR
// says is told from the state and the counts instead. A connection that is
// CLOSED with a byte in flight was reset: the client's acknowledgement of the
// proxy's FIN, which is how LAST_ACK closes cleanly, acknowledges every byte
// before it. A connection CLOSED with none in flight while the proxy sent no
// FIN was reset too: nothing else closes ESTABLISHED or CLOSE_WAIT. A timeout
// closes one as well, and keeps no error either, so a socket that
// retransmitted a byte or had a timeout episode tells no reset (unresent).

// The TCP_INFO_v0 fields read, at their offsets in mstcpip.h's struct, and the
// TCPSTATE values looked for.
const (
	wtiState           = 0  // TCPSTATE State
	wtiBytesInFlight   = 28 // ULONG BytesInFlight
	wtiBytesOut        = 48 // ULONG64 BytesOut
	wtiBytesRetrans    = 68 // ULONG BytesRetrans
	wtiFastRetrans     = 72 // ULONG FastRetrans
	wtiTimeoutEpisodes = 80 // ULONG TimeoutEpisodes
	// wtcpInfoNeeded is the end of the last field read: a shorter answer
	// (an older or changed struct) is not read.
	wtcpInfoNeeded = wtiTimeoutEpisodes + 4
	// wtcpInfoSize is sizeof(TCP_INFO_v0), the buffer asked with.
	wtcpInfoSize = 88

	wtcpClosed    = 0
	wtcpCloseWait = 7
	wtcpLastAck   = 9
	wtcpTimeWait  = 10
)

// winTCPInfo is what is read of a TCP_INFO_v0.
type winTCPInfo struct {
	state       uint32
	inFlight    uint32 // bytes sent and not acknowledged
	out         uint64 // bytes sent, a byte sent again counted again
	retrans     uint32 // bytes sent again
	fastRetrans uint32 // fast retransmits
	timeouts    uint32 // retransmission timeout episodes
}

// readWinTCPInfo reads the fields of a TCP_INFO_v0 answer b; false when b is
// shorter than the last of them ends.
func readWinTCPInfo(b []byte) (winTCPInfo, bool) {
	if len(b) < wtcpInfoNeeded {
		return winTCPInfo{}, false
	}
	return winTCPInfo{
		state:       binary.LittleEndian.Uint32(b[wtiState:]),
		inFlight:    binary.LittleEndian.Uint32(b[wtiBytesInFlight:]),
		out:         binary.LittleEndian.Uint64(b[wtiBytesOut:]),
		retrans:     binary.LittleEndian.Uint32(b[wtiBytesRetrans:]),
		fastRetrans: binary.LittleEndian.Uint32(b[wtiFastRetrans:]),
		timeouts:    binary.LittleEndian.Uint32(b[wtiTimeoutEpisodes:]),
	}, true
}

// counted reports whether the counts say what was sent and acknowledged: no
// fast retransmit and no retransmission timeout ran on the connection. Inside
// that fence the bytes resent are a closed receive window's probes: the
// first probe of a closed window is a new byte, and each later one sends that
// byte again, which adds one to BytesOut and one to BytesRetrans (measured on
// Windows 11, loopback, windows closed up to 8 s: the first probe 0.3-0.7 s
// in, its resends at about 1-1.5, 2.5 and 5 s, FastRetrans and
// TimeoutEpisodes 0 throughout, and BytesOut less BytesRetrans and
// BytesInFlight equal to the bytes the client had acknowledged, before its
// reset and after). So the bytes sent once are BytesOut less BytesRetrans
// (sentOnce). A tail-loss probe's resend counts the same way (measured from
// Windows 11 to a Linux host over the LAN, 40 connections whose last data
// segment was dropped once, 20 with it alone in flight and 20 after 2,000
// bytes: each sent it again once, 205-232 ms in, BytesOut and BytesRetrans
// each counting the resend, FastRetrans and TimeoutEpisodes 0, so BytesOut
// less BytesRetrans was the bytes written). A loss recovery that goes
// further, a fast retransmit or a timeout, trips the fence, and from then on
// the fallback holds: the counts say nothing, and what the destination sends
// a closed client is teed as it is written, as before.
func (i winTCPInfo) counted() bool {
	return i.fastRetrans == 0 && i.timeouts == 0
}

// unresent reports whether nothing was sent again and no timeout ran: the
// only answers resetCode reads a reset from. A connection that Windows' stack
// gave up on closes with a byte in flight and keeps no error, as one reset
// does, and the stack resends before it gives up.
func (i winTCPInfo) unresent() bool {
	return i.retrans == 0 && i.timeouts == 0
}

// sentOnce is the bytes the socket has sent, each counted once: BytesOut less
// BytesRetrans (counted); false when the counts cannot say.
func (i winTCPInfo) sentOnce() (uint64, bool) {
	if !i.counted() || i.out < uint64(i.retrans) {
		return 0, false
	}
	return i.out - uint64(i.retrans), true
}

// acked is how many of the bytes sent the client's stack has acknowledged:
// those sent once less BytesInFlight; false when the counts cannot say. Once
// the proxy has sent its FIN (finSent), one byte in flight is taken for it,
// since TCP_INFO_v0 does not say whether BytesInFlight counts the FIN's
// sequence number: where it does not, that counts one byte acknowledged that
// is not, which can only have a chunk teed as before, never refuse one the
// client took (chunkVerdict).
func (i winTCPInfo) acked(finSent bool) (uint64, bool) {
	sent, ok := i.sentOnce()
	if !ok {
		return 0, false
	}
	inFlight := uint64(i.inFlight)
	if finSent && inFlight > 0 {
		inFlight--
	}
	if sent < inFlight {
		return 0, false
	}
	return sent - inFlight, true
}

// tookAll reports whether the client's stack has acknowledged every one of
// the written bytes written to the socket (the count from its accept, as
// resetClientOnClose reads it): each was sent (sentOnce) and none is in
// flight, so its send buffer holds nothing. known is false when the counts
// cannot say (counted), and when they count more bytes sent than were
// written, so a write went around the count (checkCount). A FIN the proxy
// sent can count in BytesInFlight until the client acknowledges it (acked),
// which only has the reset wait for that.
func (i winTCPInfo) tookAll(written uint64) (all, known bool) {
	sent, ok := i.sentOnce()
	if !ok || sent > written {
		return false, false
	}
	return sent == written && i.inFlight == 0, true
}

// look is what the socket says of its client, as Linux's TCP_INFO says it
// (client_took_linux.go): TIME_WAIT is closed as well (the client's FIN came
// after the proxy's, and both are acknowledged).
func (i winTCPInfo) look(finSent bool) clientLook {
	acked, ok := i.acked(finSent)
	if !ok {
		return clientLook{said: clientUnknown}
	}
	switch i.state {
	case wtcpClosed, wtcpTimeWait:
		return clientLook{said: clientClosed, acked: acked}
	case wtcpCloseWait, wtcpLastAck:
		return clientLook{said: clientFinned, acked: acked}
	}
	return clientLook{said: clientOpen, acked: acked}
}

// sent is what written says on a Windows socket with no count from its accept
// (client_count_accept.go): the bytes the socket has sent, each once
// (sentOnce). Windows has no
// count of what its send buffer still holds (no SIOCOUTQ, and TCP_INFO
// counts a byte once it is sent), so those bytes are left out. A chunk set
// aside is then judged from no more bytes than were written before it, which
// can only take it for one the client took (teed, as before), never refuse
// one it took; and the relay's wait for the client's answer to every byte
// (clientAnswered) can end before the last ones are sent, the socket then read
// as it is.
func (i winTCPInfo) sent() (uint64, bool) {
	return i.sentOnce()
}

// resetCode is the error Linux's socket keeps for the reset that closed this
// connection, where Windows keeps none (see above): 0 when it is not closed,
// a byte was sent again or a timeout ran (unresent), or it may have closed
// cleanly (none in flight after
// the proxy's FIN: the client acknowledged that FIN, or reset with every byte
// acknowledged). finRead: the relay read the client's FIN; finSent: the proxy
// sent its own TCP FIN. The code is the one Linux's tcp_reset keeps: EPIPE in
// CLOSE_WAIT (the client's FIN in, the proxy's not sent), ECONNRESET in every
// other state.
func (i winTCPInfo) resetCode(finRead, finSent bool) syscall.Errno {
	switch {
	case i.state != wtcpClosed, !i.unresent():
		return 0
	case i.inFlight == 0 && finSent:
		return 0
	case finRead && !finSent:
		return syscall.EPIPE
	}
	return syscall.ECONNRESET
}

// wsaeConnAborted is Winsock's WSAECONNABORTED (winerror.h), named on every
// OS so the rows that judge it run on every OS.
const wsaeConnAborted syscall.Errno = 10053

// resetAfterFIN reports whether a read of the client's socket that failed
// with readErr, on a socket that now says i, returned a reset that came after
// the client's FIN, where no look found that FIN in before (Windows: its
// TCP_INFO_v0 has no rcv_nxt to count the FIN by, as macOS's has).
//
// Windows' read tells the two apart by its code. Measured on Windows 11 over
// loopback, 200 of 200 runs each: a client that sent its FIN and then reset
// the bytes the proxy wrote it after (it had closed its socket) makes the
// proxy's read fail with WSAECONNABORTED, and every call after it; a client
// that reset with no FIN before (it closed with bytes unread, or aborted)
// makes it fail with WSAECONNRESET, also when the proxy writes once the reset
// is in. Winsock also gives WSAECONNABORTED to a connection its own stack
// timed out, which counts a timeout episode, and the measured case closed the
// connection with the proxy's bytes in flight, before any FIN of the proxy's.
// So only that case is taken: WSAECONNABORTED, the connection CLOSED with a
// byte in flight, no timeout episode in its life and no FIN of the proxy's
// (finSent). Anything else is read as a reset, as before, so a reset with no
// FIN before it is never read as one.
func (i winTCPInfo) resetAfterFIN(readErr error, finSent bool) bool {
	return errors.Is(readErr, wsaeConnAborted) && !finSent &&
		i.state == wtcpClosed && i.inFlight > 0 && i.timeouts == 0
}
