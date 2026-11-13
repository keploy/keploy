package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// behindAtStop captures a handshake and whole queries 1..n, then ends the
// streams as the relay does at its teardown, with the recording already
// stopped: the recorder starts behind all of its capture. Mocks go to h.mocks,
// through the sync-mock manager when viaSyncMock (as the proxy routes them).
func behindAtStop(t *testing.T, h *v2Harness, n int, viaSyncMock bool) (context.Context, *syncMock.SyncMockManager) {
	t.Helper()
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	h.pushDest(greetingBuf, base)
	h.pushClient(cannedHandshakeResponse41(t, 1, false), base.Add(time.Millisecond))
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), base.Add(2*time.Millisecond))
	for q := 1; q <= n; q++ {
		at := base.Add(time.Duration(10*q) * time.Millisecond)
		h.pushClient(cannedCOMQuery(t, 0, fmt.Sprintf("SELECT %d", q)), at)
		h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at.Add(time.Millisecond))
	}
	h.closeStreams()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.sess.Ctx = ctx
	h.sess.RecordingDone = ctx.Done()
	mgr := syncMock.New(nil)
	h.sess.Mgr = mgr
	if viaSyncMock {
		mgr.SetOutputChannel(h.mocks)
		h.sess.RouteMocksViaSyncMock = true
	}
	return ctx, mgr
}

// takeMocks reads h's mocks until it has want of them, or none comes for 10s
// (a lost mock is then a failure, not a hang).
func takeMocks(h *v2Harness, want int) []*models.Mock {
	var got []*models.Mock
	for len(got) < want {
		select {
		case m := <-h.mocks:
			got = append(got, m)
		case <-time.After(10 * time.Second):
			return got
		}
	}
	return got
}

// A recorder behind its capture at the recording's stop records every
// exchange its capture holds whole: the stop ends its context before it has
// read them, and it reads them all the same, off the streams the relay ends
// at its teardown once it has handed on what it captured. It returned at its
// command loop's check of the context instead (and its reads, which return
// the context's error before every read, never read a packet), and the
// session refused the handshake's mock once the context had ended: all four
// exchanges were lost with nothing recording, leaving out or reporting them.
// Under both switches, and both ways a mock is handed on (the session's
// channel, and the sync-mock manager the proxy routes mocks through).
func TestRecordV2_BehindAtTheStopRecordsEveryExchangeCapturedWhole(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, viaSyncMock := range []bool{false, true} {
			t.Run(fmt.Sprintf("proven=%v, sync-mock=%v", proven, viaSyncMock), func(t *testing.T) {
				if proven {
					t.Setenv("KEPLOY_PROVEN", "1")
				} else {
					t.Setenv("KEPLOY_PROVEN", "")
				}
				const queries = 3
				h := newV2Harness(t)
				ctx, mgr := behindAtStop(t, h, queries, viaSyncMock)
				if err := recordV2NoPanic(t, ctx, zaptest.NewLogger(t), h.sess); err != nil {
					t.Fatalf("RecordV2 = %v, want nil: the streams' end ends the recording", err)
				}
				got := takeMocks(h, 1+queries)
				if len(got) != 1+queries {
					t.Fatalf("recorded %d mock(s), want the handshake's and the %d queries captured whole before the stop", len(got), queries)
				}
				if got[0].Spec.Metadata["type"] != "config" {
					t.Fatalf("first mock is %q, want the handshake's (config)", got[0].Spec.Metadata["type"])
				}
				for i, m := range got[1:] {
					qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
					if want := fmt.Sprintf("SELECT %d", i+1); !ok || qp.Query != want {
						t.Fatalf("mock %d is of %+v, want the query %q", i+1, m.Spec.MySQLRequests[0].Message, want)
					}
					if len(m.Spec.MySQLResponses) == 0 {
						t.Fatalf("mock %d has no response", i+1)
					}
				}
				if n := mgr.MocksLeftOut(); n != 0 {
					t.Fatalf("%d mock(s) left out, want none: every exchange was captured whole", n)
				}
			})
		}
	}
}

