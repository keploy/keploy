//go:build windows

package relay

import (
	"errors"
	"net"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/windows"
)

// wtcpEstablished is TCPSTATE's ESTABLISHED.
const wtcpEstablished = 4

// winScriptFD runs a question with no socket under it: the script answers.
type winScriptFD struct{}

func (winScriptFD) Control(f func(uintptr)) error   { f(0); return nil }
func (winScriptFD) Read(f func(uintptr) bool) error { f(0); return nil }
func (winScriptFD) Write(func(uintptr) bool) error  { return nil }

// winFailingFD is a socket whose Control fails, as a closed one's does.
type winFailingFD struct{}

func (winFailingFD) Control(func(uintptr)) error    { return syscall.EINVAL }
func (winFailingFD) Read(func(uintptr) bool) error  { return syscall.EINVAL }
func (winFailingFD) Write(func(uintptr) bool) error { return syscall.EINVAL }

// winAnsweringSocket is a client socket whose SIO_TCP_INFO answers ans, in full.
// Its next read (readAgain), when one is made, fails with WSAECONNABORTED, as
// the read after a client's FIN does: a read made where none may be is then
// taken as that FIN, and the row that made it fails on its answer.
func winAnsweringSocket(ans func() []byte) *tcpClientSocket {
	s := &tcpClientSocket{rc: winScriptFD{}, fallback: &infoFallback{}}
	s.getInfo = func(_ uintptr, buf *[wtcpInfoSize]byte) (int, error) {
		return copy(buf[:], ans()), nil
	}
	s.recvAgain = func(uintptr) error { return windows.WSAECONNABORTED }
	s.ask = s.lookAt
	return s
}

// SIO_TCP_INFO's answer is as long as the call says, and never longer than
// the buffer: a short one (an older or changed TCP_INFO_v0) is then read as
// short, and a failed call answers nothing.
func TestWindowsASIOTCPInfoAnswerIsAsLongAsTheCallSays(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		said    uint32
		err     error
		wantLen int
	}{
		{said: wtcpInfoSize, wantLen: wtcpInfoSize},
		{said: wtcpInfoNeeded - 1, wantLen: wtcpInfoNeeded - 1},
		{said: 0, wantLen: 0},
		{said: 2 * wtcpInfoSize, wantLen: wtcpInfoSize},
		{said: wtcpInfoSize, err: windows.WSAEINVAL, wantLen: 0},
	} {
		ioctl := func(_ windows.Handle, iocc uint32, _ *byte, _ uint32, _ *byte, cbob uint32,
			cbbr *uint32, _ *windows.Overlapped, _ uintptr) error {
			if iocc != sioTCPInfo || cbob != wtcpInfoSize {
				t.Fatalf("asked ioctl %#x into %d bytes, want SIO_TCP_INFO into %d", iocc, cbob, wtcpInfoSize)
			}
			*cbbr = row.said
			return row.err
		}
		var buf [wtcpInfoSize]byte
		n, err := askTCPInfo(ioctl, 0, &buf)
		if n != row.wantLen || err != row.err {
			t.Errorf("an answer said to be %d bytes, %v: %d, %v; want %d, %v", row.said, row.err, n, err, row.wantLen, row.err)
		}
	}
}

