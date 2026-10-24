package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A connection can reach the proxy while the agent has no session: the
// connection hooks redirect from the moment they are loaded, and the session
// only exists once the client's Record (record) or Mock (test) call arrives.
// That gap is real, not theoretical — every agent start has it, and an agent
// restarted in place (a k8s sidecar whose container restarted, say) sits in it
// until its client re-arms it, with the application's pooled and retried
// connections arriving the whole time.
//
// Such a connection is never left open: the application would block on it
// until its own timeout. What happens to it is decided by what the agent knows
// before any session exists — the mode it was started in (--mode) and whether
// it was told to pass everything through (--global-passthrough):
//
//   - global passthrough, in any mode: the operator asked for every connection
//     to reach its destination, as the session path does with it set. The
//     connection is passed through.
//   - record: keploy is not recording yet, so the application must behave as
//     it would without keploy. The connection is passed through to its
//     original destination, byte for byte, and nothing is captured.
//   - test: the connection is closed without contacting its destination.
//     Replay never calls a live dependency, because a live answer would let a
//     test pass on behaviour the recording never showed; a closed connection
//     fails the call visibly instead.
//   - anything else (no --mode, or "off"): nothing says a live call is safe,
//     so the connection is closed, exactly as in test mode.
//
// A passthrough lasts only as long as the session would pass its connection
// through too. Its connection was decided on before there was a session, so
// everything it carries afterwards would escape the session — a pooled or
// long-lived connection (a database pool, a Redis or gRPC client, a Kafka
// consumer) would never be recorded, or would reach a live dependency in
// replay. So when Record or Mock has set the session up, every passthrough
// the session would not have made is reset at once, and the application's
// reconnection is handled under the session. One made because of global
// passthrough stays: the session passes connections through under it too,
// unless it intercepts TLS opportunistically, which takes precedence there.
//
// The cut is a reset, not a clean close, on both sides: a passthrough cut off
// mid-exchange carries a partial message, and an end of stream would tell an
// EOF-delimited peer — the live dependency among them — that it is complete.

// noSessionAction is what handleConnection does with a connection that
// arrives while the agent has no session.
type noSessionAction int

const (
	// noSessionClose closes the connection without dialling its destination.
	noSessionClose noSessionAction = iota
	// noSessionPassThrough relays the connection to its destination
	// unrecorded, until the session is set up.
	noSessionPassThrough
)

// noSessionActionFor decides by what the agent was started with. Only global
// passthrough or an agent started to record passes traffic through, so an
// agent whose mode is unknown never makes a live call in replay.
func noSessionActionFor(agentMode models.Mode, globalPassthrough bool) noSessionAction {
	if globalPassthrough || agentMode == models.MODE_RECORD {
		return noSessionPassThrough
	}
	return noSessionClose
}

// serveWithoutSession handles a connection that arrived while the agent has
// no session, per noSessionActionFor. The caller closes srcConn when this
// returns, on every path; this function closes what it dials.
func (p *Proxy) serveWithoutSession(ctx context.Context, srcConn net.Conn, dstAddr string, pre *util.Predialed) error {
	action := noSessionActionFor(p.agentMode, p.GlobalPassthrough)
	p.logNoSession(action, dstAddr)
	if action != noSessionPassThrough {
		return nil
	}

	dstConn, err := util.DialDestination(ctx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: pre})
	if err != nil {
		// %w keeps the dial's own error classifiable (refusedLocally) by the
		// accept loop, which logs it.
		return fmt.Errorf("no session: passing the connection through to %s: %w", dstAddr, err)
	}

	pt := &noSessionPassthrough{src: srcConn, dst: dstConn, dest: dstAddr, global: p.GlobalPassthrough}
	defer pt.end(false)
	if !p.noSessionPassthroughs.add(pt) {
		// The session was set up while the destination was being dialled,
		// and it would not pass this connection through: it belongs to the
		// session now, and the application's reconnection is handled under
		// it.
		p.logger.Debug("the session was set up while this connection's destination was being dialled; resetting it so the application reconnects under the session",
			zap.String("destination", dstAddr))
		pt.end(true)
		return nil
	}
	defer p.noSessionPassthroughs.remove(pt)

	// The relay does not watch ctx, and the proxy's shutdown expects its
	// connections to end once ctx is cancelled. A reset, as when the session
	// cuts a passthrough off: the exchange may be mid-message.
	stop := context.AfterFunc(ctx, func() { pt.end(true) })
	defer stop()

	pt.relay()
	return nil
}

