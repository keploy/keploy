package relay

import (
	"encoding/binary"
	"os"
	"testing"
)

// tcpsEstablished is xnu's ESTABLISHED (bsd/netinet/tcp_fsm.h).
const tcpsEstablished = 4

// scriptedXNU is a client socket whose TCP_INFO is answered from a script,
// in turn (the last repeats).
type scriptedXNU struct {
	infos []darwinTCPInfo
	asked int
}

func (x *scriptedXNU) socket() *tcpClientSocket {
	s := &tcpClientSocket{askInfo: func(_ uintptr, _ *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
		*info = x.infos[min(x.asked, len(x.infos)-1)]
		x.asked++
		return true
	}}
	s.rc = noFD{}
	s.ask, s.count = s.lookAt, s.countAt
	return s
}

// noFD runs a question with no socket under it: the script answers.
type noFD struct{}

func (noFD) Control(f func(uintptr)) error  { f(0); return nil }
func (noFD) Read(func(uintptr) bool) error  { return nil }
func (noFD) Write(func(uintptr) bool) error { return nil }

// On macOS a look counts what the client's stack acknowledged from snd_nxt
// less tcpi_txunacked, from the first look on, across the wrap of the 32-bit
// sequence numbers; written adds what the send buffer holds. Both count from
// the same byte, so a chunk is judged against its own first byte.
func TestXNUCountsWhatTheClientAcknowledgedAcrossTheWrap(t *testing.T) {
	start := ^uint32(0) - 99 // 100 bytes before the wrap
	x := &scriptedXNU{infos: []darwinTCPInfo{
		// The first look: 40 bytes sent and not acknowledged, 40 held.
		{state: tcpsEstablished, sndNxt: start, txunacked: 40, sbbytes: 40},
		// 300 more written and sent past the wrap; 100 of all of them
		// acknowledged: 240 in flight, all 240 held.
		{state: tcpsCloseWait, sndNxt: start + 300, txunacked: 240, sbbytes: 240},
		// Everything acknowledged.
		{state: tcpsCloseWait, sndNxt: start + 300, txunacked: 0, sbbytes: 0},
	}}
	s := x.socket()
	var l clientLook
	l = s.look()
	if l.said != clientOpen || l.acked != 0 {
		t.Fatalf("first look: %+v, want open, 0 acknowledged", l)
	}
	s.wrote, s.wroteOK = s.written()
	if !s.wroteOK || s.wrote != 340 {
		t.Fatalf("written: %d %v, want 340 (100 acknowledged from the first look, 240 held)", s.wrote, s.wroteOK)
	}
	l = s.look()
	if l.said != clientFinned || l.acked != 340 {
		t.Fatalf("last look: %+v, want the FIN in, 340 acknowledged", l)
	}
}

// What each state of the proxy's socket says of the client, as on Linux: a
// TIME_WAIT socket (the client's FIN after the proxy's was acknowledged) is
// closed; CLOSING and the FIN_WAITs are open.
func TestXNUStatesReadAsLinuxs(t *testing.T) {
	for state, want := range map[uint8]clientSaid{
		tcpsClosed: clientClosed, tcpsTimeWait: clientClosed,
		tcpsCloseWait: clientFinned, tcpsLastAck: clientFinned,
		tcpsEstablished: clientOpen, 6: clientOpen, 7: clientOpen, 9: clientOpen,
	} {
		s := (&scriptedXNU{infos: []darwinTCPInfo{{state: state, sndNxt: 7}}}).socket()
		if s.got = s.look(); s.got.said != want {
			t.Errorf("state %d says %d, want %d", state, s.got.said, want)
		}
	}
}

// While the socket resends what was lost, snd_nxt can be behind snd_max:
// snd_nxt less txunacked is then less than snd_una. A look that the socket
// says is in loss recovery, or that finds snd_nxt behind where a look found
// it before, counts the most it counted before, never more; finds a closed
// connection unknown (its chunk teed, as before); and written cannot say.
func TestXNUAResendingSocketNeverSaysMoreThanItKnows(t *testing.T) {
	for _, row := range []struct {
		name string
		info darwinTCPInfo
	}{
		{"loss recovery", darwinTCPInfo{state: tcpsClosed, lossRecovery: true, sndNxt: 1000, txunacked: 800}},
		{"snd_nxt went back", darwinTCPInfo{state: tcpsClosed, sndNxt: 600, txunacked: 500}},
	} {
		t.Run(row.name, func(t *testing.T) {
			x := &scriptedXNU{infos: []darwinTCPInfo{
				{state: tcpsEstablished, sndNxt: 0, txunacked: 0},
				{state: tcpsCloseWait, sndNxt: 1000, txunacked: 800, sbbytes: 800},
				row.info,
			}}
			s := x.socket()
			_ = s.look()
			_ = s.look()
			if s.got.said != clientFinned || s.got.acked != 200 {
				t.Fatalf("second look: %+v, want the FIN in, 200 acknowledged", s.got)
			}
			_ = s.look()
			if s.got.said != clientUnknown {
				t.Fatalf("a closed look while resending says %d, want unknown", s.got.said)
			}
			if s.acked != 200 {
				t.Fatalf("%d acknowledged while resending, want the 200 counted before", s.acked)
			}
			s.wrote, s.wroteOK = s.written()
			if s.wroteOK {
				t.Fatalf("written says %d while resending, want it cannot say", s.wrote)
			}
		})
	}
}

