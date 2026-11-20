//go:build windows

package util

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A socket's TCP state is read only from a SIO_TCP_INFO answer that holds
// it: a call that fails, whatever it says it answered, or an answer shorter
// than State's four bytes, gives no state.
func TestTCPStateOnWindows(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		said      uint32
		err       error
		wantState uint32
		wantOK    bool
	}{
		{name: "a whole answer", said: tcpInfoV0Size, wantState: wtcpCloseWait, wantOK: true},
		{name: "State and nothing after it", said: 4, wantState: wtcpCloseWait, wantOK: true},
		{name: "an answer one byte short of State", said: 3},
		{name: "an empty answer", said: 0},
		{name: "a failed call that says it answered", said: tcpInfoV0Size, err: windows.WSAEINVAL},
	} {
		t.Run(row.name, func(t *testing.T) {
			ioctl := func(_ windows.Handle, iocc uint32, inbuf *byte, cbif uint32, outbuf *byte, cbob uint32,
				cbbr *uint32, _ *windows.Overlapped, _ uintptr) error {
				if iocc != sioTCPInfo || cbob != tcpInfoV0Size || cbif != 4 || *(*uint32)(unsafe.Pointer(inbuf)) != 0 {
					t.Fatalf("asked ioctl %#x, version in %d bytes, into %d bytes; want SIO_TCP_INFO, "+
						"version 0 in 4 bytes, into %d", iocc, cbif, cbob, tcpInfoV0Size)
				}
				out := unsafe.Slice(outbuf, cbob)
				binary.LittleEndian.PutUint32(out[0:4], wtcpCloseWait)
				*cbbr = row.said
				return row.err
			}
			state, ok := askTCPState(ioctl, 0)
			if state != row.wantState || ok != row.wantOK {
				t.Fatalf("state %d, %v; want %d, %v", state, ok, row.wantState, row.wantOK)
			}
		})
	}
}

// PeerGone answers from the socket's state: one that cannot be asked has no
// peer known gone, where a state read as 0 would be CLOSED, a peer gone; one
// in a state its peer's FIN or a reset leads to has its peer gone, and one
// ESTABLISHED has not.
func TestPeerGoneByTheSocketsState(t *testing.T) {
	_, s := loopbackPair(t)
	if gone, known := peerGone(s, func(uintptr) (uint32, bool) { return 0, false }); gone || known {
		t.Fatalf("a socket that cannot be asked: gone %v, known %v; want neither", gone, known)
	}
	const wtcpEstablished = 4
	for _, row := range []struct {
		state    uint32
		wantGone bool
	}{
		{state: wtcpEstablished},
		{state: wtcpCloseWait, wantGone: true},
		{state: wtcpClosing, wantGone: true},
		{state: wtcpLastAck, wantGone: true},
		{state: wtcpClosed, wantGone: true},
	} {
		gone, known := peerGone(s, func(uintptr) (uint32, bool) { return row.state, true })
		if gone != row.wantGone || !known {
			t.Errorf("a socket in state %d: gone %v, known %v; want gone %v, known", row.state, gone, known, row.wantGone)
		}
	}
}