// A SIO_TCP_INFO that answers shorter than the last field keploy reads (an
// older or changed TCP_INFO_v0), or a call that fails, says nothing of the
// client: a look is unknown, written cannot count, no reset is told, and the
// relay says so once, whatever is asked after. On a real socket.
func TestWindowsASIOTCPInfoThatCannotAnswerSaysNothing(t *testing.T) {
	for _, row := range []struct {
		name  string
		get   func(fd uintptr, buf *[wtcpInfoSize]byte) (int, error)
		level zapcore.Level
		said  string
	}{
		{name: "an answer one byte short", level: zapcore.WarnLevel, said: "answers a SIO_TCP_INFO shorter than keploy reads",
			get: func(fd uintptr, buf *[wtcpInfoSize]byte) (int, error) {
				n, err := getTCPInfo(fd, buf)
				return min(n, wtcpInfoNeeded-1), err
			}},
		{name: "a failing call", level: zapcore.DebugLevel, said: "SIO_TCP_INFO failed",
			get: func(uintptr, *[wtcpInfoSize]byte) (int, error) { return 0, windows.WSAEINVAL }},
	} {
		t.Run(row.name, func(t *testing.T) {
			app, proxyClient := loopbackPair(t)
			core, logs := observer.New(zapcore.DebugLevel)
			s := clientSocketOf(proxyClient, zap.New(core)).(*tcpClientSocket)
			s.getInfo, s.fallback = row.get, &infoFallback{}
			// A client that closed with bytes of the proxy's unread would be
			// told finned, and then reset, by a socket that answers.
			if _, err := proxyClient.Write([]byte("answer")); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			for i := 0; i < 2; i++ {
				if l := s.look(); l.said != clientUnknown {
					t.Fatalf("look %d: %+v, want unknown", i, l)
				}
				if n, ok := s.written(); ok {
					t.Fatalf("written %d: %d, true; want no count", i, n)
				}
				if err := s.pendingError(true); err != nil {
					t.Fatalf("pendingError %d: %v, want none", i, err)
				}
			}
			if s.finBeforeReset(winsockRead(windows.WSAECONNABORTED)) {
				t.Fatal("finBeforeReset = true from a socket that said nothing")
			}
			said := logs.FilterMessageSnippet(row.said)
			if n := said.Len(); n != 1 {
				t.Fatalf("the relay said %q %d times, want once", row.said, n)
			}
			if lv := said.All()[0].Level; lv != row.level {
				t.Fatalf("the relay said %q at %v, want %v", row.said, lv, row.level)
			}
		})
	}
}

// A socket with no logger whose SIO_TCP_INFO cannot answer says nothing, and
// logs nothing.
func TestWindowsASocketWithNoLoggerLogsNoFallback(t *testing.T) {
	_, proxyClient := loopbackPair(t)
	s := clientSocketOf(proxyClient, nil).(*tcpClientSocket)
	s.fallback = &infoFallback{}
	s.getInfo = func(uintptr, *[wtcpInfoSize]byte) (int, error) { return wtcpInfoNeeded - 1, nil }
	if l := s.look(); l.said != clientUnknown {
		t.Fatalf("look: %+v, want unknown", l)
	}
	s.getInfo = func(uintptr, *[wtcpInfoSize]byte) (int, error) { return 0, windows.WSAEINVAL }
	if l := s.look(); l.said != clientUnknown {
		t.Fatalf("look: %+v, want unknown", l)
	}
}

// A socket that cannot be asked (its Control fails) says nothing: a look is
// unknown, written cannot count, and no reset is told.
func TestWindowsASocketThatCannotBeAskedSaysNothing(t *testing.T) {
	s := &tcpClientSocket{rc: winFailingFD{}, fallback: &infoFallback{}}
	s.getInfo = func(_ uintptr, buf *[wtcpInfoSize]byte) (int, error) {
		return copy(buf[:], winInfo(wtcpInfoSize, wtcpClosed, 6, 6, 0, 0)), nil
	}
	s.ask = s.lookAt
	if l := s.look(); l.said != clientUnknown {
		t.Fatalf("look: %+v, want unknown", l)
	}
	if n, ok := s.written(); ok {
		t.Fatalf("written: %d, true; want no count", n)
	}
	if err := s.pendingError(false); err != nil {
		t.Fatalf("pendingError: %v, want none", err)
	}
}

// A look takes the relay's word that the proxy sent its FIN (proxyFINSent):
// one byte in flight is then the FIN's, and every byte sent is acknowledged
// (acked).
func TestWindowsALookAfterTheProxysFINTakesOneByteInFlightForIt(t *testing.T) {
	t.Parallel()
	ans := func() []byte { return winInfo(wtcpInfoSize, wtcpLastAck, 1, 6, 0, 0) }
	if l := winAnsweringSocket(ans).look(); l.said != clientFinned || l.acked != 5 {
		t.Fatalf("with no FIN of the proxy's sent: %+v, want the FIN in and 5 bytes acknowledged", l)
	}
	s := winAnsweringSocket(ans)
	s.proxyFINSent()
	if l := s.look(); l.said != clientFinned || l.acked != 6 {
		t.Fatalf("after the proxy's FIN: %+v, want the FIN in and all 6 bytes acknowledged", l)
	}
}

