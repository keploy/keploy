package util

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.uber.org/zap"
)

// pausePair is a connected pair of TCP sockets on loopback.
func pausePair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	acc := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(acc)
			return
		}
		acc <- c
	}()
	client, err = net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-acc
	if server == nil {
		t.Fatal("no connection accepted")
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

// sawReset reports whether a read of c ends with a reset, not an end of
// stream, within a bound that only a peer never reset reaches.
func sawReset(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if err == nil || errors.Is(err, io.EOF) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the connection was neither reset nor closed")
	}
	return errors.Is(err, syscall.ECONNRESET)
}

// A registry tracks only while it is open; its reset ends every tracked
// passthrough with a reset on each of its sockets, forgets them, and refuses
// what asks after it, so nothing is tracked that no reset will reach.
func TestPausePassthroughsTrackWhileOpenAndResetAtTheResume(t *testing.T) {
	r := &PausePassthroughs{}
	a, aPeer := pausePair(t)
	if _, ok := r.Begin(a); ok {
		t.Fatal("a registry that was never opened tracked a connection")
	}
	r.Open()
	r.Open() // idempotent
	pc, ok := r.Begin(a)
	if !ok {
		t.Fatal("an open registry refused a connection")
	}
	b, bPeer := pausePair(t)
	if !pc.Add(b) {
		t.Fatal("a tracked passthrough refused its destination")
	}
	// One whose relay has ended is no longer tracked, nor reset.
	c, cPeer := pausePair(t)
	done, ok := r.Begin(c)
	if !ok {
		t.Fatal("an open registry refused a second connection")
	}
	done.Done()
	done.Done() // idempotent
	if r.Len() != 1 {
		t.Fatalf("the registry tracks %d passthroughs, want 1", r.Len())
	}

	if n := r.ResetAll(); n != 1 {
		t.Fatalf("the resume reset %d passthroughs, want 1", n)
	}
	if !sawReset(t, aPeer) || !sawReset(t, bPeer) {
		t.Fatal("a tracked passthrough's peer did not see a reset on both its sockets")
	}
	if r.Len() != 0 {
		t.Fatalf("the registry still tracks %d passthroughs after the resume", r.Len())
	}
	// The ended relay's connection was left alone.
	_ = cPeer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := cPeer.Read(make([]byte, 1)); err == nil || !errors.Is(err, errTimeout(err)) {
		t.Fatalf("a connection no longer tracked was ended by the resume: %v", err)
	}

	// After the resume: nothing is tracked, and a passthrough begun before
	// it refuses a socket dialled since.
	if _, ok := r.Begin(c); ok {
		t.Fatal("the registry tracked a connection after the resume")
	}
	d, _ := pausePair(t)
	if pc.Add(d) {
		t.Fatal("a passthrough the resume reset took another socket")
	}
	if n := r.ResetAll(); n != 0 {
		t.Fatalf("a second resume reset %d passthroughs, want none", n)
	}
	// The next pause tracks again.
	r.Open()
	if again, ok := r.Begin(c); !ok {
		t.Fatal("the registry refused a connection of the next pause")
	} else {
		again.Done()
	}
}

// errTimeout is err when it is a timeout, and a sentinel no error is
// otherwise.
func errTimeout(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return err
	}
	return errors.New("not a timeout")
}

// ResetConn finds the socket beneath the wrappers the proxy puts on its
// connections, a SafeConn whose own Close does nothing and a TLS layer among
// them, and resets it.
func TestResetConnResetsTheSocketBeneathTheWrappers(t *testing.T) {
	for name, wrap := range map[string]func(net.Conn) net.Conn{
		"a bare socket": func(c net.Conn) net.Conn { return c },
		"a SafeConn":    func(c net.Conn) net.Conn { return NewSafeConn(c, zap.NewNop()) },
		"a Conn over a SafeConn": func(c net.Conn) net.Conn {
			return &Conn{Conn: NewSafeConn(c, zap.NewNop()), Reader: c, Logger: zap.NewNop()}
		},
		"a SafeConn over TLS": func(c net.Conn) net.Conn {
			return NewSafeConn(tls.Client(c, &tls.Config{InsecureSkipVerify: true}), zap.NewNop()) //nolint:gosec // never handshakes
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, peer := pausePair(t)
			ResetConn(wrap(c))
			if !sawReset(t, peer) {
				t.Fatal("the peer saw an end of stream, or nothing, not a reset")
			}
		})
	}
	ResetConn(nil) // nil-safe
}

// A parser that finds recording paused gives its connection up to a raw relay
// under the memory guard's pause as it always did, untracked; under the
// control plane's only when the relay is tracked for the resume to reset; and
// when it cannot be tracked it keeps the connection, unless the agent is
// short of memory too.
func TestDemoteForPause(t *testing.T) {
	t.Cleanup(func() {
		memoryguard.SetExternalPause(false, "")
		DefaultPausePassthroughs.ResetAll()
	})
	a, _ := pausePair(t)
	b, bPeer := pausePair(t)

	endMemory := memoryguard.SetRecordingPausedForTest(true)
	demote, done := DemoteForPause(a, b)
	if !demote || DefaultPausePassthroughs.Len() != 0 {
		t.Fatalf("under a memory pause: demote=%v with %d tracked, want a demotion nothing tracks", demote, DefaultPausePassthroughs.Len())
	}
	done()
	endMemory()

	// The control plane's pause, with tracking not open: the resume is in
	// progress (or the pause was set with no proxy to track for it).
	memoryguard.SetExternalPause(true, "rollout")
	if demote, _ := DemoteForPause(a, b); demote {
		t.Fatal("a connection that cannot be tracked was given up to a raw relay for the control plane's pause: nothing would reset it")
	}
	// ... unless the agent is short of memory as well.
	endMemory = memoryguard.SetRecordingPausedForTest(true)
	if demote, _ := DemoteForPause(a, b); !demote {
		t.Fatal("a memory pause beside the control plane's did not demote the connection")
	}
	endMemory()

	DefaultPausePassthroughs.Open()
	demote, done = DemoteForPause(a, b)
	if !demote || DefaultPausePassthroughs.Len() != 1 {
		t.Fatalf("under the control plane's pause: demote=%v with %d tracked, want a tracked demotion", demote, DefaultPausePassthroughs.Len())
	}
	if n := DefaultPausePassthroughs.ResetAll(); n != 1 {
		t.Fatalf("the resume reset %d passthroughs, want the demoted one", n)
	}
	if !sawReset(t, bPeer) {
		t.Fatal("the demoted connection was not reset at the resume")
	}
	done()
}
