package recorder

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A proxy-mode MySQL handshake read at the recording's stop: the relay holds
// the client's writes (Session.ClientWritesHeld), so the server has seen
// nothing of the client's HandshakeResponse41 (plaintext) or SSLRequest (TLS),
// and nothing after it was captured. The parser asks the relay to release the
// hold or to upgrade, and waits for its answer. The stop alone does not end
// that wait (untilParserEnds): the relay's answer, or its end, decides, and
// then the captured streams' end. Nothing captured is lost, so nothing is
// counted or said. Before, the stop ended the wait at once, and the stop
// branch counted every such connection as a mock left out, with a WARN.

// heldAtTheStop is a held connection whose capture ends at the client's
// response to the greeting (tls: an SSLRequest), read at the recording's stop.
// The relay answers through relay, run with the harness.
type heldAtTheStop struct {
	h    *v2Harness
	mgr  *syncMock.SyncMockManager
	logs *observer.ObservedLogs
	ctx  context.Context
}

func newHeldAtTheStop(t *testing.T, tls bool) *heldAtTheStop {
	t.Helper()
	c := newHeldAtTheStopStreamsOpen(t, tls)
	c.h.closeStreams()
	return c
}

// newHeldAtTheStopStreamsOpen is newHeldAtTheStop with both streams left
// open: the relay's teardown has not ended them yet.
func newHeldAtTheStopStreamsOpen(t *testing.T, tls bool) *heldAtTheStop {
	t.Helper()
	resetWarnLimiters()
	core, logs := observer.New(zapcore.DebugLevel)
	h := newV2Harness(t)
	h.logger = zap.New(core)
	h.sess.Logger = h.logger
	mgr := syncMock.New(nil)
	h.sess.Mgr = mgr
	h.sess.Orphans = &orphanSpans{}
	h.sess.ClientWritesHeld = true
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	h.pushDest(cannedHandshakeV10(t), base)
	if tls {
		// The SSLRequest and the ClientHello behind it, read together.
		h.pushClient(append(cannedSSLRequest(t, 1), tlsClientHelloBytes()...), base.Add(time.Millisecond))
	} else {
		h.pushClient(cannedHandshakeResponse41(t, 1, false), base.Add(time.Millisecond))
	}
	return &heldAtTheStop{h: h, mgr: mgr, logs: logs, ctx: stoppedRecording(context.Background(), h.sess)}
}

// nothingLost asserts the connection was neither counted nor said left out,
// and recorded nothing: its handshake never finished.
func (c *heldAtTheStop) nothingLost(t *testing.T, err error) {
	t.Helper()
	if n := len(c.h.mocks); n != 0 {
		t.Errorf("%d mock(s) recorded; want none: the handshake never finished", n)
	}
	if n := c.mgr.MocksLeftOut(); n != 0 {
		t.Errorf("%d mock(s) counted left out; want none: nothing captured was lost (RecordV2 = %v)", n, err)
	}
	if w := leftOutWarnings(c.logs); len(w) != 0 {
		t.Errorf("%d WARN(s) of a mock left out, the first %q; want none", len(w), w[0].ContextMap()["reason"])
	}
}

func heldKind(tls bool) directive.Kind {
	if tls {
		return directive.KindUpgradeTLS
	}
	return directive.KindReleaseClient
}

func heldCases(t *testing.T, row func(t *testing.T, tls bool)) {
	t.Helper()
	t.Run("plaintext hold release", func(t *testing.T) { row(t, false) })
	t.Run("TLS upgrade", func(t *testing.T) { row(t, true) })
}