// A look that finds the client's FIN in (CLOSE_WAIT, or LAST_ACK once the
// proxy's FIN is out) is kept: a reset read later came after the FIN. One
// that finds the connection open, or closed, is not.
func TestWindowsALookThatFindsTheFINInIsKept(t *testing.T) {
	for _, row := range []struct {
		state uint32
		want  bool
	}{
		{wtcpCloseWait, true}, {wtcpLastAck, true}, {wtcpEstablished, false}, {wtcpClosed, false},
	} {
		s := winAnsweringSocket(func() []byte { return winInfo(wtcpInfoSize, row.state, 0, 6, 0, 0) })
		_ = s.look()
		if got := s.finBeforeReset(winsockRead(windows.WSAECONNRESET)); got != row.want {
			t.Errorf("state %d: finBeforeReset = %v, want %v", row.state, got, row.want)
		}
	}
}

// A read that failed with WSAECONNABORTED after no look found the client's
// FIN in asks the socket, which says whether the reset came after that FIN
// (resetAfterFIN): yes for a connection closed with the proxy's bytes in
// flight, no after the proxy's own FIN or a timeout episode, and no when the
// socket cannot answer. A WSAECONNRESET is a reset unless the socket's next
// read fails with WSAECONNABORTED. One look that found the FIN in decides,
// whatever the read's code.
func TestWindowsAResetReadAfterNoLookFoundTheFINIsAskedOfTheSocket(t *testing.T) {
	aborted, reset := winsockRead(windows.WSAECONNABORTED), winsockRead(windows.WSAECONNRESET)
	answering := func(size int, timeouts uint32, again error) *tcpClientSocket {
		s := winAnsweringSocket(func() []byte { return winInfo(size, wtcpClosed, 8, 8, 0, timeouts) })
		s.recvAgain = func(uintptr) error { return again }
		return s
	}
	for _, row := range []struct {
		name    string
		s       *tcpClientSocket
		err     error
		finSent bool
		want    bool
	}{
		{"WSAECONNABORTED, closed with bytes in flight", answering(wtcpInfoSize, 0, windows.WSAECONNRESET), aborted, false, true},
		{"WSAECONNRESET, and the next read", answering(wtcpInfoSize, 0, windows.WSAECONNRESET), reset, false, false},
		{"WSAECONNRESET, the next read WSAECONNABORTED", answering(wtcpInfoSize, 0, windows.WSAECONNABORTED), reset, false, true},
		{"after the proxy's FIN", answering(wtcpInfoSize, 0, windows.WSAECONNABORTED), aborted, true, false},
		{"after a timeout episode", answering(wtcpInfoSize, 1, windows.WSAECONNABORTED), aborted, false, false},
		{"an answer one byte short", answering(wtcpInfoNeeded-1, 0, windows.WSAECONNABORTED), aborted, false, false},
		{"a socket that cannot be asked", &tcpClientSocket{rc: winFailingFD{}, getInfo: func(_ uintptr, buf *[wtcpInfoSize]byte) (int, error) {
			return copy(buf[:], winInfo(wtcpInfoSize, wtcpClosed, 8, 8, 0, 0)), nil
		}, recvAgain: func(uintptr) error { return windows.WSAECONNABORTED }, fallback: &infoFallback{}}, reset, false, false},
	} {
		if row.finSent {
			row.s.proxyFINSent()
		}
		if got := row.s.finBeforeReset(row.err); got != row.want {
			t.Errorf("%s: finBeforeReset = %v, want %v", row.name, got, row.want)
		}
	}
	finned := winAnsweringSocket(func() []byte { return winInfo(wtcpInfoSize, wtcpCloseWait, 0, 6, 0, 0) })
	_ = finned.look()
	if !finned.finBeforeReset(reset) {
		t.Error("finBeforeReset = false after a look found the FIN in; want true, whatever the read's code")
	}
}

