package relay

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// errorSocket is a scripted socket that holds err (SO_ERROR) once a look has
// found the connection closed, or from the start when always is set. Reading
// it takes it, as the kernel's does.
type errorSocket struct {
	scriptedSocket
	emu      sync.Mutex
	err      error
	always   bool
	closed   bool
	errAsked int
}

func (s *errorSocket) look() clientLook {
	l := s.scriptedSocket.look()
	if l.said == clientClosed {
		s.emu.Lock()
		s.closed = true
		s.emu.Unlock()
	}
	return l
}

func (s *errorSocket) pendingError() error {
	s.emu.Lock()
	defer s.emu.Unlock()
	s.errAsked++
	if !s.closed && !s.always {
		return nil
	}
	err := s.err
	s.err = nil
	return err
}

// clientEndOf runs clientEnd on a relay whose client socket is sock, and
// returns what it told Config.ClientEndError. It fails the test if clientEnd
// has not returned within 5s: with HalfCloseGrace an hour, only a wait that
// the socket's answer ended returns by then.
func clientEndOf(t *testing.T, sock clientSocket, finRead bool, writeErr error) []error {
	t.Helper()
	var mu sync.Mutex
	var told []error
	r := &Relay{cfg: Config{HalfCloseGrace: time.Hour, ClientEndError: func(err error) {
		mu.Lock()
		told = append(told, err)
		mu.Unlock()
	}}}
	r.client = sock
	r.clientEOF.Store(finRead)
	if writeErr != nil {
		r.noteClientWriteFailed(fakeconn.FromDest, writeErr)
	}
	done := make(chan struct{})
	go func() { defer close(done); r.clientEnd(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("clientEnd still waiting after 5s, with the client's answer in")
	}
	mu.Lock()
	defer mu.Unlock()
	return told
}

// What the relay hands on as it ends, for each answer a client's socket can
// give after the relay read the client's FIN, and before it.
func TestTheRelayEndsTellingWhatTheClientsSocketHolds(t *testing.T) {
	t.Parallel()
	reset := syscall.ECONNRESET
	for _, tc := range []struct {
		name     string
		script   []clientLook
		wrote    uint64
		blind    bool
		always   bool
		finRead  bool
		writeErr error
		want     error
		minLooks int
		maxLooks int
	}{
		{
			// The closed client resets the last chunk one round trip after
			// its Write: the relay waits for it, then reads the reset.
			name: "the FIN read, a reset after it", script: []clientLook{finned(0), finned(0), closed(0)},
			wrote: 10, finRead: true, want: reset, minLooks: 3, maxLooks: 3,
		},
		{
			// A half-closed client acknowledges every byte: nothing to read.
			name: "the FIN read, every byte acknowledged", script: []clientLook{finned(4), finned(10)},
			wrote: 10, finRead: true, minLooks: 2, maxLooks: 2,
		},
		{
			// Acknowledged past what was written (a FIN of the proxy's own
			// counts one): answered.
			name: "the FIN read, acknowledged past what was written", script: []clientLook{finned(11)},
			wrote: 10, finRead: true, minLooks: 1, maxLooks: 1,
		},
		{
			name: "the FIN read, a socket that cannot say", script: []clientLook{unknown},
			wrote: 10, finRead: true, minLooks: 1, maxLooks: 1,
		},
		{
			name: "the FIN read, a socket that cannot count what was written", script: []clientLook{finned(0)},
			wrote: 10, blind: true, finRead: true, minLooks: 0, maxLooks: 1,
		},
		{
			// No FIN read: the socket is read as it is, with no wait.
			name: "no FIN read, a reset held", script: []clientLook{finned(0)},
			wrote: 10, always: true, want: reset, maxLooks: 0,
		},
		{
			name: "no FIN read, nothing held", script: []clientLook{finned(0)},
			wrote: 10, maxLooks: 0,
		},
		{
			// A failed write took the error off the socket itself: its own
			// error is handed on, and the socket is not asked.
			name: "a failed write", script: []clientLook{closed(0)},
			wrote: 10, always: true, finRead: true, writeErr: syscall.EPIPE, want: syscall.EPIPE, maxLooks: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sock := &errorSocket{scriptedSocket: scriptedSocket{script: tc.script, wrote: tc.wrote, blind: tc.blind}, err: reset, always: tc.always}
			told := clientEndOf(t, sock, tc.finRead, tc.writeErr)
			switch {
			case tc.want == nil && len(told) != 0:
				t.Fatalf("told %v, want nothing", told)
			case tc.want != nil && (len(told) != 1 || !errors.Is(told[0], tc.want)):
				t.Fatalf("told %v, want only %v", told, tc.want)
			}
			if n := sock.looks(); n < tc.minLooks || n > tc.maxLooks {
				t.Fatalf("%d looks, want %d to %d", n, tc.minLooks, tc.maxLooks)
			}
			if tc.writeErr != nil && sock.errAsked != 0 {
				t.Fatal("the socket was asked for its error after a failed write took it")
			}
		})
	}
}

// Only a failed write to the client is the client's: one to the destination
// is not handed on, and the client's socket is asked as before.
func TestAFailedWriteToTheDestinationIsNotTheClients(t *testing.T) {
	t.Parallel()
	var told []error
	r := &Relay{cfg: Config{ClientEndError: func(err error) { told = append(told, err) }}}
	r.noteClientWriteFailed(fakeconn.FromClient, syscall.EPIPE)
	r.noteClientWriteFailed(fakeconn.FromDest, nil)
	r.client = &errorSocket{scriptedSocket: scriptedSocket{script: []clientLook{open(0)}}}
	r.clientEnd(context.Background())
	if len(told) != 0 {
		t.Fatalf("told %v, want nothing", told)
	}
	r.noteClientWriteFailed(fakeconn.FromDest, syscall.ECONNRESET)
	r.noteClientWriteFailed(fakeconn.FromDest, syscall.EPIPE)
	r.clientEnd(context.Background())
	if len(told) != 1 || !errors.Is(told[0], syscall.ECONNRESET) {
		t.Fatalf("told %v, want the first failed write's ECONNRESET", told)
	}
}

// A client that answers neither way is waited for HalfCloseGrace, and then
// its socket is read as it is.
func TestAClientThatNeverAnswersIsWaitedForTheGraceAlone(t *testing.T) {
	t.Parallel()
	var told []error
	r := &Relay{cfg: Config{HalfCloseGrace: 20 * time.Millisecond, ClientEndError: func(err error) { told = append(told, err) }}}
	sock := &errorSocket{scriptedSocket: scriptedSocket{script: []clientLook{finned(0)}, wrote: 10}, err: syscall.ECONNRESET, always: true}
	r.client = sock
	r.clientEOF.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); r.clientEnd(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("clientEnd still waiting after 5s, with a 20ms grace")
	}
	if len(told) != 1 || !errors.Is(told[0], syscall.ECONNRESET) || sock.looks() < 2 {
		t.Fatalf("told %v after %d looks, want the socket's ECONNRESET after more than one look", told, sock.looks())
	}
}

// The recording's stop ends the wait for a client that has not answered.
func TestTheRecordingsStopEndsTheWaitForTheClientsAnswer(t *testing.T) {
	t.Parallel()
	r := &Relay{cfg: Config{HalfCloseGrace: time.Hour, ClientEndError: func(error) {}}}
	r.client = &errorSocket{scriptedSocket: scriptedSocket{script: []clientLook{finned(0)}, wrote: 10}}
	r.clientEOF.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.clientEnd(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("clientEnd still waiting after 5s, with the recording stopped")
	}
}