// logNoSession logs a no-session connection at WARN the first time its
// destination is seen, and at Debug after that: an application retrying a
// call, or a pool refilling, reaches the same destination many times while
// the agent waits for its session, and one line per destination says all
// there is to say.
func (p *Proxy) logNoSession(action noSessionAction, dstAddr string) {
	log := p.logger.Debug
	if p.noSessionDests.first(dstAddr) {
		log = p.logger.Warn
	}
	fields := []zap.Field{
		zap.String("destination", dstAddr),
		zap.String("agent_mode", string(p.agentMode)),
		zap.Bool("global_passthrough", p.GlobalPassthrough),
	}
	switch {
	case action == noSessionPassThrough:
		log("keploy has no session yet, so this connection is passed through to its destination and nothing on it is recorded",
			append(fields, zap.String("next_step", "nothing to do: when the session starts keploy closes this connection, and the application's reconnection is recorded (or served from mocks) under the session"))...)
	case p.agentMode == models.MODE_TEST:
		log("keploy has no test session yet, so this connection is closed without contacting its destination: replay never calls a live dependency",
			append(fields, zap.String("next_step", "the application connected before the client set up the test session (for example, the agent restarted); the call fails instead of silently going live, and succeeds once the session is up"))...)
	default:
		log("keploy has no session and was not started in record or test mode, so this connection is closed without contacting its destination",
			append(fields, zap.String("next_step", "start the agent with --mode record or --mode test"))...)
	}
}

// endNoSessionPassthroughs resets every passthrough opened while there was no
// session that the session would not pass through, and refuses any new one:
// Record and Mock call it once the session is set up, so the applications
// reconnect, and their reconnections are handled under the session.
func (p *Proxy) endNoSessionPassthroughs(session *agent.Session) {
	// Under global passthrough the session path passes every connection
	// through as well (handleConnection), unless the session intercepts TLS
	// opportunistically, which it checks first.
	keepGlobal := !session.OutgoingOptions.OpportunisticTLSIntercept
	ended, kept := p.noSessionPassthroughs.sessionStarted(keepGlobal)
	if kept > 0 {
		p.logger.Debug("the session started; the connections passed through under global passthrough stay open, since the session passes connections through too",
			zap.Int("connections", kept))
	}
	if len(ended) == 0 {
		return
	}
	mode := session.Mode
	dests := make(map[string]int, len(ended))
	for _, pt := range ended {
		dests[pt.dest]++
	}
	names := make([]string, 0, len(dests))
	for d := range dests {
		names = append(names, d)
	}
	sort.Strings(names)
	const maxNamed = 20
	more := 0
	if len(names) > maxNamed {
		more = len(names) - maxNamed
		names = names[:maxNamed]
	}
	p.logger.Warn("the session started: reset the connections keploy had passed through unrecorded while it had no session, so their applications reconnect under the session",
		zap.String("session_mode", string(mode)),
		zap.Int("connections", len(ended)),
		zap.Strings("destinations", names),
		zap.Int("more_destinations", more),
		zap.String("next_step", "nothing to do: an application that pools its connections reconnects, and everything from then on is handled under the session; what those connections carried before was not recorded"))
}

// noSessionPassthrough is one connection relayed to its destination while the
// agent has no session.
type noSessionPassthrough struct {
	src, dst net.Conn
	dest     string
	// global is set when the agent passes everything through
	// (--global-passthrough): a session passes such a connection through too.
	global bool
	once   sync.Once
}