// On a real socket: the next read of a connection the client reset says
// whether its FIN came first. A client that closed and then reset the
// proxy's answer: WSAECONNABORTED, so a read that failed with WSAECONNRESET
// on it is a reset after the FIN. A client that aborted with the proxy's
// bytes in flight, no FIN sent: WSAECONNRESET, and such a read is a reset.
func TestWindowsTheNextReadOfAResetSocketSaysWhetherTheFINCameFirst(t *testing.T) {
	t.Parallel()
	for _, finFirst := range []bool{true, false} {
		app, proxyClient := loopbackPair(t)
		rc, err := proxyClient.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		s := newTCPClientSocket(rc, nil)
		want := windows.WSAECONNABORTED
		if finFirst {
			_ = app.Close()
			awaitTCPState(t, proxyClient, stateCloseWait, "the closed client's FIN is in")
			if _, err := proxyClient.Write([]byte("the answer")); err != nil {
				t.Fatal(err)
			}
		} else {
			want = windows.WSAECONNRESET
			stop := make(chan struct{})
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				chunk := make([]byte, 1024)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := proxyClient.Write(chunk); err != nil {
						return
					}
				}
			}()
			if err := app.(*net.TCPConn).SetLinger(0); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			awaitTCPState(t, proxyClient, stateClosed, "the aborting client's reset is in")
			close(stop)
			<-wrote
		}
		awaitTCPState(t, proxyClient, stateClosed, "the connection is closed")
		if got := s.readAgain(); !errors.Is(got, want) {
			t.Fatalf("FIN first %v: the next read failed with %v, want %v", finFirst, got, want)
		}
		if got := s.finBeforeReset(winsockRead(windows.WSAECONNRESET)); got != finFirst {
			t.Fatalf("FIN first %v: a read's WSAECONNRESET is taken as after the FIN: %v", finFirst, got)
		}
	}
}

// The reset a closed connection tells is given once, as the kernel gives an
// error once.
func TestWindowsAResetIsGivenOnce(t *testing.T) {
	s := winAnsweringSocket(func() []byte { return winInfo(wtcpInfoSize, wtcpClosed, 6, 6, 0, 0) })
	if err := s.pendingError(false); err != syscall.ECONNRESET {
		t.Fatalf("pendingError: %v, want ECONNRESET", err)
	}
	if err := s.pendingError(false); err != nil {
		t.Fatalf("pendingError asked again: %v, want none", err)
	}
}

// Once the relay has sent the client its FIN, a connection closed with
// nothing in flight may have closed cleanly, and tells no reset; before it
// did, the same answer is a reset (the client reset with the proxy's bytes
// in hand).
func TestWindowsTheProxysFINIsTakenIntoTheReset(t *testing.T) {
	ans := func() []byte { return winInfo(wtcpInfoSize, wtcpClosed, 0, 6, 0, 0) }
	if err := winAnsweringSocket(ans).pendingError(false); err != syscall.ECONNRESET {
		t.Fatalf("pendingError before the proxy's FIN: %v, want ECONNRESET", err)
	}
	s := winAnsweringSocket(ans)
	s.proxyFINSent()
	if err := s.pendingError(false); err != nil {
		t.Fatalf("pendingError after the proxy's FIN: %v, want none", err)
	}
}

// A socket says the connection has closed before the proxy's FIN only once it
// has: CLOSED, whatever its counts. A socket that cannot say does not: the
// FIN is taken as sent, as before.
func TestWindowsAConnectionClosedBeforeTheProxysFINSaysSo(t *testing.T) {
	for _, row := range []struct {
		name string
		ans  []byte
		want bool
	}{
		{"closed", winInfo(wtcpInfoSize, wtcpClosed, 6, 6, 0, 0), true},
		{"closed, a byte sent again", winInfo(wtcpInfoSize, wtcpClosed, 6, 6, 1, 0), true},
		{"open", winInfo(wtcpInfoSize, wtcpEstablished, 6, 6, 0, 0), false},
		{"the client's FIN in", winInfo(wtcpInfoSize, wtcpCloseWait, 6, 6, 0, 0), false},
		{"the proxy's FIN out", winInfo(wtcpInfoSize, wtcpLastAck, 6, 6, 0, 0), false},
		{"an answer too short", winInfo(wtcpInfoNeeded-1, wtcpClosed, 6, 6, 0, 0), false},
	} {
		if got := winAnsweringSocket(func() []byte { return row.ans }).closedBeforeFIN(); got != row.want {
			t.Errorf("%s: closedBeforeFIN = %v, want %v", row.name, got, row.want)
		}
	}
	s := winAnsweringSocket(func() []byte { return winInfo(wtcpInfoSize, wtcpClosed, 6, 6, 0, 0) })
	s.rc = winFailingFD{}
	if s.closedBeforeFIN() {
		t.Error("a socket that cannot be asked said the connection had closed")
	}
}

