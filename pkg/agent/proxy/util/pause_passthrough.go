package util

import (
	"net"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
)

// PausePassthroughs are the connections relayed unread because the agent's
// control plane had paused recording (memoryguard.SetExternalPause): the new
// connections the egress proxy and the ingress passed through without reading
// them, and the connections a record parser owned and gave up to a raw relay
// for the pause.
//
// None of them can be recorded again: nothing puts a parser back on a
// connection that was relayed unread. So each is tracked here, and the resume
// resets them all (ResetAll), as the start of a session resets what was
// passed through before it (the proxy's noSessionPassthroughs): the
// application, or the application's client, reconnects, and the new
// connection is recorded. Left open, a pooled connection would stay
// unrecorded for the rest of its life, and every test case that used it would
// be saved without its mocks.
//
// It tracks only while it is open (Open, at the pause's start) and until the
// resume's ResetAll. A connection that asks in between the two edges is
// refused (Begin), and its caller records it as it would any other: nothing
// tracked after the reset could be reset by it.
type PausePassthroughs struct {
	mu        sync.Mutex
	accepting bool
	live      map[*PausedConn]struct{}
}

// DefaultPausePassthroughs is the process's registry: the egress proxy, the
// record parsers and the ingress all track in it, and the proxy's
// PauseRecording and ResumeRecording open and reset it.
var DefaultPausePassthroughs = &PausePassthroughs{}

// PausedConn is one tracked passthrough: the sockets of one relayed
// connection, both sides of it once its destination is dialled.
type PausedConn struct {
	reg   *PausePassthroughs
	conns []net.Conn
	ended bool
}

// Open starts tracking: the control plane's pause has begun. Idempotent.
func (r *PausePassthroughs) Open() {
	r.mu.Lock()
	r.accepting = true
	r.mu.Unlock()
}

// Begin tracks a passthrough of conns, and reports false, tracking nothing,
// unless the registry is open: the caller then handles the connection as if
// recording were not paused for the control plane. The caller calls Done when
// its relay ends.
func (r *PausePassthroughs) Begin(conns ...net.Conn) (*PausedConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accepting {
		return nil, false
	}
	pc := &PausedConn{reg: r, conns: conns}
	if r.live == nil {
		r.live = make(map[*PausedConn]struct{})
	}
	r.live[pc] = struct{}{}
	return pc, true
}

// Add tracks one more socket of the passthrough (the destination, once it is
// dialled). It reports false when the passthrough was reset meanwhile: the
// resume came while the destination was being dialled. The caller then resets
// conn itself (ResetConn), since its other side is gone.
func (pc *PausedConn) Add(conn net.Conn) bool {
	pc.reg.mu.Lock()
	defer pc.reg.mu.Unlock()
	if pc.ended {
		return false
	}
	pc.conns = append(pc.conns, conn)
	return true
}

// Done stops tracking the passthrough: its relay has ended. Idempotent and
// nil-safe.
func (pc *PausedConn) Done() {
	if pc == nil {
		return
	}
	pc.reg.mu.Lock()
	defer pc.reg.mu.Unlock()
	delete(pc.reg.live, pc)
}

// Len is how many passthroughs are tracked.
func (r *PausePassthroughs) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// ResetAll stops tracking and resets every tracked passthrough, both sides of
// each (ResetConn): the control plane's pause is ending. It returns how many
// it reset. A reset, not a clean close: a passthrough cut mid-exchange carries
// a partial message, and an end of stream would tell an EOF-delimited peer
// that the message is complete.
func (r *PausePassthroughs) ResetAll() int {
	r.mu.Lock()
	r.accepting = false
	ended := make([]*PausedConn, 0, len(r.live))
	var conns []net.Conn
	for pc := range r.live {
		pc.ended = true
		ended = append(ended, pc)
		conns = append(conns, pc.conns...)
		delete(r.live, pc)
	}
	r.mu.Unlock()
	for _, c := range conns {
		ResetConn(c)
	}
	return len(ended)
}

// DemoteForPause is what a record parser asks when it finds recording paused
// (memoryguard.IsRecordingPaused) and would give its connection up to a raw
// relay for the rest of the connection's life: whether to, and what to call
// when that relay ends.
//
// Under the memory guard's pause it says yes, as the parsers always did. Under
// a pause the control plane asked for it says yes only if the connection can
// be tracked (DefaultPausePassthroughs), so that the resume resets it and the
// application reconnects under capture. When it cannot be (the resume is in
// progress), the parser keeps the connection and goes on parsing: the mocks it
// makes of requests within the pause are dropped by the mock manager, and the
// connection is still recorded after it.
func DemoteForPause(client, dest net.Conn) (demote bool, done func()) {
	if !memoryguard.ExternalPausedNow() {
		return true, func() {}
	}
	if pc, ok := DefaultPausePassthroughs.Begin(client, dest); ok {
		return true, pc.Done
	}
	return memoryguard.PausedForMemory(), func() {}
}

// ResetConn closes c so that its peer sees a reset, not an end of stream
// (SO_LINGER 0), looking beneath the wrappers the proxy puts on its
// connections for the socket: a wrapper's own Close may do nothing (SafeConn)
// or write first (TLS's close_notify). A connection with no socket beneath it
// is closed as it is. Nil-safe.
func ResetConn(c net.Conn) {
	if c == nil {
		return
	}
	top := c
	for i := 0; i < maxHalfCloseDepth && c != nil; i++ { // bounded: a cycle must not hang
		switch v := c.(type) {
		case interface{ SetLinger(sec int) error }:
			_ = v.SetLinger(0)
			_ = c.Close()
			return
		case *connseq.Upstream:
			c = v.Conn
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		case interface{ Unwrap() net.Conn }:
			c = v.Unwrap()
		default:
			_ = top.Close()
			return
		}
	}
	_ = top.Close()
}