// end ends the passthrough, once, closing both connections. abort resets them
// instead of closing them cleanly: a direction that broke (a reset, a failed
// write) is passed on to the other side the way it would reach it without
// keploy in between. Whatever ends the passthrough first decides how.
func (pt *noSessionPassthrough) end(abort bool) {
	pt.once.Do(func() {
		if abort {
			util.ResetOnClose(pt.src)
			util.ResetOnClose(pt.dst)
		}
		_ = pt.src.Close()
		_ = pt.dst.Close()
	})
}

// relay copies both ways until both directions have ended, and returns once
// they have. Both connections are plain TCP, so each event is passed on as it
// happens, with nothing to wait out: a clean end of stream becomes a
// half-close of the other side, and the other direction carries on; a broken
// direction resets both sides at once. No timer is involved — the relay ends
// when the peers end it, when the session starts, or when the proxy shuts
// down.
func (pt *noSessionPassthrough) relay() {
	done := make(chan error, 2)
	cp := func(to, from net.Conn) {
		_, err := io.Copy(to, from)
		if err == nil {
			_ = util.CloseWriteIfPossible(to)
		}
		done <- err
	}
	go cp(pt.dst, pt.src)
	go cp(pt.src, pt.dst)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			pt.end(true)
		}
	}
}

// noSessionPassthroughs are the passthroughs opened while the agent had no
// session that are still open. The zero value is ready to use.
type noSessionPassthroughs struct {
	mu   sync.Mutex
	live map[*noSessionPassthrough]struct{}
	// started is set once a session has been set up, and keepGlobal says
	// whether the latest one passes through what global passthrough did.
	started    bool
	keepGlobal bool
}

// survivesLocked reports whether pt may stay open under the session set up
// last, if any. The caller holds mu.
func (s *noSessionPassthroughs) survivesLocked(pt *noSessionPassthrough) bool {
	return !s.started || (pt.global && s.keepGlobal)
}

// add registers pt, reporting false, without registering it, when the session
// set up meanwhile would not pass it through.
func (s *noSessionPassthroughs) add(pt *noSessionPassthrough) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.survivesLocked(pt) {
		return false
	}
	if s.live == nil {
		s.live = make(map[*noSessionPassthrough]struct{})
	}
	s.live[pt] = struct{}{}
	return true
}

func (s *noSessionPassthroughs) remove(pt *noSessionPassthrough) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, pt)
}

// sessionStarted records that a session has been set up, keepGlobal saying
// whether it passes through what global passthrough did. It resets every open
// passthrough that does not survive it, refuses such passthroughs from now
// on, and returns the ones it reset and how many stay open.
func (s *noSessionPassthroughs) sessionStarted(keepGlobal bool) (ended []*noSessionPassthrough, kept int) {
	s.mu.Lock()
	s.started = true
	s.keepGlobal = keepGlobal
	for pt := range s.live {
		if s.survivesLocked(pt) {
			kept++
			continue
		}
		ended = append(ended, pt)
		delete(s.live, pt)
	}
	s.mu.Unlock()
	for _, pt := range ended {
		pt.end(true)
	}
	return ended, kept
}

// noSessionDests remembers which destinations a no-session connection has
// already been logged for. The zero value is ready to use. It is bounded:
// once full it starts over, so a destination may be logged again after a
// thousand others, and the memory an agent that never gets a session can
// spend on it stays fixed.
type noSessionDests struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// noSessionDestsMax bounds noSessionDests.
const noSessionDestsMax = 1024

// first reports whether dest has not been seen before, remembering it.
func (d *noSessionDests) first(dest string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[dest]; ok {
		return false
	}
	if d.seen == nil || len(d.seen) >= noSessionDestsMax {
		d.seen = make(map[string]struct{})
	}
	d.seen[dest] = struct{}{}
	return true
}