// The recording's stop can come inside EmitMock, after the recorder saw the
// context last and before EmitMock's own look at it, which comes after the
// post-record hooks. Here the stop comes as the handshake's mock is handed
// over (its record hook). Through the sync-mock manager, as the agent
// records, the mock is recorded all the same: it was captured before the
// stop, and the supervisor's stop waits for the recorder to read what was
// captured. Nothing is left out, and the queries read after it are recorded.
//
// Before, EmitMock refused the handshake's mock, and it was left out with
// the test cases over it.
//
// Not parallel: the WARNs are limited process-wide, and this test reads them.
func TestRecordV2_AMockHandedOverAsTheStopComesIsRecorded(t *testing.T) {
	resetWarnLimiters()
	h := newV2Harness(t)
	_, mgr := behindAtStop(t, h, 2, true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.sess.Ctx = ctx
	h.sess.OnMockRecorded = func(*models.Mock) { cancel() }
	core, logs := observer.New(zapcore.DebugLevel)
	h.sess.Logger = zap.New(core)
	if err := recordV2NoPanic(t, ctx, zaptest.NewLogger(t), h.sess); err != nil {
		t.Fatalf("RecordV2 = %v, want nil", err)
	}
	got := takeMocks(h, 3)
	if len(got) != 3 || got[0].Spec.Metadata["type"] != "config" {
		t.Fatalf("recorded %d mock(s), want the handshake's and the 2 queries' (every exchange was captured before the stop)", len(got))
	}
	var reasons []string
	for _, e := range leftOutWarnings(logs) {
		reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
	}
	if n := mgr.MocksLeftOut(); n != 0 || len(reasons) != 0 {
		t.Fatalf("%d left out, reasons %q; want none", n, reasons)
	}
}

// Straight to the session's channel, a mock EmitMock refuses as the stop
// comes (the context ends between emitV2's look at it and EmitMock's own,
// here in the hook EmitMock runs before it, with no room on the channel) is
// left out with its reason, counted and spanned, not dropped with nothing
// said.
//
// Not parallel: the WARNs are limited process-wide, and this test reads them.
func TestEmitV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
	resetWarnLimiters()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := syncMock.New(nil)
	spans := &syncMock.Spans{}
	core, logs := observer.New(zapcore.DebugLevel)
	sess := &supervisor.Session{
		Ctx:            ctx,
		Mgr:            mgr,
		Mocks:          make(chan *models.Mock),
		OnMockRecorded: func(*models.Mock) { cancel() },
		Orphans:        spans,
		Logger:         zap.New(core),
	}
	emitV2(sess, emitAtStopMock("refused"))
	var reasons []string
	for _, e := range leftOutWarnings(logs) {
		reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
	}
	if n, s := mgr.MocksLeftOut(), spanCount(spans); n != 1 || s != 1 || len(reasons) != 1 || reasons[0] != leftOutRefusedAtStop {
		t.Fatalf("%d left out (%q), %d span(s); want the refused mock left out once for %q, with its span", n, reasons, s, leftOutRefusedAtStop)
	}
}

// Straight to the session's channel, every mock handed over after the stop
// is recorded while the channel has room: EmitMock would refuse each one
// whose send it did not pick (a select between the send and the context's
// end, both ready), so emitV2 uses EmitMockOnShutdown once the context has
// ended.
func TestEmitV2_MocksHandedOverAfterTheStopAreRecorded(t *testing.T) {
	const n = 24
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, n)
	sess := &supervisor.Session{Ctx: ctx, Mgr: mgr, Mocks: out, Orphans: &syncMock.Spans{}, Logger: zaptest.NewLogger(t)}
	for i := 0; i < n; i++ {
		emitV2(sess, emitAtStopMock(fmt.Sprint("after the stop ", i)))
	}
	if got, left := len(out), mgr.MocksLeftOut(); got != n || left != 0 {
		t.Fatalf("recorded %d of the %d mocks handed over after the stop, %d left out; want every one recorded", got, n, left)
	}
}

