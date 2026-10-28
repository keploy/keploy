package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// CaptureLosses counts, by kind, the calls the proxy intercepted and could not
// record or mock since it started: a client-side TLS handshake that failed
// other than by the client giving it up, and one it abandoned silently once
// keploy's certificate was out (clientTLS), a TLS upgrade it could not make. Each only ever grows.
//
// A read decides every TLS hang-up held for a call of its process
// (caTrust.hangUp): none was cancelled by one before it, so each is charged
// now. The agent reads the counts as a test ends, after the test's calls
// reached the proxy (and at a test process's claim, the run's start and its
// end), so a hang-up is decided at the end of the test it happened in, by
// the order of events, not by a timer.
func (p *Proxy) CaptureLosses() models.CaptureLosses {
	for _, h := range p.caTrust.settle() {
		p.losses.Add(models.CaptureLossTLSAbandoned, "to "+h.dst)
	}
	return p.losses.Snapshot()
}

// Compile-time proof that the proxy reports its capture losses: were the
// method renamed, the agent's type assertion would stop matching and those
// losses would go unreported.
var _ agent.CaptureLossReporter = (*Proxy)(nil)

// countUnreadContainers counts each use of a connection whose container the
// keyer cannot name, the Docker daemon not having answered the read of its
// configuration (containerkey's Keyer.OnUnread), as a capture loss, which
// leaves the test it is charged to not judged
// (models.CaptureLossContainerUnread). bindContainerKeys calls it, under the
// proven switch only: without it nothing reads containers.
func (p *Proxy) countUnreadContainers() {
	containerkey.Default.OnUnread(func(what string) {
		p.losses.Add(models.CaptureLossContainerUnread, what)
	})
}

// teeDropLoss is the capture loss a relay tee's drop reason is, if any. A
// parser that stopped taking what was captured for it, whose queue was then
// abandoned (W8), and bytes dropped for want of room lose what they carried.
// A drop while the direction was paused (the parser asked for it, or a TLS
// upgrade was in flight) loses nothing the parser wanted, and a drop after a
// desync is the consequence of one already counted.
func teeDropLoss(reason string) (models.CaptureLossKind, bool) {
	switch reason {
	case relay.DropConsumerGone:
		return models.CaptureLossParserStall, true
	case relay.DropPerConnCap:
		return models.CaptureLossDropped, true
	}
	return "", false
}

// teeDropped wraps a relay's drop hook (the session's MarkMockIncomplete) so
// that a drop that loses captured bytes is also counted as a capture loss of
// the connection to dst, which every test that ends after it is charged with
// under the proven switch. Not once the recording is stopping (stopping): a
// stop retires the parsers, and what they still had queued is abandoned as
// the stop's, not a test's loss -- as a TLS handshake a stopping agent cut
// short is not counted either.
func (p *Proxy) teeDropped(mark func(reason string), dst func() string, stopping func() bool) func(reason string) {
	return func(reason string) {
		if mark != nil {
			mark(reason)
		}
		if stopping != nil && stopping() {
			return
		}
		if kind, ok := teeDropLoss(reason); ok {
			p.losses.Add(kind, reason+" on a connection to "+dst())
		}
	}
}

