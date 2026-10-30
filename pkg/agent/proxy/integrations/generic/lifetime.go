package generic

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// exchangeLifetime is the lifetime the Generic recorder states
// (models.MetadataLifetimeKey) for a connection's n-th exchange, from 0. The
// recorder cannot read the protocol, so it takes the connection's first
// exchange for its preamble (a greeting, a handshake, an auth step): session,
// every test may be served it, as every Generic exchange was before. Every
// later exchange is the data of the test it was made in: per-test, owned by
// that test and judged under the proven switch (models.Mock.IsProtocolPhase).
// A handshake longer than one exchange has its later steps owned by the test
// that opened the connection.
//
// The type tag stays "config", and serving does not read the statement
// (models.Mock.DeriveLifetime keeps every Generic mock session): with the
// switch off a recording replays as it did before the recorder stated it.
//
// A connection whose whole life lies inside one test's window is that test's
// whole: its first exchange too (a publisher that connects per message, a
// connection-per-request protocol). So under the proven switch the recorder
// holds a connection's first exchange while the test that owns its request may
// own the whole connection, and states it by recorded times (preambleHold).
func exchangeLifetime(n int) string {
	if n == 0 {
		return models.MetadataLifetimeSession
	}
	return models.MetadataLifetimePerTest
}

// notOwnedExchange is, under exact serving (the proven switch), the name of
// the recorded exchange this request is, byte for byte, when the open view
// does not show it and its recorder stated it per-test: a later exchange of a
// connection recorded in another test's window, or between tests, or the
// first exchange of a connection that lay wholly inside another test. The
// likely cause of the miss is that at replay the call was made on another
// connection, or in another test, than when recording. Only an exchange that
// is this request is named: keploy record's judging bakes what a miss names
// into the set's worker scope, and an exchange merely like it (the per-test
// similarity) is another call. "" when it is none, or exact serving is off.
//
// The store is asked of call, the round the request's first read found it
// made in (integrations.CallRound, marked before the matcher read the
// store): an exchange that read's view held as its own is never named (the
// replay ended the call's test under it), nor is any once the replay staged
// another set, which restaged then reports.
func notOwnedExchange(reqBuffs [][]byte, mockDb integrations.MockMemDb, call integrations.CallRound) (name string, restaged bool) {
	sf, ok := mockDb.(integrations.StagedFinder)
	if !ok {
		return "", false
	}
	return sf.FindStagedNotOwned(models.GENERIC, call, func(mk *models.Mock) bool {
		return mk.Spec.Metadata[models.MetadataLifetimeKey] == models.MetadataLifetimePerTest &&
			findExactMatch([]*models.Mock{mk}, reqBuffs, true) == 0
	})
}

// restagedExchangeNote is what a miss says when the replay staged a test set
// again while it decided the request: the matcher, held to the request's
// first read (integrations.HoldToRound), found nothing to answer it with, or
// the lookup of an exchange recorded elsewhere named none.
const restagedExchangeNote = "keploy replay staged a test set again while it decided this request, so keploy answered it with no recorded exchange and names none made elsewhere for it: a request is never answered with, nor named as, an exchange of a staging made after it began."

// notOwnedExchangeNextSteps says what a miss on an exchange the test does not
// own (notOwnedExchange) likely means, and what to do.
func notOwnedExchangeNextSteps(name string) string {
	return fmt.Sprintf("This request is %s, which keploy recorded where this replay did not make it (in another test's window, or after a test this replay had not reached): an exchange of a connection outside this test (keploy serves a connection's first exchange, its handshake, to every test, unless the whole connection lay inside one test, which then owns it all; every other exchange goes to the test that recorded it). Likely, at replay this test made the call on a connection it opened itself, which it did not do when recording, or the call was made in this test where it was made in another when recording: record again so each test opens the connections it uses, or keep the application's connection reuse and timing as they were when recording. If this is a heartbeat or keepalive on a long-lived connection, it is to be declared session in keploy.yaml, by destination and request prefix: that rule for Generic connections is not available yet, so until it is, a test that sends one where it was not recorded fails.", name)
}

// maxHeldExchanges is how many exchanges a connection may have and still be
// wholly one test's: the first exchange of one with more is its preamble,
// whatever its times, so the recorder holds no more than this many of a
// connection's exchanges (preambleHold).
const maxHeldExchanges = 64

// preambleHold is, under the proven switch, a connection's first exchange the
// recorder holds while the test that owns its request may own the whole
// connection, and the exchanges recorded after it, held behind it so they go
// out in recorded order. It is decided by recorded times, never by when the
// recorder's goroutines run:
//
//   - the test that owns the connection's first request is the one keploy
//     record maps it to (syncMock.SyncMockManager.TestSpanAt): of the test
//     cases whose [request, answer] holds it, the one whose request came
//     first;
//   - the connection is wholly that test's when none of its chunks is
//     recorded after the test's answer, and it has no more than
//     maxHeldExchanges exchanges. A chunk is recorded when the relay read
//     it (fakeconn.Chunk.ReadAt), in either direction: a write's return can
//     be stamped after the other side acted on the bytes, the test's answer
//     included. A read can be stamped late too, but a request's read is
//     before its answer's, which the test waited for; a client's last write
//     that no answer follows, read after the test's answer, still reads as
//     outliving it (a known limit).
//
// Then its first exchange is per-test; else it is the connection's preamble:
// session. Its close is not stamped by when it happened (the proxy sees it as
// late as it runs), so it does not count: a connection the test opened, used,
// and left idle until it closed is wholly the test's, as no later test used
// it (its chunks would have come after the answer).
//
// The test's answer is known once its window has ended a test case: until
// then the exchanges are held, and a connection that ends first waits for it.
// The parser has consumed what it holds, so the session's pending work is
// cleared as it holds each (supervisor.Session.MockHeld). The manager counts
// them pending for the recording's stop (SyncMockManager.HoldExchanges), and
// as the stop begins they are decided with what is known then, and handed on
// before the stop's streams end.
type preambleHold struct {
	mgr    *syncMock.SyncMockManager
	opened time.Time
	last   time.Time
	// span is the owner's, while ok: a test may own the first request.
	span syncMock.TestSpan
	ok   bool
	// exchanges is how many it holds, the first included.
	exchanges int
	first     *models.Mock
	pending   []*models.Mock
	// stopping is closed as the recording's stop begins; handedOn tells the
	// manager the held exchanges are handed on.
	stopping <-chan struct{}
	handedOn func()
}