// emitAtStopMock is a mock with the times of an exchange, so a mock left out
// gets its span.
func emitAtStopMock(name string) *models.Mock {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	return &models.Mock{Kind: models.MySQL, Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// spanCount is how many spans s holds.
func spanCount(s *syncMock.Spans) int {
	n, _ := s.Counts()
	return n
}

// A connection joined mid-stream frames on an assumption, so a PREPARE with one
// run of definitions is held until a packet settles its framing
// (heldPrepare). The recording can stop with the PREPARE held and nothing sent
// after it on the client's stream: what the server sent after the definitions,
// its EOF here, was captured before the stop, and the relay ends both streams
// once it has handed that on. The recorder settles the PREPARE from it as at
// any end of the client's stream, and records it with its EOF, leaving nothing
// out. Before, it settled it so only while the recording ran: at the stop it
// left the PREPARE out, though its capture held it whole.
func TestRecordV2_PostTLS_APrepareHeldAtTheStopIsSettledByWhatTheServerSent(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	reply := prepareOK(false, [][]byte{def, def}, nil) // PREPARE_OK, 2 definitions, EOF
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0), reply)
	ctx, cancel := context.WithCancel(j.ctx)
	cancel()
	j.ctx = ctx
	j.h.sess.Ctx = ctx
	j.h.sess.RecordingDone = ctx.Done()
	mocks, err := j.run(t)
	if err != nil {
		t.Fatalf("RecordV2 = %v, want nil: the streams' end ends the recording", err)
	}
	if len(mocks) != 1 || leftOut.count() != 0 {
		t.Fatalf("%d mock(s), %d left out; want the PREPARE recorded, nothing left out", len(mocks), leftOut.count())
	}
	sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
	if !ok || len(sp.ParamDefs) != 2 || !bytes.Equal(sp.EOFAfterParamDefs, reply[len(reply)-1]) {
		t.Fatalf("the PREPARE recorded %+v; want its 2 definitions and the EOF it was sent", mocks[0].Spec.MySQLResponses[0].Message)
	}
}

// The client's reader can be closed while its producer could still send (the
// supervisor's teardown): it then reads fakeconn.ErrClosed, not io.EOF, once it
// has taken what it was handed. With a PREPARE held, what the server sent
// after the definitions settles it there too: it is recorded with its EOF, and
// nothing is left out.
func TestRecordV2_PostTLS_APrepareHeldAsTheClientsReaderIsClosedIsSettled(t *testing.T) {
	t.Parallel()
	def := columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
	j := newJoinedMidStream(t)
	leftOut := &orphanSpans{}
	j.h.sess.Orphans = leftOut
	reply := prepareOK(false, [][]byte{def, def}, nil) // PREPARE_OK, 2 definitions, EOF
	j.exchange(wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0), reply)
	close(j.h.destCh)
	_ = j.h.sess.ClientStream.Close() // its channel stays open
	done := make(chan error, 1)
	go func() { done <- recordV2NoPanic(t, j.ctx, j.h.logger, j.h.sess) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, fakeconn.ErrClosed) {
			t.Fatalf("RecordV2 = %v; want nil or the client reader's close", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("RecordV2 still running a minute after the client's reader was closed")
	}
	var mocks []*models.Mock
	for len(j.h.mocks) > 0 {
		if m := <-j.h.mocks; m.Spec.Metadata["type"] != "config" {
			mocks = append(mocks, m)
		}
	}
	if len(mocks) != 1 || leftOut.count() != 0 {
		t.Fatalf("%d mock(s), %d left out; want the PREPARE recorded, nothing left out", len(mocks), leftOut.count())
	}
	sp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtPrepareOkPacket)
	if !ok || len(sp.ParamDefs) != 2 || !bytes.Equal(sp.EOFAfterParamDefs, reply[len(reply)-1]) {
		t.Fatalf("the PREPARE recorded %+v; want its 2 definitions and the EOF it was sent", mocks[0].Spec.MySQLResponses[0].Message)
	}
}

// stoppedRecording is a recording that has stopped: the parser's context ends
// with it, and the session is told (RecordingDone).
func stoppedRecording(parent context.Context, sess *supervisor.Session) context.Context {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	sess.Ctx = ctx
	sess.RecordingDone = ctx.Done()
	return ctx
}

// TestResolvePreTLSGreeting_TheStopDoesNotEndTheWait: the recording stops
// before the raw leg's parser reads the greeting it captured, and that parser
// pushes it 150 ms into the decrypted stream's wait. The wait takes it: the
// stop ends the parser's context, but the greeting it waits for was captured
// before the stop. Before, the stop ended the wait at once and the
// connection's capture was left out.
func TestResolvePreTLSGreeting_TheStopDoesNotEndTheWait(t *testing.T) {
	t.Parallel()
	store := models.NewTLSHandshakeStore()
	sess := &supervisor.Session{}
	ctx := stoppedRecording(context.Background(), sess)
	go func() {
		time.Sleep(150 * time.Millisecond)
		store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{RespPackets: [][]byte{[]byte("own-greeting")}})
	}()
	entry, _, source := resolvePreTLSGreeting(ctx, sess, store, models.HandshakeOwner{}, 3306, "", "", testDst("10.0.0.5:3306"), false)
	if source == greetingNone || len(entry.RespPackets) != 1 || string(entry.RespPackets[0]) != "own-greeting" {
		t.Fatalf("source %v, entry %+v; want the greeting the raw leg pushed after the stop", source, entry)
	}
}