// clientTLS is one client-side TLS handshake keploy's proxy makes, whatever
// path makes it (handleConnection, the opportunistic upgrade, the relay's,
// the MySQL replayer's, a record upgrader's): the pTls.ClientEvents its
// context carries (withClientTLS), which charge what it lost to its
// destination, one way for every path.
//
// A handshake that failed lost its call when the client did not give it up:
// it rejected the certificate keploy answered with (it does not trust
// keploy's CA), keploy refused its hello, or keploy could not make its
// certificate -- tls_handshake_failed, the test's it happened in under the
// proven switch. A client that gave the handshake up (pTls.IsClientGiveUp)
// once keploy had sent its certificate (pTls.CertificateSent), or that hung
// up as soon as the handshake was done having sent nothing (SilentHangUp),
// abandoned it: it may have rejected the certificate without a word --
// Node.js, curl and libcurl do -- or given up, and nothing on the wire tells
// the two apart, so its test is not judged (tls_handshake_abandoned). One that
// gave up before keploy answered rejected nothing, and one keploy's stop, or
// keploy itself, cut short is no loss of a test's.
//
// An abandon is charged only when the handshake and the hang-up fall within
// one scope -- a test, or the time outside tests (the run's start, the gap
// between two tests, after the last one) -- never across a boundary: the
// hang-up, the handshake's own or a silent one after it, in the scope the
// handshake began in. Each scope has an id no other has had, so a hang-up in
// it has the handshake's beginning, its end, and everything between in it.
// A Go client's http.Transport keeps the connection it dialled for a request
// another connection served first -- a spare, its handshake done, nothing
// sent -- and the application's stop closes it with no close_notify, after
// the test it was dialled in: it lost nothing, and it is not charged. A
// client that rejects keploy's certificate does so as its call is made, in
// the scope that makes it, wherever that is. The limit: a spare dialled in
// the scope the application's stop closes it in -- after the last test, or
// in keploy record, whose tests the proxy does not see -- is charged, and
// reads as not verified (or, in keploy record, a WARN), the safe direction.
//
// The scope a hang-up is told in is the one it happened in, whatever the
// load: a test's end waits, by events, for a hang-up its connection's socket
// shows and its handler has not yet told (pTls.HangUpHolder), before it reads
// what the test lost and closes its scope. A handler the host had not run
// since the client hung up used to be outrun by the end, and dropped the
// abandon as one after the test.
//
// Under the proven switch, a client process that sent a call through
// keploy's certificate in this run (caTrust) trusts keploy's CA: it gave a
// handshake up, or hung up silent after one, of its own accord, as a browser
// does with the connections it opens ahead of a call and with those of a page
// it closes, and its hang-up is a cancel, not a loss. Only its hang-ups: a
// handshake it failed in another way (tls_handshake_failed) is charged as
// before. A hang-up before its first such call is held, not charged, until
// the counts are next read (CaptureLosses), which the agent does as the test
// it happened in ends: a call of the same process later in that test (in
// the scope the hang-up was in) makes it a cancel too -- a browser opens
// connections ahead of a page's first call and drops the ones it does not
// use, the first of the run among them -- and the read charges every
// hang-up still held.
type clientTLS struct {
	p   *Proxy
	ctx context.Context
	dst string
	// tgid is the client's process (its kernel tgid, the eBPF redirect's);
	// 0 when unknown, which no call ever makes trusted.
	tgid uint32

	mu sync.Mutex
	// began is the scope the handshake began in.
	began testScope
}

// testScope is the scope the proxy sees in effect: the Universal Mocker's
// (containerkey's: its open test, or the run's outside tests) and keploy
// test's or a replay's (the running test's misses window, or the gap after
// one), each by an id no other scope has had.
type testScope struct {
	keyer, windows uint64
}

// testScopeNow is the scope in effect now.
func (p *Proxy) testScopeNow() testScope {
	return testScope{keyer: containerkey.Default.Scope(), windows: p.testWindows.Load()}
}

// inScope says the scope in effect now is the one c's handshake began in.
func (c *clientTLS) inScope() bool {
	c.mu.Lock()
	began := c.began
	c.mu.Unlock()
	return c.p.testScopeNow() == began
}

// withClientTLS is ctx carrying the events of the client-side handshakes made
// under it by the process tgid (0: unknown), charged to dst (clientTLS).
func (p *Proxy) withClientTLS(ctx context.Context, dst string, tgid uint32) context.Context {
	return pTls.WithClientEvents(ctx, &clientTLS{p: p, ctx: ctx, dst: dst, tgid: tgid})
}