// On a real socket: a connection the client reset says so before the proxy's
// half-close, whose shutdown still succeeds and sends no FIN; the reset
// answer then reads as refused, not one byte of it as taken.
func TestWindowsAConnectionTheClientResetSaysSoBeforeTheProxysFIN(t *testing.T) {
	t.Parallel()
	const answer = "the answer, after the client's FIN"
	app, proxyClient := loopbackPair(t)
	rc, err := proxyClient.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	s := newTCPClientSocket(rc, nil)
	if s.closedBeforeFIN() {
		t.Fatal("an open connection said it had closed")
	}
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateCloseWait, "the closed client's FIN is in")
	if s.closedBeforeFIN() {
		t.Fatal("a connection with the client's FIN in said it had closed")
	}
	if _, err := proxyClient.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the answer")
	if !s.closedBeforeFIN() {
		t.Fatal("a connection the client reset did not say it had closed")
	}
	if err := proxyClient.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("the shutdown of a reset connection failed (%v): Windows' succeeds", err)
	}
	if l := s.look(); l.said != clientClosed || l.acked != 0 {
		t.Fatalf("the reset answer reads as %+v, want closed with none of it acknowledged", l)
	}
}

// On a real socket: a client that resets after its FIN, with no FIN of the
// proxy's sent, closes the connection with the error Linux's socket keeps in
// CLOSE_WAIT, EPIPE, and no other: the session keeps a pipe apart from a reset
// (supervisor's client_end_codes.go), so its parsers see which came. A client
// does so two ways: it closes and then resets the answer the proxy writes it,
// or it aborts its half-closed socket and its stack resets that answer.
// Windows sends no reset when a client aborts (SO_LINGER 0) a socket whose
// FIN is acknowledged: measured on Windows 11 over loopback, the proxy's end
// stayed in CLOSE_WAIT for 3 s and read EOF, whether the linger was set before
// or after the half-close, with a shutdown of both sides or a read pending;
// the client's stack resets the next segment the proxy sends instead. So the
// aborting client's reset is drawn by the answer, and until then the socket
// tells none.
func TestWindowsAClientsResetAfterItsFINIsEPIPE(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		reset func(t *testing.T, app, proxyClient net.Conn)
	}{
		{"aborted after its half-close, then sent the answer", func(t *testing.T, app, proxyClient net.Conn) {
			tc := app.(*net.TCPConn)
			if err := tc.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN is in")
			if err := tc.SetLinger(0); err != nil {
				t.Fatal(err)
			}
			_ = tc.Close()
			// A stack that did reset at the abort has closed the socket, and
			// the write may then fail; the reset is the same one.
			if _, err := proxyClient.Write([]byte("the answer, after the client's abort")); err != nil &&
				tcpState(t, proxyClient) != stateClosed {
				t.Fatal(err)
			}
		}},
		{"closed, then reset the proxy's answer", func(t *testing.T, app, proxyClient net.Conn) {
			_ = app.Close()
			awaitTCPState(t, proxyClient, stateCloseWait, "the closed client's FIN is in")
			if _, err := proxyClient.Write([]byte("the answer, after the client's FIN")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			rc, err := proxyClient.(*net.TCPConn).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			s := newTCPClientSocket(rc, nil)
			row.reset(t, app, proxyClient)
			awaitTCPState(t, proxyClient, stateClosed, "the client's reset is in")
			if err := s.pendingError(true); err != syscall.EPIPE {
				t.Fatalf("pendingError(true) = %v, want EPIPE exactly", err)
			}
		})
	}
}
