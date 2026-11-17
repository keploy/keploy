package relay

import (
	"encoding/binary"
	"net"
	"os"
	"syscall"
	"testing"
)

// winInfo is a TCP_INFO_v0 answer of size bytes holding these counts, with
// every byte that no field read covers set to 0xFF.
func winInfo(size int, state, inFlight uint32, out uint64, retrans, timeouts uint32) []byte {
	b := make([]byte, wtcpInfoSize)
	for i := range b {
		b[i] = 0xFF
	}
	binary.LittleEndian.PutUint32(b[wtiState:], state)
	binary.LittleEndian.PutUint32(b[wtiBytesInFlight:], inFlight)
	binary.LittleEndian.PutUint64(b[wtiBytesOut:], out)
	binary.LittleEndian.PutUint32(b[wtiBytesRetrans:], retrans)
	binary.LittleEndian.PutUint32(b[wtiTimeoutEpisodes:], timeouts)
	return b[:size]
}

// readSafely reads b as readWinTCPInfo does, failing the row where the read
// goes past b's end instead of answering false.
func readSafely(t *testing.T, b []byte) (info winTCPInfo, ok bool) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("reading a %d-byte TCP_INFO_v0 answer went past its end: %v", len(b), p)
			ok = false
		}
	}()
	return readWinTCPInfo(b)
}

// A TCP_INFO_v0 answer is read at its fields' offsets in mstcpip.h's struct,
// and only when it holds the last of them: an older or changed struct that
// answers shorter is not read.
func TestAWindowsTCPInfoIsReadOnlyWhenItHoldsEveryFieldRead(t *testing.T) {
	t.Parallel()
	want := winTCPInfo{state: wtcpCloseWait, inFlight: 0x0102, out: 0x0a0b0c0d0e0f, retrans: 0x0304, timeouts: 0x0506}
	for _, size := range []int{wtcpInfoSize, wtcpInfoNeeded} {
		got, ok := readSafely(t, winInfo(size, want.state, want.inFlight, want.out, want.retrans, want.timeouts))
		if !ok || got != want {
			t.Fatalf("a %d-byte answer read as %+v, %v, want %+v", size, got, ok, want)
		}
	}
	for _, size := range []int{wtcpInfoNeeded - 1, wtiTimeoutEpisodes, wtiBytesOut, 0} {
		if got, ok := readSafely(t, winInfo(size, wtcpClosed, 4, 10, 0, 0)); ok {
			t.Fatalf("a %d-byte answer, shorter than the %d keploy reads, read as %+v", size, wtcpInfoNeeded, got)
		}
	}
}

// What a Windows socket's look says, from its TCP state and its counts.
func TestAWindowsSocketsLookIsItsStateAndTheBytesAcknowledged(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		info     winTCPInfo
		finSent  bool
		want     clientLook
		wantSent uint64
		sentOK   bool
	}{
		{"open", winTCPInfo{state: 4, out: 10, inFlight: 4}, false, open(6), 10, true},
		{"proxy's FIN out first", winTCPInfo{state: 5, out: 10}, true, open(10), 10, true},
		{"both FINs out at once", winTCPInfo{state: 8, out: 10}, true, open(10), 10, true},
		{"client's FIN in", winTCPInfo{state: wtcpCloseWait, out: 10, inFlight: 4}, false, finned(6), 10, true},
		{"client's FIN in, the proxy's out", winTCPInfo{state: wtcpLastAck, out: 10, inFlight: 4}, true, finned(7), 10, true},
		{"closed", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4}, false, closed(6), 10, true},
		{"closed after both FINs", winTCPInfo{state: wtcpTimeWait, out: 10}, true, closed(10), 10, true},
		{"the proxy's FIN alone in flight", winTCPInfo{state: wtcpLastAck, out: 10, inFlight: 1}, true, finned(10), 10, true},
		{"only the FIN sent, in flight", winTCPInfo{state: wtcpLastAck, inFlight: 1}, true, finned(0), 0, true},
		{"none in flight after the FIN", winTCPInfo{state: wtcpClosed, out: 10}, true, closed(10), 10, true},
		{"every byte sent in flight", winTCPInfo{state: 4, out: 4, inFlight: 4}, false, open(0), 4, true},
		{"more in flight than sent", winTCPInfo{state: wtcpClosed, out: 3, inFlight: 4}, false, clientLook{said: clientUnknown}, 3, true},
		{"a byte sent again", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4, retrans: 1}, false, clientLook{said: clientUnknown}, 0, false},
		{"a timeout", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4, timeouts: 1}, false, clientLook{said: clientUnknown}, 0, false},
	} {
		if got := row.info.look(row.finSent); got != row.want {
			t.Errorf("%s: look = %+v, want %+v", row.name, got, row.want)
		}
		if n, ok := row.info.sent(); ok != row.sentOK || ok && n != row.wantSent {
			t.Errorf("%s: sent = %d, %v, want %d, %v", row.name, n, ok, row.wantSent, row.sentOK)
		}
	}
}