// caTrust is the client processes, by kernel tgid, that sent a call through
// keploy's TLS server in this run, and the hang-ups of those that had not
// yet. Each sent an application byte the server decrypted, so it accepted
// keploy's certificate against the trust store keploy put its CA in for the
// whole run. A client that does not trust it sends none: it fails the
// handshake, or hangs up silent once it is done (Node.js, curl, libcurl).
// The set only grows. The limits: a process that checks some connections
// against a store of its own without keploy's CA (a Node.js agent given its
// own ca) and rejects one silently after a call on another, or before one in
// the same test; and a tgid the kernel gives again, within the run, to a
// process that does not trust keploy's CA. Their silent hang-ups are
// cancels.
type caTrust struct {
	mu sync.Mutex
	// trusted is the processes that sent a call through keploy's
	// certificate in this run.
	trusted map[uint32]struct{}
	// held is the hang-ups of processes not trusted when they hung up,
	// each waiting for a call of its process in its scope (a cancel) or
	// for the next read of the counts (a loss: settle).
	held []heldHangUp
}

// heldHangUp is a hang-up caTrust holds: by the process tgid, in the scope
// its handshake began and ended in, on a connection to dst.
type heldHangUp struct {
	tgid  uint32
	scope testScope
	dst   string
}

// hangUpVerdict is what caTrust.hangUp makes of a hang-up.
type hangUpVerdict int

const (
	// hangUpCharged is a loss, charged now.
	hangUpCharged hangUpVerdict = iota
	// hangUpCancelled is a cancel: its process trusts keploy's CA.
	hangUpCancelled
	// hangUpHeld waits for a call of its process in its scope, or for
	// the next read of the counts.
	hangUpHeld
)

// hangUp is a hang-up by the process tgid in scope, on a connection to dst,
// under the proven switch: a cancel when the process sent a call through
// keploy's certificate in this run; held when it has not, until a call of
// the process in the same scope cancels it (noteCall) or the next read of
// the counts charges it (settle). It is charged now when the process is
// unknown (0: no process, which no call makes trusted), and without the
// switch, as before, whatever the process sent.
func (t *caTrust) hangUp(tgid uint32, scope testScope, dst string) hangUpVerdict {
	if tgid == 0 || !provenswitch.On() {
		return hangUpCharged
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.trusted[tgid]; ok {
		return hangUpCancelled
	}
	t.held = append(t.held, heldHangUp{tgid: tgid, scope: scope, dst: dst})
	return hangUpHeld
}

// noteCall marks tgid as a process that sent a call through keploy's
// certificate, in scope, and returns the hang-ups of that process held in
// that scope, which the call makes cancels. One held in another scope stays
// held: a hang-up in one test is not cancelled by a call in the next.
// (hangUp never asks after 0, no process.)
func (t *caTrust) noteCall(tgid uint32, scope testScope) (cancelled []heldHangUp) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trusted == nil {
		t.trusted = map[uint32]struct{}{}
	}
	t.trusted[tgid] = struct{}{}
	var kept []heldHangUp
	for _, h := range t.held {
		if h.tgid == tgid && h.scope == scope {
			cancelled = append(cancelled, h)
		} else {
			kept = append(kept, h)
		}
	}
	t.held = kept
	return cancelled
}

// settle takes every hang-up held: no call of its process in its scope came
// before this read, so each is a loss.
func (t *caTrust) settle() []heldHangUp {
	t.mu.Lock()
	defer t.mu.Unlock()
	held := t.held
	t.held = nil
	return held
}

// noteTLSCall is a call the process tgid sent through keploy's certificate:
// the process trusts keploy's CA from here, and its hang-ups held in the
// scope in effect are cancels (caTrust.noteCall).
func (p *Proxy) noteTLSCall(tgid uint32) {
	for _, h := range p.caTrust.noteCall(tgid, p.testScopeNow()) {
		p.logger.Debug("a client process sent a call through keploy's certificate after hanging up in the same test: that hang-up is a cancel, not a capture loss",
			zap.Uint32("tgid", h.tgid), zap.String("destination", h.dst))
	}
}