// TestResolvePreTLSGreeting_AtTheStopTheWaitKeepsItsBound: a raw leg that never
// pushes leaves the wait at the stop to its overall bound, not to the stop
// and not beyond the bound.
func TestResolvePreTLSGreeting_AtTheStopTheWaitKeepsItsBound(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "300")
	store := models.NewTLSHandshakeStore()
	sess := &supervisor.Session{}
	ctx := stoppedRecording(context.Background(), sess)
	start := time.Now()
	sources := make(chan preTLSGreetingSource, 1)
	go func() {
		_, _, source := resolvePreTLSGreeting(ctx, sess, store, models.HandshakeOwner{}, 3306, "", "", testDst("10.0.0.5:3306"), false)
		sources <- source
	}()
	select {
	case source := <-sources:
		if source != greetingNone {
			t.Fatalf("source = %v, want greetingNone: nothing was pushed", source)
		}
		if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
			t.Fatalf("the wait at the stop took %v; want its 300ms bound", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait at the stop did not end at its 300ms bound")
	}
}

// TestRecordV2_PostTLS_AConnectionCapturedBeforeTheStopIsRecordedWithItsGreeting:
// a fresh TLS connection's decrypted streams hold its HandshakeResponse41 and a
// query with their answers, all captured, none read, when the recording stops;
// the relay has ended both streams. Its raw leg's parser pushes the pre-TLS
// greeting 150 ms after the recorder starts waiting for it. The connection is
// recorded whole: its config mock with the greeting and SSLRequest, and its
// query. Before, the stop ended the wait at once, the recorder refused to dial
// the server at the stop, and the connection was left out.
func TestRecordV2_PostTLS_AConnectionCapturedBeforeTheStopIsRecordedWithItsGreeting(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	handshakeBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), handshakeBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))
	h.closeStreams()
	store := models.NewTLSHandshakeStore()
	ctx := stoppedRecording(postTLSCtxWithStore(store), h.sess)
	go func() {
		time.Sleep(150 * time.Millisecond)
		store.Push(models.HandshakeStoreKey("", 3306), models.TLSHandshakeEntry{
			RespPackets:  [][]byte{handshakeBuf},
			ReqPackets:   [][]byte{cannedSSLRequest(t, 1)},
			ReqTimestamp: base,
		})
	}()
	done := make(chan error, 1)
	go func() { done <- recordV2NoPanic(t, ctx, h.logger, h.sess) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RecordV2 = %v, want nil: the greeting was captured before the stop", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("RecordV2 did not return")
	}
	got := takeMocks(h, 2)
	if len(got) != 2 {
		t.Fatalf("%d mock(s); want the config mock and the query's", len(got))
	}
	if got[0].Name != "config" || len(got[0].Spec.MySQLResponses) < 1 || len(got[0].Spec.MySQLRequests) < 2 {
		t.Fatalf("first mock %q with %d request(s), %d response(s); want the config mock with the SSLRequest, the HandshakeResponse41 and the greeting",
			got[0].Name, len(got[0].Spec.MySQLRequests), len(got[0].Spec.MySQLResponses))
	}
	if !got[0].Spec.ReqTimestampMock.Equal(base) {
		t.Errorf("config ReqTimestampMock = %v, want the raw leg's %v", got[0].Spec.ReqTimestampMock, base)
	}
	assertQueryMock(t, got[1])
}

// freshTLSConnectionAtTheStop captures, on h, a fresh TLS connection's
// decrypted streams, its HandshakeResponse41 and a query with their answers,
// none read, ends both streams as the relay does at its teardown, and points the
// session at addr, the server a greeting fetch dials. No greeting is in store
// and none is remembered. The recording has stopped (ctx).
func freshTLSConnectionAtTheStop(t *testing.T, h *v2Harness, addr string) (context.Context, time.Time) {
	t.Helper()
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), cannedHandshakeV10(t)[4:])
	if err != nil {
		t.Fatal(err)
	}
	h.pushClient(cannedHandshakeResponse41(t, 2, false), base.Add(5*time.Millisecond))
	h.pushDest(cannedOK(t, 3, greeting.CapabilityFlags), base.Add(10*time.Millisecond))
	h.pushClient(cannedCOMQuery(t, 0, "SELECT 1"), base.Add(20*time.Millisecond))
	h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), base.Add(25*time.Millisecond))
	h.closeStreams()
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: addr, Port: 3306}
	return stoppedRecording(postTLSCtxWithStore(models.NewTLSHandshakeStore()), h.sess), base.Add(5 * time.Millisecond)
}

