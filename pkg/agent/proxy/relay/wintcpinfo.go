package relay

import (
	"encoding/binary"
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
// retransmitted a byte or had a timeout episode says nothing.

// The TCP_INFO_v0 fields read, at their offsets in mstcpip.h's struct, and the
// TCPSTATE values looked for.
const (
	wtiState           = 0  // TCPSTATE State
	wtiBytesInFlight   = 28 // ULONG BytesInFlight
	wtiBytesOut        = 48 // ULONG64 BytesOut
	wtiBytesRetrans    = 68 // ULONG BytesRetrans
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
	state    uint32
	inFlight uint32 // bytes sent and not acknowledged
	out      uint64 // bytes sent
	retrans  uint32 // bytes sent again
	timeouts uint32 // retransmission timeout episodes
}

// readWinTCPInfo reads the fields of a TCP_INFO_v0 answer b; false when b is
// shorter than the last of them ends.
func readWinTCPInfo(b []byte) (winTCPInfo, bool) {
	if len(b) < wtcpInfoNeeded {
		return winTCPInfo{}, false
	}
	return winTCPInfo{
		state:    binary.LittleEndian.Uint32(b[wtiState:]),
		inFlight: binary.LittleEndian.Uint32(b[wtiBytesInFlight:]),
		out:      binary.LittleEndian.Uint64(b[wtiBytesOut:]),
		retrans:  binary.LittleEndian.Uint32(b[wtiBytesRetrans:]),
		timeouts: binary.LittleEndian.Uint32(b[wtiTimeoutEpisodes:]),
	}, true
}

// counted reports whether the counts say what was sent and acknowledged:
// nothing was sent twice and no timeout ran. With a retransmission it is not
// documented whether BytesOut counts a byte again, and a connection a timeout
// closed must not be read as reset.
func (i winTCPInfo) counted() bool {
	return i.retrans == 0 && i.timeouts == 0
}

// acked is how many of the bytes sent the client's stack has acknowledged:
// BytesOut less BytesInFlight; false when the counts cannot say. Once the
// proxy has sent its FIN (finSent), one byte in flight is taken for it, since
// TCP_INFO_v0 does not say whether BytesInFlight counts the FIN's sequence
// number: where it does not, that counts one byte acknowledged that is not,
// which can only have a chunk teed as before, never refuse one the client
// took (chunkVerdict).
func (i winTCPInfo) acked(finSent bool) (uint64, bool) {
	if !i.counted() {
		return 0, false
	}
	inFlight := uint64(i.inFlight)
	if finSent && inFlight > 0 {
		inFlight--
	}
	if i.out < inFlight {
		return 0, false
	}
	return i.out - inFlight, true
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

// sent is what written says on Windows: the bytes the socket has sent. Windows
// has no count of what its send buffer still holds (no SIOCOUTQ, and TCP_INFO
// counts a byte once it is sent), so those bytes are left out. A chunk set
// aside is then judged from no more bytes than were written before it, which
// can only take it for one the client took (teed, as before), never refuse
// one it took; and the relay's wait for the client's answer to every byte
// (clientAnswered) can end before the last ones are sent, the socket then read
// as it is.
func (i winTCPInfo) sent() (uint64, bool) {
	return i.out, i.counted()
}

// resetCode is the error Linux's socket keeps for the reset that closed this
// connection, where Windows keeps none (see above): 0 when it is not closed,
// the counts cannot say, or it may have closed cleanly (none in flight after
// the proxy's FIN: the client acknowledged that FIN, or reset with every byte
// acknowledged). finRead: the relay read the client's FIN; finSent: the proxy
// sent its own TCP FIN. The code is the one Linux's tcp_reset keeps: EPIPE in
// CLOSE_WAIT (the client's FIN in, the proxy's not sent), ECONNRESET in every
// other state.
func (i winTCPInfo) resetCode(finRead, finSent bool) syscall.Errno {
	switch {
	case i.state != wtcpClosed, !i.counted():
		return 0
	case i.inFlight == 0 && finSent:
		return 0
	case finRead && !finSent:
		return syscall.EPIPE
	}
	return syscall.ECONNRESET
}