// The reset a Windows socket kept no error for, told from its state and
// counts as Linux's socket keeps it: EPIPE in CLOSE_WAIT, ECONNRESET in every
// other state; nothing where the connection may have closed cleanly, a
// timeout may have closed it, or it is not closed.
func TestAWindowsSocketsResetIsToldFromItsStateAndCounts(t *testing.T) {
	t.Parallel()
	closedWith := func(inFlight uint32) winTCPInfo {
		return winTCPInfo{state: wtcpClosed, out: 10, inFlight: inFlight}
	}
	for _, row := range []struct {
		name             string
		info             winTCPInfo
		finRead, finSent bool
		want             syscall.Errno
	}{
		{"a chunk reset after the client's FIN", closedWith(4), true, false, syscall.EPIPE},
		{"a chunk reset after both FINs", closedWith(4), true, true, syscall.ECONNRESET},
		{"a chunk reset with no FIN", closedWith(4), false, false, syscall.ECONNRESET},
		{"a chunk reset after the proxy's FIN alone", closedWith(4), false, true, syscall.ECONNRESET},
		{"the proxy's FIN reset", closedWith(1), true, true, syscall.ECONNRESET},
		{"every byte taken, reset after the client's FIN", closedWith(0), true, false, syscall.EPIPE},
		{"every byte taken, reset with no FIN", closedWith(0), false, false, syscall.ECONNRESET},
		{"every byte and the proxy's FIN taken", closedWith(0), true, true, 0},
		{"every byte taken after the proxy's FIN alone", closedWith(0), false, true, 0},
		{"not closed", winTCPInfo{state: wtcpCloseWait, out: 10, inFlight: 4}, true, false, 0},
		{"closed after both FINs", winTCPInfo{state: wtcpTimeWait, out: 10}, true, true, 0},
		{"a byte sent again", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4, retrans: 1}, true, false, 0},
		{"a timeout", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4, timeouts: 1}, true, false, 0},
	} {
		if got := row.info.resetCode(row.finRead, row.finSent); got != row.want {
			t.Errorf("%s (FIN read %v, sent %v): resetCode = %v, want %v", row.name, row.finRead, row.finSent, got, row.want)
		}
	}
}

// winsockRead is a read's error as Windows' net gives it for errno.
func winsockRead(errno syscall.Errno) error {
	return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", errno)}
}

// A Windows read's reset is taken as one that came after the client's FIN
// only in the case measured: WSAECONNABORTED on a connection CLOSED with a
// byte in flight, no timeout episode and no FIN of the proxy's. Every other
// read is a reset, as before.
func TestAWindowsReadsResetIsAfterTheClientsFINOnlyByItsCodeAndCounts(t *testing.T) {
	t.Parallel()
	closed := winTCPInfo{state: wtcpClosed, out: 8, inFlight: 8}
	for _, row := range []struct {
		name    string
		err     error
		info    winTCPInfo
		finSent bool
		want    bool
	}{
		{"WSAECONNABORTED, closed with the proxy's bytes in flight", winsockRead(wsaeConnAborted), closed, false, true},
		{"the same, retransmitted once", winsockRead(wsaeConnAborted), winTCPInfo{state: wtcpClosed, out: 8, inFlight: 8, retrans: 8}, false, true},
		{"WSAECONNRESET: a reset with no FIN before", winsockRead(10054), closed, false, false},
		{"WSAENETRESET", winsockRead(10052), closed, false, false},
		{"a read that ended in no error", nil, closed, false, false},
		{"a timeout episode", winsockRead(wsaeConnAborted), winTCPInfo{state: wtcpClosed, out: 8, inFlight: 8, timeouts: 1}, false, false},
		{"nothing in flight", winsockRead(wsaeConnAborted), winTCPInfo{state: wtcpClosed, out: 8}, false, false},
		{"after the proxy's FIN", winsockRead(wsaeConnAborted), closed, true, false},
		{"not closed", winsockRead(wsaeConnAborted), winTCPInfo{state: wtcpCloseWait, out: 8, inFlight: 8}, false, false},
	} {
		if got := row.info.resetAfterFIN(row.err, row.finSent); got != row.want {
			t.Errorf("%s: resetAfterFIN = %v, want %v", row.name, got, row.want)
		}
	}
}