// Once resending ends, snd_nxt is snd_max again and the counts are exact.
func TestXNUCountsAreExactAgainOnceResendingEnds(t *testing.T) {
	x := &scriptedXNU{infos: []darwinTCPInfo{
		{state: tcpsEstablished, sndNxt: 0},
		{state: tcpsEstablished, sndNxt: 1000, txunacked: 800, sbbytes: 800},
		{state: tcpsEstablished, sndNxt: 300, txunacked: 800, sbbytes: 800, lossRecovery: true},
		{state: tcpsEstablished, sndNxt: 1000, txunacked: 100, sbbytes: 150},
	}}
	s := x.socket()
	for range 3 {
		_ = s.look()
	}
	if s.got.acked != 200 {
		t.Fatalf("while resending: %d acknowledged, want the 200 counted before", s.got.acked)
	}
	s.wrote, s.wroteOK = s.written()
	if !s.wroteOK || s.wrote != 900+150 {
		t.Fatalf("written: %d %v, want 1050 (900 acknowledged, 150 held)", s.wrote, s.wroteOK)
	}
}

// A socket that cannot answer says nothing.
func TestXNUASocketThatCannotAnswerSaysNothing(t *testing.T) {
	s := &tcpClientSocket{rc: noFD{}, askInfo: func(uintptr, *[tcpInfoBuf]byte, *darwinTCPInfo) bool { return false }}
	s.ask, s.count = s.lookAt, s.countAt
	if s.got = s.look(); s.got.said != clientUnknown {
		t.Fatalf("look: %+v, want unknown", s.got)
	}
	if _, ok := s.written(); ok {
		t.Fatal("written says a count, want it cannot say")
	}
}

// A TCP_INFO answer is read at xnu's offsets, and one shorter than a look
// needs is not read.
func TestXNUReadsTCPInfoAtItsOffsets(t *testing.T) {
	b := make([]byte, tcpInfoNeeded)
	le := binary.LittleEndian
	b[tcpiState] = tcpsLastAck
	le.PutUint32(b[tcpiFlags:], tcpiLossRecovery|0x4)
	le.PutUint32(b[tcpiSndNxt:], 0xfffffff0)
	le.PutUint32(b[tcpiSndSbbytes:], 1234)
	le.PutUint64(b[tcpiTxunacked:], 77)
	le.PutUint32(b[tcpiRcvNxt:], 0xffffff00)
	le.PutUint64(b[tcpiRxbytes:], 5000)
	le.PutUint64(b[tcpiRxdupbytes:], 40)
	le.PutUint64(b[tcpiRxooobytes:], 3)
	var info darwinTCPInfo
	if !readTCPInfo(b, &info) {
		t.Fatal("readTCPInfo = false for an answer as long as a look needs")
	}
	want := darwinTCPInfo{state: tcpsLastAck, lossRecovery: true, sndNxt: 0xfffffff0, sbbytes: 1234, txunacked: 77,
		received: received{rcvNxt: 0xffffff00, rxBytes: 5000, dup: 40, ooo: 3}}
	if info != want {
		t.Fatalf("readTCPInfo read %+v, want %+v", info, want)
	}
	le.PutUint32(b[tcpiFlags:], 0x4) // other flags only: not resending
	if !readTCPInfo(b, &info) || info.lossRecovery {
		t.Fatalf("readTCPInfo read %+v with TCPI_FLAG_LOSSRECOVERY clear, want not in loss recovery", info)
	}
	info = darwinTCPInfo{}
	if readTCPInfo(b[:tcpInfoNeeded-1], &info) || info != (darwinTCPInfo{}) {
		t.Fatalf("readTCPInfo read %+v from an answer one byte short, want nothing read", info)
	}
}

// A file that is no TCP socket answers no TCP_INFO: the call fails, and
// tcpInfo says nothing, whatever its buffer holds.
func TestXNUAFileThatIsNoTCPSocketAnswersNoTCPInfo(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	var buf [tcpInfoBuf]byte
	if _, err := getTCPInfo(r.Fd(), &buf); err == nil {
		t.Fatal("getTCPInfo answered for a pipe; want the call's error")
	}
	s := &tcpClientSocket{getInfo: getTCPInfo, fallback: &infoFallback{}}
	var info darwinTCPInfo
	if s.tcpInfo(r.Fd(), &buf, &info) {
		t.Fatalf("tcpInfo = true, read %+v, for a pipe; want false", info)
	}
}