// A fresh TLS connection captured before the stop whose raw-leg greeting never
// comes, and whose server's greeting is not remembered: at the stop the
// recorder fetches the greeting from the server, as it would while the
// recording runs, and records the connection with it. Before, the stop refused
// the dial, and the connection was left out without a word.
func TestRecordV2_PostTLS_AtTheStopTheGreetingIsFetchedAndTheConnectionRecorded(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "100")
	srv := startFakeMySQLGreeter(t, cannedHandshakeV10(t), 0)
	h := newV2Harness(t)
	ctx, _ := freshTLSConnectionAtTheStop(t, h, srv.addr)
	if err := recordV2NoPanic(t, ctx, h.logger, h.sess); err != nil {
		t.Fatalf("RecordV2 = %v, want nil: the server answers with its greeting", err)
	}
	got := takeMocks(h, 2)
	if len(got) != 2 {
		t.Fatalf("%d mock(s); want the config mock and the query's", len(got))
	}
	if got[0].Name != "config" || len(got[0].Spec.MySQLResponses) < 1 {
		t.Fatalf("first mock %q with %d response(s); want the config mock with the fetched greeting", got[0].Name, len(got[0].Spec.MySQLResponses))
	}
	if _, ok := got[0].Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet); !ok {
		t.Fatalf("the config mock's first response is %T; want the fetched HandshakeV10", got[0].Spec.MySQLResponses[0].Message)
	}
	assertQueryMock(t, got[1])
	if n := srv.accepted.Load(); n != 1 {
		t.Fatalf("the server was dialled %d times; want once, for the greeting", n)
	}
}

// When that fetch fails (the server refuses the dial), the connection is left
// out at the stop as it would be while the recording runs, and says so: one
// left-out WARN that names the failed fetch, counted, spanned from its first
// client packet. Before, the stop made the loss silent.
func TestRecordV2_PostTLS_AtTheStopAFailedGreetingFetchLeavesTheConnectionOutNamed(t *testing.T) {
	resetWarnLimiters()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := ln.Addr().String()
	_ = ln.Close()
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "100")
	core, logs := observer.New(zapcore.DebugLevel)
	h := newV2Harness(t)
	h.logger = zap.New(core)
	h.sess.Logger = h.logger
	mgr := syncMock.New(nil)
	h.sess.Mgr = mgr
	spans := &orphanSpans{}
	h.sess.Orphans = spans
	ctx, firstAt := freshTLSConnectionAtTheStop(t, h, refused)
	_ = recordV2NoPanic(t, ctx, h.logger, h.sess)
	if len(h.mocks) != 0 {
		t.Fatalf("%d mock(s) recorded with no greeting", len(h.mocks))
	}
	warns := leftOutWarnings(logs)
	if len(warns) != 1 || mgr.MocksLeftOut() != 1 || spans.count() != 1 {
		t.Fatalf("%d left-out WARN(s), %d counted, %d span(s); want the connection left out once", len(warns), mgr.MocksLeftOut(), spans.count())
	}
	if reason, _ := warns[0].ContextMap()["reason"].(string); !strings.Contains(reason, "direct fetch failed") || !strings.Contains(reason, refused) {
		t.Fatalf("left out for %q; want the failed fetch from %s named", reason, refused)
	}
	if start := spans.windows[0][0]; !start.Equal(firstAt) {
		t.Fatalf("the span starts at %v; want the first client packet's %v", start, firstAt)
	}
}

