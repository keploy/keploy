package relay

import (
	"encoding/binary"
	"net"
	"os"
	"syscall"
	"testing"
)

// winInfo is a TCP_INFO_v0 answer of size bytes holding these counts and no
// fast retransmit (winAnswer).
func winInfo(size int, state, inFlight uint32, out uint64, retrans, timeouts uint32) []byte {
	return winAnswer(size, winTCPInfo{state: state, inFlight: inFlight, out: out, retrans: retrans, timeouts: timeouts})
}

// winAnswer is a TCP_INFO_v0 answer of size bytes holding i's counts, with
// every byte that no field read covers set to 0xFF.
func winAnswer(size int, i winTCPInfo) []byte {
	b := make([]byte, wtcpInfoSize)
	for n := range b {
		b[n] = 0xFF
	}
	binary.LittleEndian.PutUint32(b[wtiState:], i.state)
	binary.LittleEndian.PutUint32(b[wtiBytesInFlight:], i.inFlight)
	binary.LittleEndian.PutUint64(b[wtiBytesOut:], i.out)
	binary.LittleEndian.PutUint32(b[wtiBytesRetrans:], i.retrans)
	binary.LittleEndian.PutUint32(b[wtiFastRetrans:], i.fastRetrans)
	binary.LittleEndian.PutUint32(b[wtiTimeoutEpisodes:], i.timeouts)
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
	want := winTCPInfo{state: wtcpCloseWait, inFlight: 0x0102, out: 0x0a0b0c0d0e0f, retrans: 0x0304, fastRetrans: 0x0708, timeouts: 0x0506}
	for _, size := range []int{wtcpInfoSize, wtcpInfoNeeded} {
		got, ok := readSafely(t, winAnswer(size, want))
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

// The fields are read at TCP_INFO_v0's own offsets (mstcpip.h, Windows SDK
// 10.0.26100), written here as numbers, apart from the constants keploy reads
// with, and every field around them holds a value of its own: a field read
// one slot off reads its neighbour's.
func TestAWindowsTCPInfoIsReadAtMstcpipsOffsets(t *testing.T) {
	t.Parallel()
	b := make([]byte, 88) // sizeof(TCP_INFO_v0)
	u32 := map[int]string{
		4: "Mss", 20: "RttUs", 24: "MinRttUs", 28: "BytesInFlight", 32: "Cwnd", 36: "SndWnd", 40: "RcvWnd",
		44: "RcvBuf", 64: "BytesReordered", 68: "BytesRetrans", 72: "FastRetrans", 76: "DupAcksIn", 80: "TimeoutEpisodes",
	}
	for off := range u32 {
		binary.LittleEndian.PutUint32(b[off:], uint32(0x1000+off))
	}
	binary.LittleEndian.PutUint32(b[0:], wtcpCloseWait) // State
	binary.LittleEndian.PutUint64(b[8:], 0x3000000008)  // ConnectionTimeMs
	b[16] = 1                                           // TimestampsEnabled
	binary.LittleEndian.PutUint64(b[48:], 0x2000000030) // BytesOut
	binary.LittleEndian.PutUint64(b[56:], 0x2000000038) // BytesIn
	b[84] = 0x54                                        // SynRetrans
	want := winTCPInfo{state: wtcpCloseWait, inFlight: 0x1000 + 28, out: 0x2000000030,
		retrans: 0x1000 + 68, fastRetrans: 0x1000 + 72, timeouts: 0x1000 + 80}
	if got, ok := readSafely(t, b); !ok || got != want {
		t.Fatalf("TCP_INFO_v0 read as %+v, %v, want %+v", got, ok, want)
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
		{"a byte sent again", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4, retrans: 1}, false, closed(5), 9, true},
		{"more in flight than sent once", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 8, retrans: 3}, false, clientLook{said: clientUnknown}, 7, true},
		{"more sent again than sent", winTCPInfo{state: wtcpClosed, out: 3, retrans: 4}, false, clientLook{said: clientUnknown}, 0, false},
		{"a fast retransmit", winTCPInfo{state: wtcpClosed, out: 10, inFlight: 4, retrans: 1, fastRetrans: 1}, false, clientLook{said: clientUnknown}, 0, false},
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

// A client whose receive window stays closed for 5 s and more: the proxy's
// socket probes the window with one new byte, then resends that byte about 1,
// 2.5 and 5 s in, each resend counted in BytesOut and in BytesRetrans with no
// fast retransmit and no timeout (the counts of a Windows 11 loopback run, a
// 12288-byte window: the client acknowledged 12288 bytes and never more). The
// socket still says what the client took, each probe's resend counted once,
// and once the client has reset, the chunk the destination sent after what
// it took is refused, not teed; one it took part of is taken. A fast
// retransmit or a timeout in the connection's life leaves the counts unread,
// and the chunk teed, as before.
func TestAWindowsClientWhoseWindowStaysClosedHasWhatItNeverTookRefused(t *testing.T) {
	t.Parallel()
	const took = 12288
	probes := []struct {
		name  string
		state uint32
		out   uint64
		retr  uint32
	}{
		{"the window just closed", 4, took, 0},
		{"the first probe out", 4, took + 1, 0},
		{"its first resend", 4, took + 2, 1},
		{"its resend 2.5 s in", 4, took + 3, 2},
		{"its resend 5 s in", 4, took + 4, 3},
		{"the client reset after it", wtcpClosed, took + 4, 3},
	}
	for _, p := range probes {
		info := winTCPInfo{state: p.state, out: p.out, retrans: p.retr, inFlight: uint32(p.out - uint64(p.retr) - took)}
		l := info.look(false)
		if l.acked != took || l.said == clientUnknown {
			t.Fatalf("%s: look = %+v, want the %d bytes the client acknowledged", p.name, l, took)
		}
		if n, ok := info.sent(); !ok || n != p.out-uint64(p.retr) {
			t.Fatalf("%s: sent = %d, %v, want %d", p.name, n, ok, p.out-uint64(p.retr))
		}
		if p.state != wtcpClosed {
			continue
		}
		if v := chunkVerdict(l, took, false); v != verdictRefused {
			t.Errorf("%s: the chunk after what the client took is %v, want refused", p.name, v)
		}
		if v := chunkVerdict(l, took-1, false); v != verdictTaken {
			t.Errorf("%s: the chunk the client took a byte of is %v, want taken", p.name, v)
		}
		for _, fenced := range []winTCPInfo{
			{state: info.state, out: info.out, retrans: info.retrans, inFlight: info.inFlight, fastRetrans: 1},
			{state: info.state, out: info.out, retrans: info.retrans, inFlight: info.inFlight, timeouts: 1},
		} {
			if v := chunkVerdict(fenced.look(false), took, false); v != verdictAsBefore {
				t.Errorf("%s, fenced %+v: the chunk is %v, want teed as before", p.name, fenced, v)
			}
		}
	}
}

// A Windows socket's client took every byte written to it (so its close may
// reset it: resetClientOnClose) only when each was sent once and none is in
// flight, inside the counts' fence; an answer outside the fence, or one that
// counts more bytes sent than were written, says nothing.
func TestAWindowsSocketsClientTookEveryByteOnlyWhenNoneIsUnsentOrInFlight(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		info       winTCPInfo
		written    uint64
		all, known bool
	}{
		{"every byte sent and acknowledged", winTCPInfo{out: 10}, 10, true, true},
		{"nothing written", winTCPInfo{}, 0, true, true},
		{"bytes in flight", winTCPInfo{out: 10, inFlight: 3}, 10, false, true},
		{"the proxy's FIN in flight", winTCPInfo{out: 10, inFlight: 1}, 10, false, true},
		{"bytes the send buffer holds unsent", winTCPInfo{out: 7}, 10, false, true},
		{"a window probe sent again, every byte acknowledged", winTCPInfo{out: 12, retrans: 2}, 10, true, true},
		{"a write around the count", winTCPInfo{out: 11}, 10, false, false},
		{"a fast retransmit", winTCPInfo{out: 10, fastRetrans: 1}, 10, false, false},
		{"a timeout", winTCPInfo{out: 10, timeouts: 1}, 10, false, false},
	} {
		if all, known := row.info.tookAll(row.written); all != row.all || known != row.known {
			t.Errorf("%s: tookAll(%d) = %v, %v; want %v, %v", row.name, row.written, all, known, row.all, row.known)
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
		{"a window probe sent again", winTCPInfo{state: wtcpClosed, out: 12, inFlight: 1, retrans: 1}, false, false, 0},
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
		{"one byte in flight (a window probe's)", winsockRead(wsaeConnAborted), winTCPInfo{state: wtcpClosed, out: 8, inFlight: 1}, false, true},
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