// hungUp is c's client hanging up (how) in the scope its handshake began
// in: charged, a cancel, or held for a call of its process (caTrust.hangUp).
func (c *clientTLS) hungUp(how string) {
	c.mu.Lock()
	began := c.began
	c.mu.Unlock()
	switch c.p.caTrust.hangUp(c.tgid, began, c.dst) {
	case hangUpCancelled:
		c.p.logger.Debug("a client process that sent a call through keploy's certificate in this run "+how+": a cancel, not a capture loss",
			zap.Uint32("tgid", c.tgid), zap.String("destination", c.dst))
	case hangUpHeld:
		c.p.logger.Debug("a client process that has not sent a call through keploy's certificate yet "+how+": a cancel if it sends one in this test, a capture loss otherwise",
			zap.Uint32("tgid", c.tgid), zap.String("destination", c.dst))
	default:
		c.p.losses.Add(models.CaptureLossTLSAbandoned, "to "+c.dst)
	}
}

func (c *clientTLS) HandshakeBegin() {
	now := c.p.testScopeNow()
	c.mu.Lock()
	c.began = now
	c.mu.Unlock()
}

func (c *clientTLS) HandshakeFailed(err error) {
	switch {
	case err == nil || c.ctx.Err() != nil:
	case errors.Is(err, net.ErrClosed):
		// keploy closed its own end: the client gave nothing up.
	case !pTls.IsClientGiveUp(err):
		c.p.losses.Add(models.CaptureLossTLSAbort, "to "+c.dst)
	case pTls.CertificateSent(err) && c.inScope():
		c.hungUp("gave a TLS handshake up")
	}
}

func (c *clientTLS) SilentHangUp(err error) {
	if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) || !pTls.IsClientGiveUp(err) || !c.inScope() {
		return
	}
	c.hungUp("hung up silent after its TLS handshake")
}

// clientTLSHandler is pTls.HandleTLSConnection for a parser's TLS upgrader
// on the record path (util.ConnTLSUpgrader: MySQL's SSLRequest, Postgres's)
// on a connection to dstAddr from the process tgid: its handshake is charged
// as every client-side handshake is (clientTLS), and its own error
// (pTls.HandshakeCause) is returned, which the parsers above it tell a
// client's EOF apart by identity with.
func (p *Proxy) clientTLSHandler(dstAddr string, tgid uint32) util.ClientTLSHandlerFunc {
	return func(ctx context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
		c, isMTLS, err := pTls.HandleTLSConnection(p.withClientTLS(ctx, dstAddr, tgid), logger, conn, backdate)
		return c, isMTLS, pTls.HandshakeCause(err)
	}
}

// relayClientHandshake is the relay's OnClientHandshakeFailed for a
// connection to dst: a client-side TLS upgrade a parser asked for that failed
// in its handshake is no upgrade keploy could not make (starttls_failed, a
// taint of every later test). keploy's TLS server already told the relay's
// events of it (withClientTLS, on the relay's context), which charged it as
// every client-side handshake is.
func (p *Proxy) relayClientHandshake(dst net.Conn) func(context.Context, error) {
	return func(_ context.Context, err error) {
		p.logger.Debug("a client-side TLS upgrade failed in its handshake; charged as a client-side handshake, not as an upgrade keploy could not make",
			zap.String("destination", remoteOf(dst)), zap.Error(err))
	}
}

// remoteOf is c's remote address, or "" for no connection.
func remoteOf(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return ""
	}
	return c.RemoteAddr().String()
}

// countParserPanic counts, as a capture loss, a parser that panicked while it
// served a connection to dst (util.ErrParserPanicked, which the parser's
// goroutine returns through util.RecoverInto): the call it was serving was
// neither answered from a mock nor recorded. That is keploy's failure, not a
// call the application made that has no recording (a miss), so it is not
// filed as one: the read of the run's misses leaves out a parser's error
// that is no miss. As a parser that stalled, it fails, under the proven
// switch, the test open when it happened and every later one
// (CAPTURE_INCOMPLETE). Before, the panicked goroutine sent nothing, its
// caller read its result channel's close as a clean end, and a test whose
// call was never answered could pass.
//
// dst is the destination's address, as a string or a net.Addr, read only for
// a panic.
func (p *Proxy) countParserPanic(err error, parser integrations.IntegrationType, dst any) {
	if errors.Is(err, util.ErrParserPanicked) {
		p.losses.Add(models.CaptureLossParserPanic, "the "+string(parser)+" parser, on a connection to "+fmt.Sprint(dst))
	}
}