// A parser the supervisor retires at the stop (here the stop's own deadline)
// while it waits for a TLS connection's greeting ends the wait within a poll
// slice and never dials the server: nothing it records from its retirement is
// kept. Before, it waited out the 30 s bound.
func TestRecordV2_PostTLS_AParserRetiredAtTheStopEndsItsGreetingWait(t *testing.T) {
	t.Parallel()
	srv := startFakeMySQLGreeter(t, cannedHandshakeV10(t), 0)
	h := newV2Harness(t)
	ctx, _ := freshTLSConnectionAtTheStop(t, h, srv.addr)
	s := supervisor.New(supervisor.Config{Logger: zap.NewNop(), HangBudget: time.Hour})
	deadline := make(chan struct{})
	s.StopDeadline = deadline
	returned := make(chan error, 1)
	go s.Run(ctx, func(pctx context.Context, sess *supervisor.Session) error {
		err := recordV2NoPanic(t, pctx, h.logger, sess)
		returned <- err
		return err
	}, h.sess)
	time.Sleep(300 * time.Millisecond) // into the wait for the raw leg's greeting
	select {
	case err := <-returned:
		t.Fatalf("RecordV2 = %v before its retirement: the stop alone ended the greeting wait", err)
	default:
	}
	close(deadline)
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("RecordV2 still waits for the greeting 10 s after the supervisor retired it; want within a poll slice, never its 30 s bound")
	}
	if n := srv.accepted.Load(); n != 0 {
		t.Fatalf("the retired parser dialled the server %d times", n)
	}
}

// A parser the supervisor retires at the stop while its greeting fetch waits on
// the server ends the fetch then: the fetch fails with the parser's end, not
// with a fault of the server's, so the recorder does not report the connection
// left out for a failed fetch (the retirement's loss is the supervisor's to
// tell). Before the fetch watched the retirement, it waited out its 3 s read
// bound and blamed the server.
func TestRecordV2_PostTLS_AParserRetiredAtTheStopMidFetchEndsTheFetch(t *testing.T) {
	t.Setenv("KEPLOY_MYSQL_POSTTLS_STASH_WAIT_MS", "100")
	gate := make(chan struct{}) // the server never answers
	srv := startGatedFakeMySQLGreeter(t, cannedHandshakeV10(t), 0, gate)
	t.Cleanup(func() { close(gate) })
	resetWarnLimiters()
	core, logs := observer.New(zapcore.DebugLevel)
	h := newV2Harness(t)
	h.logger = zap.New(core)
	h.sess.Logger = h.logger
	mgr := syncMock.New(nil)
	h.sess.Mgr = mgr
	ctx, _ := freshTLSConnectionAtTheStop(t, h, srv.addr)
	s := supervisor.New(supervisor.Config{Logger: zap.NewNop(), HangBudget: time.Hour})
	deadline := make(chan struct{})
	s.StopDeadline = deadline
	returned := make(chan error, 1)
	go s.Run(ctx, func(pctx context.Context, sess *supervisor.Session) error {
		err := recordV2NoPanic(t, pctx, h.logger, sess)
		returned <- err
		return err
	}, h.sess)
	waitAccepted(t, srv, 1) // the fetch has dialled, and waits for the greeting
	close(deadline)
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("RecordV2 did not return after its retirement")
	}
	if n := mgr.MocksLeftOut(); n != 0 || len(leftOutWarnings(logs)) != 0 {
		t.Fatalf("%d left out, %d WARN(s): the retirement's end of the fetch was reported as a failed fetch", n, len(leftOutWarnings(logs)))
	}
}

// While the recording runs, a parser torn down mid-handshake (its ctx ends:
// a hang, the memory cap) returns its context's end, and reports nothing
// itself: the supervisor's retirement tells it, and a report here would tell
// it twice. Here the greeting fetch then fails on its own (a stand-in
// destination), not with the parser's end.
func TestRecordV2_PostTLS_ATornDownParsersHandshakeFailureIsNotReportedTwice(t *testing.T) {
	t.Parallel()
	h := newV2Harness(t)
	mgr := syncMock.New(nil)
	h.sess.Mgr = mgr
	h.pushClient(cannedHandshakeResponse41(t, 2, false), time.Now())
	h.closeStreams()
	h.sess.Opts.DstCfg = &models.ConditionalDstCfg{Addr: "10.0.0.5:3306", Port: 3306, AddrFabricated: true}
	h.sess.RecordingDone = make(chan struct{}) // the recording runs
	ctx, cancel := context.WithCancel(postTLSCtxWithStore(models.NewTLSHandshakeStore()))
	cancel()
	if err := recordV2NoPanic(t, ctx, h.logger, h.sess); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordV2 = %v; want the teardown's context.Canceled", err)
	}
	if n := mgr.MocksLeftOut(); n != 0 {
		t.Fatalf("%d left out by the recorder of a torn-down parser; want none (the supervisor tells it)", n)
	}
}
