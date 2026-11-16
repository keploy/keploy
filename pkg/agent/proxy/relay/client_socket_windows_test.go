//go:build windows

package relay

import (
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

func (winScriptFD) Control(f func(uintptr)) error  { f(0); return nil }
func (winScriptFD) Read(func(uintptr) bool) error  { return nil }
func (winScriptFD) Write(func(uintptr) bool) error { return nil }

// winFailingFD is a socket whose Control fails, as a closed one's does.
type winFailingFD struct{}

func (winFailingFD) Control(func(uintptr)) error    { return syscall.EINVAL }
func (winFailingFD) Read(func(uintptr) bool) error  { return syscall.EINVAL }
func (winFailingFD) Write(func(uintptr) bool) error { return syscall.EINVAL }

// winAnsweringSocket is a client socket whose SIO_TCP_INFO answers ans, in full.
func winAnsweringSocket(ans func() []byte) *tcpClientSocket {
	s := &tcpClientSocket{rc: winScriptFD{}, fallback: &infoFallback{}}
	s.getInfo = func(_ uintptr, buf *[wtcpInfoSize]byte) (int, error) {
		return copy(buf[:], ans()), nil
	}
	s.ask = s.lookAt
	return s
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
			if s.finBeforeReset() {
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
		if got := s.finBeforeReset(); got != row.want {
			t.Errorf("state %d: finBeforeReset = %v, want %v", row.state, got, row.want)
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