// holdPreamble decides at a connection's first exchange whether the recorder
// holds it: under the proven switch, while a test may own its request
// (opened, the connection's first). last is the connection's latest chunk
// time so far. nil means emit it now, as the session preamble: with the
// switch off, and for a connection opened outside every test (a boot-time
// pool, a call made after a test was answered), which no test can wholly
// contain.
func holdPreamble(ctx context.Context, first *models.Mock, opened, last time.Time) *preambleHold {
	if !provenswitch.On() {
		return nil
	}
	mgr := syncMock.FromContextOrGlobal(ctx)
	span, ok := mgr.TestSpanAt(opened)
	if !ok {
		return nil
	}
	h := &preambleHold{mgr: mgr, opened: opened, last: last, span: span, ok: true, exchanges: 1, first: first}
	h.stopping, h.handedOn = mgr.HoldExchanges(opened)
	return h
}

// changes is closed when the owner may have changed or been decided: then
// refresh. nil once the owner's span is final.
func (h *preambleHold) changes() <-chan struct{} {
	return h.span.Changed()
}

// refresh asks again which test owns the connection's first request, once the
// window it waited on has left the open ones.
func (h *preambleHold) refresh() {
	h.span, h.ok = h.mgr.TestSpanAt(h.opened)
}

// saw records a chunk of the connection at at.
func (h *preambleHold) saw(at time.Time) {
	if at.After(h.last) {
		h.last = at
	}
}

// add holds an exchange recorded after the first.
func (h *preambleHold) add(m *models.Mock) {
	h.pending = append(h.pending, m)
	h.exchanges++
}

// outlived reports whether the connection is known not to be wholly its
// test's: no test owns its first request, a chunk was recorded after the
// test's answer, or it has more exchanges than a test may wholly own. The
// first exchange is then the preamble, released at once.
func (h *preambleHold) outlived() bool {
	return !h.ok || h.exchanges > maxHeldExchanges || (h.span.Final() && h.last.After(h.span.End))
}

// decided reports whether, the connection ended, what it is is known: it
// outlived its test, or the test's answer is known.
func (h *preambleHold) decided() bool {
	return h.outlived() || h.span.Final()
}

// wholly reports whether the connection, ended, lay wholly inside its test.
// Undecided (the recording stopped first), it did not.
func (h *preambleHold) wholly() bool {
	return !h.outlived() && h.span.Final()
}

// settle waits, as the connection ends, until what it is is known, or the
// recording stops (its stop begins, or ctx ends).
func (h *preambleHold) settle(ctx context.Context) {
	for !h.decided() {
		select {
		case <-h.changes():
			h.refresh()
		case <-h.stopping:
			return
		case <-ctx.Done():
			return
		}
	}
}

// release returns the held exchanges in recorded order, the first stated
// per-test when the connection lay wholly inside its test, else session. A
// first exchange stated session while a test's window held its request is
// marked (models.MetadataLifetimeUndecidedKey) with why and that test: it may
// be the test's own call, and keploy record's judging does not judge a test
// that owns no call and held one.
func (h *preambleHold) release(wholly bool) []*models.Mock {
	if wholly {
		h.first.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	} else {
		h.first.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimeSession
		if why := h.undecided(); why != "" {
			h.first.Spec.Metadata[models.MetadataLifetimeUndecidedKey] = models.MockLifetimeUndecided{Why: why, TestStart: h.span.Start}.String()
		}
	}
	return append([]*models.Mock{h.first}, h.pending...)
}

// undecided is why a connection whose first request a test's window held is
// not known to lie wholly inside that test (a models.LifetimeUndecided*
// reason), or "" when no test's window holds its first request any more.
func (h *preambleHold) undecided() string {
	switch {
	case !h.ok:
		return ""
	case h.exchanges > maxHeldExchanges:
		return models.LifetimeUndecidedManyExchanges
	case !h.span.Final():
		return models.LifetimeUndecidedStoppedFirst
	case h.last.After(h.span.End):
		return models.LifetimeUndecidedReadAfterAnswer
	}
	return ""
}

// handOn emits the held exchanges in recorded order (release), then tells the
// manager they are handed on: they stop being pending for the stop only once
// the manager has them.
func (h *preambleHold) handOn(wholly bool, emit func(*models.Mock)) {
	for _, m := range h.release(wholly) {
		emit(m)
	}
	h.handedOn()
}