// A relay that takes the directive and answers it late, as a starved agent's
// does: the parser waits for both. The directive channel has no room, so a
// wait the stop ends is seen here, whichever wait it is. The parser returns
// cleanly, so the supervisor never retires it: nothing that waited on its end
// is left behind.
func TestRecordV2_AtTheStopAHeldHandshakeWaitsForTheRelaysAnswer(t *testing.T) {
	heldCases(t, func(t *testing.T, tls bool) {
		before := fetchWatchers()
		c := newHeldAtTheStop(t, tls)
		dirs := make(chan directive.Directive)
		c.h.sess.Directives = dirs
		taken := make(chan directive.Kind, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			select {
			case d := <-dirs:
				taken <- d.Kind
				time.Sleep(50 * time.Millisecond)
				if tls {
					// The relay's own handshake took the ClientHello.
					c.h.sess.ClientStream.DiscardBefore(int64(len(cannedSSLRequest(t, 1)) + len(tlsClientHelloBytes())))
				}
				c.h.acks <- directive.Ack{Kind: d.Kind, OK: true}
			case <-time.After(5 * time.Second):
				close(taken)
			}
		}()
		err := recordV2NoPanic(t, c.ctx, c.h.logger, c.h.sess)
		if k, ok := <-taken; !ok || k != heldKind(tls) {
			t.Fatalf("the relay took %v (taken: %v); want %v: the stop ended the parser's wait to send it", k, ok, heldKind(tls))
		}
		if err != nil {
			t.Fatalf("RecordV2 = %v; want its streams' end, silently", err)
		}
		c.nothingLost(t, err)
		deadline := time.Now().Add(2 * time.Second)
		for fetchWatchers() > before {
			if time.Now().After(deadline) {
				t.Fatalf("%d goroutine(s) still wait on the parser's end 2 s after it returned; want none", fetchWatchers()-before)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// A relay that ends without answering, as one does at the stop: its teardown
// ends both streams, and then closes the ack channel (relay.Run). The parser
// takes that for the end it is: nothing more was captured.
func TestRecordV2_AtTheStopARelayThatEndsUnansweredLeavesAHeldHandshakeSilent(t *testing.T) {
	heldCases(t, func(t *testing.T, tls bool) {
		c := newHeldAtTheStop(t, tls)
		go func() {
			select {
			case <-c.h.dirs:
				time.Sleep(50 * time.Millisecond)
				close(c.h.acks)
			case <-time.After(5 * time.Second):
			}
		}()
		err := recordV2NoPanic(t, c.ctx, c.h.logger, c.h.sess)
		if err != nil {
			t.Fatalf("RecordV2 = %v; want the relay's end taken for its streams' end, silently", err)
		}
		c.nothingLost(t, err)
	})
}

// A TLS upgrade the relay fails at the stop (the stop cut its handshake):
// past the SSLRequest the capture holds TLS records only, which no recorder
// reads, and what the connection carries from the stop on is not recorded, so
// the failed upgrade loses nothing. While the recording runs it does lose what
// the connection carries next, and the recorder returns its error then
// (TestRecordV2_TLSUpgrade_FailureReturnsItsErrorAndLeavesNoMark).
func TestRecordV2_AtTheStopAFailedTLSUpgradeLeavesAHeldHandshakeSilent(t *testing.T) {
	c := newHeldAtTheStop(t, true)
	go func() {
		select {
		case d := <-c.h.dirs:
			time.Sleep(50 * time.Millisecond)
			c.h.acks <- directive.Ack{Kind: d.Kind, OK: false, Err: errFakeTLSFail}
		case <-time.After(5 * time.Second):
		}
	}()
	err := recordV2NoPanic(t, c.ctx, c.h.logger, c.h.sess)
	if err != nil {
		t.Fatalf("RecordV2 = %v; want the failed upgrade at the stop to end the capture silently", err)
	}
	c.nothingLost(t, err)
}

// The same failed upgrade before the relay's teardown has ended the streams:
// the capture ends at the SSLRequest then and there. What the client stream
// holds past it is a ClientHello, a TLS record, which no recorder reads (its
// header frames a MySQL packet of 66326 bytes), so the parser neither reads it
// as MySQL nor waits for the streams' end to return.
func TestRecordV2_AtTheStopAFailedTLSUpgradeEndsTheCaptureAtTheSSLRequest(t *testing.T) {
	c := newHeldAtTheStopStreamsOpen(t, true)
	go func() {
		select {
		case d := <-c.h.dirs:
			c.h.acks <- directive.Ack{Kind: d.Kind, OK: false, Err: errFakeTLSFail}
		case <-time.After(5 * time.Second):
		}
	}()
	var err error
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		err = recordV2NoPanic(t, c.ctx, c.h.logger, c.h.sess)
	}()
	// The streams end only once the row has its answer, as the relay's
	// teardown would end them, so a parser still reading them returns too.
	t.Cleanup(func() {
		c.h.closeStreams()
		<-returned
	})
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("RecordV2 still reads the client stream 10 s after the relay failed the upgrade at the stop; " +
			"want the capture ended at the SSLRequest: past it the stream holds TLS records only")
	}
	if err != nil {
		t.Fatalf("RecordV2 = %v; want the failed upgrade at the stop to end the capture silently", err)
	}
	c.nothingLost(t, err)
}

// A relay that never answers and never ends: the parser waits until the
// supervisor retires it (here the stop's own deadline), never less, and then
// ends silently: its retirement is the supervisor's to tell.
func TestRecordV2_AtTheStopAHeldHandshakeWaitsUntilTheParserIsRetired(t *testing.T) {
	heldCases(t, func(t *testing.T, tls bool) {
		c := newHeldAtTheStop(t, tls)
		s := supervisor.New(supervisor.Config{Logger: zap.NewNop(), HangBudget: time.Hour})
		deadline := make(chan struct{})
		s.StopDeadline = deadline
		returned := make(chan error, 1)
		go s.Run(c.ctx, func(pctx context.Context, sess *supervisor.Session) error {
			err := recordV2NoPanic(t, pctx, c.h.logger, sess)
			returned <- err
			return err
		}, c.h.sess)
		time.Sleep(300 * time.Millisecond)
		select {
		case err := <-returned:
			t.Fatalf("RecordV2 = %v before its retirement: the stop alone ended the wait for the relay", err)
		default:
		}
		close(deadline)
		var err error
		select {
		case err = <-returned:
		case <-time.After(10 * time.Second):
			t.Fatal("RecordV2 still waits for the relay 10 s after the supervisor retired it")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RecordV2 = %v; want the parser's end", err)
		}
		c.nothingLost(t, err)
	})
}
