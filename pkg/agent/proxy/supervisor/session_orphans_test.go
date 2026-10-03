package supervisor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A caller that keeps its spans elsewhere (a DaemonSet agent, per pod) gets a
// parser's spans there, not on the manager its mocks go to.
func TestSessionOrphansTakesTheSpans(t *testing.T) {
	t.Parallel()
	mgr := syncMock.New(nil)
	spans := &syncMock.Spans{}
	s := &Session{Mgr: mgr, Orphans: spans}

	base := time.Now().Add(-time.Minute)
	s.RecordOrphanWindow(base, base.Add(time.Second))
	closeIt := s.OpenOrphanWindow(base.Add(10 * time.Second))
	closeIt()

	if c, o := spans.Counts(); c != 2 || o != 0 {
		t.Fatalf("Orphans holds (closed=%d, open=%d), want (2, 0)", c, o)
	}
	if _, c, o := mgr.OrphanRangeCount(); c != 0 || o != 0 {
		t.Fatalf("the manager got spans (closed=%d, open=%d) that belong to Orphans", c, o)
	}

	// Unset, the manager takes them, as before.
	s2 := &Session{Mgr: mgr}
	s2.RecordOrphanWindow(base, base.Add(time.Second))
	if _, c, _ := mgr.OrphanRangeCount(); c != 1 {
		t.Fatalf("without Orphans the manager holds %d spans, want 1", c)
	}
}

// A mock the session leaves out for the incomplete-mock flag is an exchange
// its connection could not record: it is reported where the session's
// unrecordable spans go, over the mock's own times, so the test cases recorded
// over it that are not saved yet are left out; it is counted on the session's
// manager, for the recording's summary; and it is said at WARN with why the
// flag was set and what to do. The next mock is recorded. Before, the mock was
// left out with a Debug line: nothing reported it, and its test cases were
// saved without it.
//
// The WARN is limited per cause, once per leftOutWarnEvery: a cause is never
// held back behind another's (a per_conn_cap loss after a memory_pressure
// WARN says what to raise; a short_write after a decode error's WARN says
// short_write), the error text a parser's reason carries after its cause does
// not make a cause of its own, and the next WARN of a cause says how many of
// it were held back. Each one held back is logged at Debug, and each is
// counted, whether or not the session has a logger.
//
// Not parallel: the WARN limit is process-wide, and this test sets its clock.
func TestAMockLeftOutForTheIncompleteFlagIsReported(t *testing.T) {
	clock := time.Now()
	prev := leftOutWarns
	leftOutWarns = &WarnLimiters{every: leftOutWarnEvery, now: func() time.Time { return clock }}
	t.Cleanup(func() { leftOutWarns = prev })

	core, logs := observer.New(zapcore.DebugLevel)
	spans := &syncMock.Spans{}
	mgr := syncMock.New(nil)
	mocks := make(chan *models.Mock, 4)
	s := &Session{Mgr: mgr, Orphans: spans, Mocks: mocks, Logger: zap.New(core), ClientConnID: "conn-7", Ctx: context.Background()}
	cleared := 0
	s.OnPendingCleared = func() { cleared++ }

	base := time.Now().Add(-time.Minute)
	mock := func(name string, at time.Time) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.Mongo}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(20*time.Millisecond)
		return m
	}
	leftOut := 0
	leaveOut := func(reason string) {
		t.Helper()
		leftOut++
		s.MarkMockIncomplete(reason)
		if err := s.EmitMock(mock("left-out", base.Add(time.Duration(2*leftOut)*time.Hour))); err != nil {
			t.Fatalf("EmitMock: %v", err)
		}
	}
	warns := func() []observer.LoggedEntry { return logs.FilterLevelExact(zapcore.WarnLevel).All() }
	heldBack := func() int {
		return logs.FilterLevelExact(zapcore.DebugLevel).FilterMessage(leftOutDebugMsg).Len()
	}

	s.MarkMockIncomplete("channel_full")
	if err := s.EmitMock(mock("left-out", base)); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	leftOut++
	if len(mocks) != 0 {
		t.Fatal("a mock marked incomplete was emitted")
	}
	if over, _ := spans.Overlaps(base.Add(time.Millisecond), base.Add(2*time.Millisecond)); !over {
		t.Fatal("a test case over the exchange whose mock was left out is not left out: the span was not reported")
	}
	if over, _ := spans.Overlaps(base.Add(time.Second), base.Add(2*time.Second)); over {
		t.Fatal("a test case after the exchange is left out: the span is wider than the mock's exchange")
	}
	w := warns()
	if len(w) != 1 || w[0].ContextMap()["reason"] != "channel_full" || w[0].ContextMap()["connID"] != "conn-7" ||
		w[0].ContextMap()["mockName"] != "left-out" {
		t.Fatalf("the mock left out is said %d time(s) at WARN (%v), want once, with why and for which connection", len(w), w)
	}
	if next, _ := w[0].ContextMap()["next_step"].(string); next != leftOutNextStep(leftOutOther, true) {
		t.Fatalf("the WARN's next_step is %q, want what to do for its reason", next)
	}
	if _, ok := w[0].ContextMap()["sameWarningsHeldBack"]; ok {
		t.Fatal("the first WARN says warnings were held back before it")
	}
	if cleared != 1 {
		t.Fatalf("pending work cleared %d time(s), want once: the parser consumed its input", cleared)
	}

	// The next mock is recorded, and reports nothing.
	if err := s.EmitMock(mock("kept", base.Add(time.Minute))); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if len(mocks) != 1 {
		t.Fatalf("%d mocks emitted after the one left out, want the next one", len(mocks))
	}
	if c, o := spans.Counts(); c != 1 || o != 0 {
		t.Fatalf("the spans hold (closed=%d, open=%d), want the one exchange left out", c, o)
	}

	// The same class within the interval: held back, each at Debug.
	for i := 0; i < 99; i++ {
		leaveOut("channel_full")
	}
	if n := len(warns()); n != 1 {
		t.Fatalf("%d WARN lines for 100 mocks of one class left out within %v, want 1", n, leftOutWarnEvery)
	}
	if n := heldBack(); n != 99 {
		t.Fatalf("%d mocks held back from WARN are said at Debug, want each of the 99", n)
	}

	// Another cause within the interval is said at once, with the knob of
	// its class.
	for _, tc := range []struct{ reason, class string }{
		{"memory_pressure", leftOutMemoryPressure},
		{"per_conn_cap", leftOutPerConnCap},
		{"desynced", leftOutDesynced},
		{"short_write", leftOutOther},
		{"http decode error: response read failed: invalid content-length", leftOutOther},
	} {
		n := len(warns())
		leaveOut(tc.reason)
		w = warns()
		last := w[len(w)-1].ContextMap()
		if len(w) != n+1 || last["reason"] != tc.reason || last["next_step"] != leftOutNextStep(tc.class, true) {
			t.Fatalf("a %s mock left out after another cause's WARN is not said at WARN with its own next_step (last WARN: %v)", tc.reason, last)
		}
		if _, ok := last["sameWarningsHeldBack"]; ok {
			t.Fatalf("the first %s WARN says warnings were held back before it", tc.reason)
		}
	}
	if n := len(warns()); n != 6 {
		t.Fatalf("%d WARN lines, want one per cause", n)
	}

	// The same cause with other error text waits with it, and the cause's
	// next WARN, past the interval, says how many waited.
	leaveOut("http decode error: request read failed: EOF")
	if n := len(warns()); n != 6 {
		t.Fatalf("%d WARN lines, want a decode error held back behind the WARN of its cause, whatever its text", n)
	}
	if n := heldBack(); n != 100 {
		t.Fatalf("%d mocks held back from WARN are said at Debug, want 100", n)
	}
	clock = clock.Add(leftOutWarnEvery)
	for _, tc := range []struct {
		reason string
		held   uint64
	}{
		{"http decode error: response read failed: unexpected EOF", 1},
		{"channel_full", 99},
	} {
		leaveOut(tc.reason)
		w = warns()
		last := w[len(w)-1].ContextMap()
		if last["reason"] != tc.reason {
			t.Fatalf("%d WARN lines, the last %v; want the %s cause's next one past the interval", len(w), last, tc.reason)
		}
		if held, _ := last["sameWarningsHeldBack"].(uint64); held != tc.held {
			t.Fatalf("the %s WARN says %v held back, want the %d of its cause since its last one", tc.reason, last["sameWarningsHeldBack"], tc.held)
		}
	}
	if n := len(warns()); n != 8 {
		t.Fatalf("%d WARN lines, want the 6 and one per cause past the interval", n)
	}

	// Each is reported and counted, whatever was logged; a session with no
	// logger counts its own too.
	quiet := &Session{Mgr: mgr, Orphans: spans, Mocks: mocks, Ctx: context.Background()}
	quiet.MarkMockIncomplete("memory_pressure")
	_ = quiet.EmitMock(mock("left-out-quietly", base.Add(1000*time.Hour)))
	leftOut++
	if c, _ := spans.Counts(); c != leftOut {
		t.Fatalf("the spans hold %d exchanges, want each of the %d left out", c, leftOut)
	}
	if n := mgr.MocksLeftOut(); n != int64(leftOut) {
		t.Fatalf("the manager counts %d mocks left out, want each of the %d", n, leftOut)
	}
	if len(mocks) != 1 {
		t.Fatalf("%d mocks emitted, want none of those left out", len(mocks))
	}
}

// A cause is the reason up to its first colon, trimmed, and bounded: a
// parser's error text after the colon is not part of it.
func TestALeftOutCauseIsTheReasonUpToItsFirstColon(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 2*maxLeftOutCause)
	for _, tc := range []struct{ reason, cause string }{
		{"memory_pressure", "memory_pressure"},
		{"short_write", "short_write"},
		{"http decode error: request read failed: EOF", "http decode error"},
		{"http decode error: response read failed: invalid content-length", "http decode error"},
		{"  tls upgrade failed ", "tls upgrade failed"},
		{"", leftOutOther},
		{": nothing before it", leftOutOther},
		{long, long[:maxLeftOutCause]},
	} {
		if got := leftOutCause(tc.reason); got != tc.cause {
			t.Errorf("leftOutCause(%q) = %q, want %q", tc.reason, got, tc.cause)
		}
	}
}

// The WARN keeps at most maxOpenKinds causes apart, process-wide, so causes a
// parser builds from varying text cannot grow it without bound. A cause past
// them is limited by its class, so it is still said at WARN, with its own
// knob, and is never silent.
//
// Not parallel: the WARN limit is process-wide, and this test replaces it.
func TestALeftOutWarnPastTheCausesKeptIsLimitedByItsClass(t *testing.T) {
	clock := time.Now()
	prev := leftOutWarns
	leftOutWarns = &WarnLimiters{every: leftOutWarnEvery, now: func() time.Time { return clock }}
	t.Cleanup(func() { leftOutWarns = prev })

	core, logs := observer.New(zapcore.DebugLevel)
	s := &Session{Mgr: syncMock.New(nil), Orphans: &rawSpans{}, Mocks: make(chan *models.Mock, 1),
		Logger: zap.New(core), Ctx: context.Background()}
	at := time.Now().Add(-time.Minute)
	leaveOut := func(reason string) {
		t.Helper()
		m := &models.Mock{Name: "left-out"}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
		s.MarkMockIncomplete(reason)
		if err := s.EmitMock(m); err != nil {
			t.Fatalf("EmitMock: %v", err)
		}
	}
	warns := func() []observer.LoggedEntry { return logs.FilterLevelExact(zapcore.WarnLevel).All() }

	for i := 0; i < maxOpenKinds; i++ {
		leaveOut(fmt.Sprintf("decode error %d", i))
	}
	if n := len(warns()); n != maxOpenKinds {
		t.Fatalf("%d WARN lines for %d causes, want one each", n, maxOpenKinds)
	}
	for _, tc := range []struct {
		reason, class string
		warned        bool
	}{
		{"decode error new", leftOutOther, true},         // the first of class other
		{"decode error newer", leftOutOther, false},      // held behind it
		{"memory_pressure", leftOutMemoryPressure, true}, // its own class, its own knob
		{"decode error 0: again", leftOutOther, false},   // a cause kept: held behind its own WARN
	} {
		n := len(warns())
		leaveOut(tc.reason)
		w := warns()
		if warned := len(w) == n+1; warned != tc.warned {
			t.Fatalf("%q said at WARN: %v, want %v", tc.reason, warned, tc.warned)
		}
		if tc.warned && w[len(w)-1].ContextMap()["next_step"] != leftOutNextStep(tc.class, true) {
			t.Fatalf("%q is said with next_step %v, want its class's", tc.reason, w[len(w)-1].ContextMap()["next_step"])
		}
	}
	kinds := 0
	leftOutWarns.kinds.Range(func(_, _ any) bool { kinds++; return true })
	if kinds != maxOpenKinds+2 {
		t.Fatalf("the WARN limit keeps %d kinds, want the %d causes and the 2 classes used past them", kinds, maxOpenKinds)
	}
}

// The call a parser makes when it drops a mock for the incomplete flag without
// emitting it, made the way a parser outside this repository makes it: a mock
// with only its kind and its exchange's times, LeaveOutIfIncomplete, and
// return. The mock is reported once: one span over its exchange, one count,
// one WARN, pending work cleared once. The next exchange's mock is emitted and
// recorded, and reports nothing.
func TestAParserDroppingAFlaggedMockReportsItOnce(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	spans := &rawSpans{}
	mgr := syncMock.New(nil)
	mocks := make(chan *models.Mock, 2)
	s := &Session{Mgr: mgr, Orphans: spans, Mocks: mocks, Logger: zap.New(core), Ctx: context.Background()}
	cleared := 0
	s.OnPendingCleared = func() { cleared++ }

	at := time.Now().Add(-time.Minute)
	parse := func(name string, req time.Time) {
		res := req.Add(time.Millisecond)
		if s.IsMockIncomplete() {
			m := &models.Mock{Kind: models.REDIS}
			m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
			if s.LeaveOutIfIncomplete(m) {
				return
			}
		}
		m := &models.Mock{Name: name, Kind: models.REDIS}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
		if err := s.EmitMock(m); err != nil {
			t.Fatalf("EmitMock: %v", err)
		}
	}

	s.MarkMockIncomplete("redis decode error: short frame")
	parse("dropped", at)
	if cleared != 1 {
		t.Fatalf("pending work cleared %d time(s) for the mock dropped, want once", cleared)
	}
	parse("kept", at.Add(time.Second))

	if len(spans.got) != 1 || !spans.got[0][0].Equal(at) || !spans.got[0][1].Equal(at.Add(time.Millisecond)) {
		t.Fatalf("Orphans got %v, want the one exchange dropped, over its own times", spans.got)
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the one dropped", n)
	}
	reported := logs.FilterMessage(leftOutWarnMsg).Len() + logs.FilterMessage(leftOutDebugMsg).Len()
	if reported != 1 {
		t.Fatalf("the mock dropped is logged %d times, want once", reported)
	}
	close(mocks)
	var names []string
	for m := range mocks {
		names = append(names, m.Name)
	}
	if len(names) != 1 || names[0] != "kept" {
		t.Fatalf("recorded mocks %q, want only the next exchange's", names)
	}
}

// A parser that carries several exchanges at once (an HTTP/2 parser, say)
// takes the flag itself, decides which exchanges the mark hit, and reports
// each one it leaves out with ReportLeftOut and the reason it took. Each is
// reported as LeaveOutIfIncomplete reports one: a span over its own exchange,
// a count on the manager, and pending work cleared, as an emitted mock clears
// it. One WARN names the cause; the rest of it is held back to DEBUG. Reporting
// does not touch the flag, so a mark set since it was taken is the next
// mock's. RecordOrphanWindow alone, which the parser API named before, counted
// none of them and logged nothing.
func TestAParserLeavingOutSeveralExchangesForOneMarkReportsEach(t *testing.T) {
	clock := time.Unix(5000, 0)
	prev := leftOutWarns
	leftOutWarns = &WarnLimiters{every: leftOutWarnEvery, now: func() time.Time { return clock }}
	t.Cleanup(func() { leftOutWarns = prev })

	core, logs := observer.New(zapcore.DebugLevel)
	spans := &rawSpans{}
	mgr := syncMock.New(nil)
	s := &Session{Mgr: mgr, Orphans: spans, Logger: zap.New(core), Ctx: context.Background(), ClientConnID: "h2-conn"}
	cleared := 0
	s.OnPendingCleared = func() { cleared++ }

	at := time.Now().Add(-time.Minute)
	streams := [][2]time.Time{
		{at, at.Add(3 * time.Millisecond)},
		{at.Add(time.Millisecond), at.Add(5 * time.Millisecond)},
	}
	s.MarkMockIncomplete("memory_pressure")
	reason, ok := s.TakeMockIncomplete()
	if !ok || reason != "memory_pressure" {
		t.Fatalf("took (%q, %v), want the reason that set the flag", reason, ok)
	}
	for i, st := range streams {
		if i == 1 {
			s.MarkMockIncomplete("short_write")
		}
		m := &models.Mock{Kind: models.HTTP2}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = st[0], st[1]
		s.ReportLeftOut(m, reason)
	}

	if len(spans.got) != len(streams) {
		t.Fatalf("Orphans got %d spans, want one for each exchange left out", len(spans.got))
	}
	for i, st := range streams {
		if got := spans.got[i]; !got[0].Equal(st[0]) || !got[1].Equal(st[1]) {
			t.Errorf("exchange %d is reported over [%v, %v], want its own [%v, %v]", i, got[0], got[1], st[0], st[1])
		}
	}
	if n := mgr.MocksLeftOut(); n != 2 {
		t.Fatalf("the manager counts %d mocks left out, want both exchanges", n)
	}
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("logged %d WARNs, want one for the cause", len(warns))
	}
	w := warns[0].ContextMap()
	if w["reason"] != "memory_pressure" || w["connID"] != "h2-conn" || w["kind"] != string(models.HTTP2) {
		t.Fatalf("the WARN says %v, want the reason taken, the connection and the kind", w)
	}
	if n := logs.FilterLevelExact(zapcore.DebugLevel).FilterMessage(leftOutDebugMsg).Len(); n != 1 {
		t.Fatalf("logged %d held-back DEBUG lines, want the second exchange's", n)
	}
	if cleared != 2 {
		t.Fatalf("pending work cleared %d time(s), want once for each exchange left out", cleared)
	}
	if r, ok := s.TakeMockIncomplete(); !ok || r != "short_write" {
		t.Fatalf("took (%q, %v) after the reports, want the mark set since: reporting must not take it", r, ok)
	}

	// A nil mock still counts: its flag was taken. It carries no time, so no
	// span is recorded for it. A nil session is a no-op.
	s.ReportLeftOut(nil, reason)
	if n := mgr.MocksLeftOut(); n != 3 {
		t.Fatalf("a nil mock left out is counted as %d, want 3", n-2)
	}
	if len(spans.got) != len(streams) {
		t.Fatalf("a nil mock is reported over %v, want no span: it carries no time", spans.got[len(streams):])
	}
	var none *Session
	none.ReportLeftOut(&models.Mock{}, reason)
}

// A parser that knows which exchange it cannot record reports that exchange
// with ReportLeftOut and leaves the flag alone (MarkMockIncomplete's doc): the
// exchange's span is reported, it is counted and said at WARN with the
// parser's reason, and pending work is cleared, as for a mock the flag leaves
// out. Nothing else is left out for it: the next mock is recorded. Marking the
// flag instead would leave out that next mock, which may be a healthy one; and
// RecordOrphanWindow alone would report the span with nothing counting or
// saying the loss. A mark the relay set meanwhile is still the next mock's.
func TestAParserReportingAnExchangeItCannotRecordLeavesTheFlagAlone(t *testing.T) {
	prev := leftOutWarns
	leftOutWarns = NewWarnLimiters(leftOutWarnEvery)
	t.Cleanup(func() { leftOutWarns = prev })

	core, logs := observer.New(zapcore.WarnLevel)
	spans := &rawSpans{}
	mgr := syncMock.New(nil)
	mocks := make(chan *models.Mock, 2)
	s := &Session{Mgr: mgr, Orphans: spans, Mocks: mocks, Logger: zap.New(core), Ctx: context.Background()}
	cleared := 0
	s.OnPendingCleared = func() { cleared++ }

	at := time.Now().Add(-time.Minute)
	exchange := func(name string, from time.Duration) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.MySQL}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at.Add(from), at.Add(from+time.Millisecond)
		return m
	}
	s.ReportLeftOut(exchange("undecodable", 0), "mysql decode error: bad packet")
	if len(spans.got) != 1 || !spans.got[0][0].Equal(at) || !spans.got[0][1].Equal(at.Add(time.Millisecond)) {
		t.Fatalf("Orphans got %v, want the exchange's own span", spans.got)
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the one reported", n)
	}
	if w := logs.All(); len(w) != 1 || w[0].ContextMap()["reason"] != "mysql decode error: bad packet" {
		t.Fatalf("logged %v at WARN, want the one exchange with the parser's reason", w)
	}
	if cleared != 1 {
		t.Fatalf("pending work cleared %d time(s), want once", cleared)
	}
	if s.IsMockIncomplete() {
		t.Fatal("reporting an exchange set the flag")
	}
	if err := s.EmitMock(exchange("next", time.Second)); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if len(mocks) != 1 || len(spans.got) != 1 {
		t.Fatalf("the next mock was left out (%d emitted, %d spans): only the exchange reported may be", len(mocks), len(spans.got))
	}

	// A mark the relay set before the report is the next mock's.
	s.MarkMockIncomplete("per_conn_cap")
	s.ReportLeftOut(exchange("undecodable-2", 2*time.Second), "mysql decode error: bad packet")
	if !s.IsMockIncomplete() {
		t.Fatal("reporting an exchange took the relay's mark")
	}
	if err := s.EmitMock(exchange("voided", 3*time.Second)); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if len(mocks) != 1 || len(spans.got) != 3 || mgr.MocksLeftOut() != 3 {
		t.Fatalf("after the relay's mark: %d emitted, %d spans, %d counted; want the next mock left out and reported",
			len(mocks), len(spans.got), mgr.MocksLeftOut())
	}
}

// With no Orphans, the exchange goes to the session's manager, where
// routes/record.go checks each test case (WasMockOrphanedInWindow).
func TestAMockLeftOutIsReportedToTheManagerWithoutOrphans(t *testing.T) {
	mgr := syncMock.New(nil)
	s := &Session{Mgr: mgr, Mocks: make(chan *models.Mock, 1), Ctx: context.Background()}
	at := time.Now().Add(-time.Minute)
	m := &models.Mock{Name: "left-out"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	s.MarkMockIncomplete("short_write")
	if err := s.EmitMock(m); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if over, _ := mgr.WasMockOrphanedInWindow(at, at.Add(time.Millisecond)); !over {
		t.Fatal("the manager does not leave out a test case over the exchange whose mock was left out")
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the one", n)
	}
}

// A session's mocks, the mocks it leaves out and its spans reach one manager
// (Session.manager): Mgr when it is set, leaving the package-global manager
// untouched, else the package-global one. The recording's summary counts the
// mocks left out on the manager its mocks go to, and routes/record.go checks
// test cases against that manager's spans, so a session that split them would
// count a mock, or span its exchange, where nothing reads it.
// Not parallel: it reads the package-global manager's counters, which this
// package's parallel tests also add to.
func TestASessionsMocksCountsAndSpansReachOneManager(t *testing.T) {
	type tally struct {
		added, leftOut int64
		spans          int
	}
	read := func(m *syncMock.SyncMockManager) tally {
		_, _, added, _ := m.GetDropStats()
		spans, _, _ := m.OrphanRangeCount()
		return tally{added: added, leftOut: m.MocksLeftOut(), spans: spans}
	}
	since := func(before, after tally) tally {
		return tally{added: after.added - before.added, leftOut: after.leftOut - before.leftOut, spans: after.spans - before.spans}
	}
	// One mock kept; two left out, one of them spanned; and one hole spanned.
	use := func(s *Session) {
		at := time.Now().Add(-time.Minute)
		left := &models.Mock{Name: "left-out"}
		left.Spec.ReqTimestampMock, left.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
		s.MarkMockIncomplete("short_write")
		if err := s.EmitMock(left); err != nil {
			t.Fatalf("EmitMock of the flagged mock: %v", err)
		}
		s.ReportLeftOut(&models.Mock{Name: "no-times"}, "decode_error")
		s.OpenOrphanWindow(at.Add(time.Second))()
		if err := s.EmitMock(&models.Mock{Name: "kept"}); err != nil {
			t.Fatalf("EmitMock of the kept mock: %v", err)
		}
	}
	want := tally{added: 1, leftOut: 2, spans: 2}

	global := syncMock.Get()
	mgr := syncMock.New(nil)
	before := read(global)
	use(&Session{Mgr: mgr, RouteMocksViaSyncMock: true, Ctx: context.Background()})
	if got := read(mgr); got != want {
		t.Errorf("with Mgr set, Mgr got %+v, want %+v", got, want)
	}
	if got := since(before, read(global)); got != (tally{}) {
		t.Errorf("with Mgr set, the package-global manager got %+v, want nothing", got)
	}

	before = read(global)
	use(&Session{RouteMocksViaSyncMock: true, Ctx: context.Background()})
	if got := since(before, read(global)); got != want {
		t.Errorf("with Mgr nil, the package-global manager got %+v, want %+v", got, want)
	}
}

// rawSpans is an OrphanSpans that keeps each span as it was handed over: a
// caller's own Session.Orphans need not clamp what it is given, as
// *syncMock.Spans does.
// EmitMockOnShutdown keeps EmitMock's incomplete-mock gate. A parser that
// flushes a connection's last mocks as the recording shuts down (postgres v3,
// and parsers outside this repository) can hand it a partial mock the flag
// marked: it is left out and reported exactly as EmitMock reports one (its
// span once, a count, pending work cleared, a WARN with the reason), and
// never delivered, whichever way the session routes its mocks. The flag is
// taken, so the next mock flushed is delivered. The session's ctx is
// cancelled, as it is at shutdown.
// Not parallel: the WARN limit is process-wide, and this test sets its clock.
func TestEmitMockOnShutdownLeavesOutAndReportsAMockMarkedIncomplete(t *testing.T) {
	clock := time.Unix(7000, 0)
	prev := leftOutWarns
	leftOutWarns = &WarnLimiters{every: leftOutWarnEvery, now: func() time.Time { return clock }}
	t.Cleanup(func() { leftOutWarns = prev })

	for _, viaSyncMock := range []bool{false, true} {
		t.Run(fmt.Sprintf("viaSyncMock=%v", viaSyncMock), func(t *testing.T) {
			leftOutWarns.Reset()
			core, logs := observer.New(zapcore.DebugLevel)
			spans := &rawSpans{}
			mgr := syncMock.New(nil)
			mocks := make(chan *models.Mock, 2)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s := &Session{Mgr: mgr, Orphans: spans, Mocks: mocks, Logger: zap.New(core), Ctx: ctx,
				ClientConnID: "pg-conn", RouteMocksViaSyncMock: viaSyncMock}
			cleared := 0
			s.OnPendingCleared = func() { cleared++ }
			delivered := func() int64 {
				if viaSyncMock {
					_, _, added, _ := mgr.GetDropStats()
					return added
				}
				return int64(len(mocks))
			}

			at := time.Now().Add(-time.Minute)
			partial := &models.Mock{Name: "partial", Kind: models.PostgresV3}
			partial.Spec.ReqTimestampMock, partial.Spec.ResTimestampMock = at, at.Add(4*time.Millisecond)
			s.MarkMockIncomplete("per_conn_cap")
			if err := s.EmitMockOnShutdown(partial); err != nil {
				t.Fatalf("EmitMockOnShutdown(a mock marked incomplete) = %v, want nil: it is left out, not failed", err)
			}
			if n := delivered(); n != 0 {
				t.Fatalf("%d mock(s) delivered: the partial mock flushed at shutdown was recorded", n)
			}
			if len(spans.got) != 1 || !spans.got[0][0].Equal(at) || !spans.got[0][1].Equal(at.Add(4*time.Millisecond)) {
				t.Fatalf("Orphans got %v, want the partial mock's exchange once", spans.got)
			}
			if n := mgr.MocksLeftOut(); n != 1 {
				t.Fatalf("the manager counts %d mocks left out, want the partial one", n)
			}
			if cleared != 1 {
				t.Fatalf("pending work cleared %d time(s), want once", cleared)
			}
			w := logs.FilterLevelExact(zapcore.WarnLevel).All()
			if len(w) != 1 || w[0].ContextMap()["reason"] != "per_conn_cap" || w[0].ContextMap()["mockName"] != "partial" {
				t.Fatalf("logged %d WARN(s) (%v), want one for the partial mock, with its reason", len(w), w)
			}
			if s.IsMockIncomplete() {
				t.Fatal("the flag is still set after the mock it left out: it was not taken")
			}

			next := &models.Mock{Name: "whole", Kind: models.PostgresV3}
			next.Spec.ReqTimestampMock, next.Spec.ResTimestampMock = at.Add(time.Second), at.Add(time.Second+time.Millisecond)
			if err := s.EmitMockOnShutdown(next); err != nil {
				t.Fatalf("EmitMockOnShutdown(the next mock) = %v", err)
			}
			if n := delivered(); n != 1 {
				t.Fatalf("%d mock(s) delivered, want the next one: the flag left it out too", n)
			}
			if !viaSyncMock {
				if got := <-mocks; got != next {
					t.Fatalf("delivered %q, want the next mock", got.Name)
				}
			}
			if len(spans.got) != 1 || mgr.MocksLeftOut() != 1 || cleared != 2 {
				t.Fatalf("after the next mock: %d span(s), %d left out, pending cleared %d times; want 1, 1, 2",
					len(spans.got), mgr.MocksLeftOut(), cleared)
			}
		})
	}
}

type rawSpans struct{ got [][2]time.Time }

func (r *rawSpans) Record(start, end time.Time) { r.got = append(r.got, [2]time.Time{start, end}) }
func (r *rawSpans) Open(time.Time) func()       { return func() {} }

// A mock left out is spanned over its own times, one rule: [request,
// response], ending where it starts when it has no response time or one before
// its request's. One with no request time gets no span: no time is invented
// for it. The moment it is reported is not its exchange's (the parser runs
// behind the traffic), so a span there would leave out the test cases in
// flight then, which did not use it, and miss the ones that did. It is still
// counted and logged, with spanRecorded false, and its next_step says that no
// test case was left out for it, not that the ones not saved yet were. So
// Session.Orphans is never handed a span with no start, nor one that ends
// before it starts.
func TestAMockLeftOutIsSpannedOverItsOwnTimesOnly(t *testing.T) {
	prev := leftOutWarns
	leftOutWarns = NewWarnLimiters(leftOutWarnEvery)
	t.Cleanup(func() { leftOutWarns = prev })
	core, logs := observer.New(zapcore.DebugLevel)
	spans := &rawSpans{}
	mgr := syncMock.New(nil)
	s := &Session{Orphans: spans, Mgr: mgr, Logger: zap.New(core), Mocks: make(chan *models.Mock, 4), Ctx: context.Background()}
	at := time.Now().Add(-time.Minute)
	none := [2]time.Time{}
	// The first is said at WARN, the rest of its cause at DEBUG.
	for _, tc := range []struct {
		name     string
		req, res time.Time
		span     [2]time.Time
	}{
		{"answered-only", time.Time{}, at, none},
		{"untimed", time.Time{}, time.Time{}, none},
		{"unanswered", at, time.Time{}, [2]time.Time{at, at}},
		{"answered-before-asked", at, at.Add(-time.Second), [2]time.Time{at, at}},
		{"answered", at, at.Add(time.Second), [2]time.Time{at, at.Add(time.Second)}},
	} {
		spans.got = nil
		logs.TakeAll()
		counted := mgr.MocksLeftOut()
		m := &models.Mock{Name: tc.name}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = tc.req, tc.res
		s.MarkMockIncomplete("short_write")
		_ = s.EmitMock(m)
		if n := mgr.MocksLeftOut() - counted; n != 1 {
			t.Fatalf("%s: counted %d, want the mock left out once", tc.name, n)
		}
		switch {
		case tc.span == none && len(spans.got) != 0:
			t.Fatalf("%s: spanned over %v, want no span: the mock carries no request time", tc.name, spans.got)
		case tc.span != none && (len(spans.got) != 1 || !spans.got[0][0].Equal(tc.span[0]) || !spans.got[0][1].Equal(tc.span[1])):
			t.Fatalf("%s: spanned over %v, want [%v, %v]", tc.name, spans.got, tc.span[0], tc.span[1])
		}
		lines := logs.Filter(func(e observer.LoggedEntry) bool {
			return e.Message == leftOutWarnMsg || e.Message == leftOutDebugMsg
		}).All()
		if len(lines) != 1 {
			t.Fatalf("%s: %d left-out lines, want one", tc.name, len(lines))
		}
		if got, want := lines[0].ContextMap()["spanRecorded"], tc.span != none; got != want {
			t.Fatalf("%s: spanRecorded is %v, want %v", tc.name, got, want)
		}
	}

	// The first, with no span, was the WARN: its next_step promises no test
	// case was left out for it.
	s.MarkMockIncomplete("short_write")
	leftOutWarns = NewWarnLimiters(leftOutWarnEvery)
	logs.TakeAll()
	_ = s.EmitMock(&models.Mock{Name: "untimed"})
	w := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(leftOutWarnMsg).All()
	if len(w) != 1 {
		t.Fatalf("%d left-out WARNs, want 1", len(w))
	}
	next, _ := w[0].ContextMap()["next_step"].(string)
	if next != leftOutNextStep(leftOutOther, false) || !strings.Contains(next, "no span was recorded") ||
		!strings.Contains(next, "no test case is left out for it") || strings.Contains(next, "not saved yet") {
		t.Fatalf("the next_step of a mock with no span is %q, want one that says no test case was left out for it", next)
	}
}

// A parser that stops on an exchange it cannot record, and returns, reports it
// with ReportStoppedOn: one rule for every parser (the HTTP/1 recorder's
// request or response that does not decode, the MySQL recorder's lost
// framing). It is counted, said at WARN and cleared from the pending work like
// any mock left out, and spanned from its request to the stop, not to the last
// byte the parser read of it: the parser runs behind its connection, and the
// span must meet the one opened for what the connection carries after the
// stop. A mark on the flag is taken with it, since no later EmitMock would take
// it, and the exchange is reported once, under the mark's cause; with none,
// under the parser's reason, and the flag stays clear. The stop is the
// session's stop instant (StoppedAt), the one that span starts at: read before
// the report (by the abort that retired a parser still running, or at a capture
// hole), it is that earlier instant. A request with no time is counted with no
// span, and one stamped after the clock reads ends where it starts.
func TestAParserStoppingOnAnExchangeReportsItUpToTheStop(t *testing.T) {
	prev := leftOutWarns
	t.Cleanup(func() { leftOutWarns = prev })
	type run struct {
		s       *Session
		spans   *rawSpans
		mgr     *syncMock.SyncMockManager
		logs    *observer.ObservedLogs
		cleared int
	}
	newRun := func() *run {
		leftOutWarns = NewWarnLimiters(leftOutWarnEvery)
		core, logs := observer.New(zapcore.DebugLevel)
		r := &run{spans: &rawSpans{}, mgr: syncMock.New(nil), logs: logs}
		r.s = &Session{Orphans: r.spans, Mgr: r.mgr, Logger: zap.New(core), ClientConnID: "stopped"}
		r.s.OnPendingCleared = func() { r.cleared++ }
		return r
	}
	warned := func(t *testing.T, r *run) map[string]any {
		t.Helper()
		w := r.logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(leftOutWarnMsg).All()
		if len(w) != 1 {
			t.Fatalf("%d left-out WARNs, want one", len(w))
		}
		return w[0].ContextMap()
	}
	at := time.Now().Add(-time.Minute)

	t.Run("its own reason", func(t *testing.T) {
		r := newRun()
		before := time.Now()
		r.s.ReportStoppedOn(models.HTTP, at, "http decode error: response read failed: bad")
		after := time.Now()
		if len(r.spans.got) != 1 {
			t.Fatalf("%d spans, want the exchange's", len(r.spans.got))
		}
		if sp := r.spans.got[0]; !sp[0].Equal(at) || sp[1].Before(before) || sp[1].After(after) {
			t.Fatalf("the exchange is spanned over [%v, %v], want from its request (%v) to the stop (between %v and %v)",
				sp[0], sp[1], at, before, after)
		}
		if stop := r.s.StoppedAt(); !r.spans.got[0][1].Equal(stop) {
			t.Fatalf("the exchange is spanned up to %v, want the session's stop instant (%v), where the span after the stop starts", r.spans.got[0][1], stop)
		}
		if n := r.mgr.MocksLeftOut(); n != 1 || r.cleared != 1 {
			t.Fatalf("counted %d, pending work cleared %d times; want 1 and 1", n, r.cleared)
		}
		w := warned(t, r)
		if w["reason"] != "http decode error: response read failed: bad" || w["kind"] != string(models.HTTP) ||
			w["spanRecorded"] != true || w["next_step"] != leftOutNextStep(leftOutOther, true) {
			t.Fatalf("the WARN says %v, want the parser's reason, the HTTP kind, its span and class other's next_step", w)
		}
		if r.s.IsMockIncomplete() {
			t.Fatal("the flag is set after a stop with no mark")
		}
	})
	t.Run("under the relay's mark", func(t *testing.T) {
		r := newRun()
		r.s.MarkMockIncomplete(relay.DropPerConnCap)
		r.s.ReportStoppedOn(models.MySQL, at, "mysql exchange not recorded: framing lost")
		if n := r.mgr.MocksLeftOut(); n != 1 || len(r.spans.got) != 1 {
			t.Fatalf("counted %d, %d spans; want the one exchange, once", n, len(r.spans.got))
		}
		w := warned(t, r)
		if w["reason"] != relay.DropPerConnCap+": mysql exchange not recorded: framing lost" || w["kind"] != string(models.MySQL) ||
			w["next_step"] != leftOutNextStep(leftOutPerConnCap, true) {
			t.Fatalf("the WARN says %v, want the mark's cause and per_conn_cap's next_step, then the parser's reason", w)
		}
		if reason, ok := r.s.TakeMockIncomplete(); ok {
			t.Fatalf("the mark %q was left on the flag: nothing takes it once the parser returns", reason)
		}
	})
	t.Run("a stop read first, at a retirement or a hole", func(t *testing.T) {
		r := newRun()
		stopped := r.s.StoppedAt()
		time.Sleep(time.Millisecond)
		r.s.ReportStoppedOn(models.HTTP, at, "http decode error: x")
		if len(r.spans.got) != 1 || !r.spans.got[0][1].Equal(stopped) {
			t.Fatalf("spanned over %v, want up to the earlier stop (%v), where the span after it starts", r.spans.got, stopped)
		}
	})
	t.Run("a request with no time", func(t *testing.T) {
		r := newRun()
		r.s.ReportStoppedOn(models.HTTP, time.Time{}, "http decode error: x")
		if len(r.spans.got) != 0 {
			t.Fatalf("spanned over %v, want no span: no time is invented for the request", r.spans.got)
		}
		if n := r.mgr.MocksLeftOut(); n != 1 {
			t.Fatalf("counted %d, want 1", n)
		}
		if w := warned(t, r); w["spanRecorded"] != false || w["next_step"] != leftOutNextStep(leftOutOther, false) {
			t.Fatalf("the WARN says %v, want no span recorded and its next_step", w)
		}
	})
	t.Run("a request stamped after the clock reads", func(t *testing.T) {
		r := newRun()
		ahead := time.Now().Add(time.Hour)
		r.s.ReportStoppedOn(models.HTTP, ahead, "http decode error: x")
		if len(r.spans.got) != 1 || !r.spans.got[0][0].Equal(ahead) || !r.spans.got[0][1].Equal(ahead) {
			t.Fatalf("spanned over %v, want [%v, %v]: a span never ends before it starts", r.spans.got, ahead, ahead)
		}
	})
	var none *Session
	none.ReportStoppedOn(models.HTTP, at, "http decode error: x")
}

// A connection's stop is read from the clock once, at the first StoppedAt,
// and every later call, from any goroutine, gets that same instant: the parser
// ends the span of the exchange it stopped on there, and the span of what the
// connection carries after the stop starts there, whether the parser's
// retirement or a capture hole opens it, so nothing captured between the two
// is in neither.
func TestStoppedAtIsReadFromTheClockOnce(t *testing.T) {
	s := &Session{}
	before := time.Now()
	first := s.StoppedAt()
	after := time.Now()
	if first.Before(before.Round(0)) || first.After(after.Round(0)) {
		t.Fatalf("the first StoppedAt is %v, want the clock as it was called (between %v and %v)", first, before, after)
	}
	const callers = 8
	got := make(chan time.Time, callers)
	for i := 0; i < callers; i++ {
		go func() {
			time.Sleep(time.Millisecond)
			got <- s.StoppedAt()
		}()
	}
	for i := 0; i < callers; i++ {
		if g := <-got; !g.Equal(first) {
			t.Fatalf("a later StoppedAt is %v, want the first's instant, %v", g, first)
		}
	}
	racing := &Session{}
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			got <- racing.StoppedAt()
		}()
	}
	close(start)
	want := <-got
	for i := 1; i < callers; i++ {
		if g := <-got; !g.Equal(want) {
			t.Fatalf("callers racing to the first StoppedAt got %v and %v, want one instant", want, g)
		}
	}
	var none *Session
	if !none.StoppedAt().IsZero() {
		t.Fatal("a nil session has a stop instant")
	}
}

// The call that stamps the stop tells OnStop, once, with the instant every
// call returns, before it returns itself: the dispatcher opens the span of what
// the connection carries after the stop there, so the span is open from the
// moment of the stop, whichever event stamped it.
func TestStoppedAtTellsOnStopOnceAsItStamps(t *testing.T) {
	var mu sync.Mutex
	var told []time.Time
	s := &Session{OnStop: func(at time.Time) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, at)
	}}
	first := s.StoppedAt()
	mu.Lock()
	if len(told) != 1 || !told[0].Equal(first) {
		mu.Unlock()
		t.Fatalf("OnStop was told %v by the time the call that stamped the stop returned, want once, with %v", told, first)
	}
	mu.Unlock()
	const callers = 8
	got := make(chan time.Time, callers)
	for i := 0; i < callers; i++ {
		go func() { got <- s.StoppedAt() }()
	}
	for i := 0; i < callers; i++ {
		<-got
	}
	racing := &Session{}
	var calls atomic.Int32
	racing.OnStop = func(time.Time) { calls.Add(1) }
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			got <- racing.StoppedAt()
		}()
	}
	close(start)
	for i := 0; i < callers; i++ {
		<-got
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 {
		t.Fatalf("OnStop was told %d times, want once", len(told))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("callers racing to the first StoppedAt told OnStop %d times, want once", n)
	}
}

// LeaveOutIfIncomplete, for a parser that checks the flag itself, takes the
// flag with the reason that set it: the first since it was last cleared. With
// the flag clear it leaves nothing out and reports nothing; a nil mock leaves
// the flag for the next one; and a flag cleared by MarkMockComplete leaves
// nothing out.
func TestLeaveOutIfIncompleteTakesTheFlagWithItsReason(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	spans := &syncMock.Spans{}
	s := &Session{Orphans: spans, Logger: zap.New(core)}
	at := time.Now().Add(-time.Minute)
	m := &models.Mock{Name: "left-out"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	reasons := func() []string {
		var out []string
		for _, e := range logs.All() {
			if e.Message != "mock marked incomplete" {
				if r, ok := e.ContextMap()["reason"].(string); ok {
					out = append(out, r)
				}
			}
		}
		return out
	}

	if s.LeaveOutIfIncomplete(m) {
		t.Fatal("a mock was left out with the flag clear")
	}
	s.MarkMockIncomplete("memory_pressure")
	s.MarkMockIncomplete("short_write")
	if s.LeaveOutIfIncomplete(nil) || !s.IsMockIncomplete() {
		t.Fatal("a nil mock took the flag")
	}
	if !s.LeaveOutIfIncomplete(m) {
		t.Fatal("the mock was not left out with the flag set")
	}
	if s.IsMockIncomplete() {
		t.Fatal("the flag is still set after it was taken")
	}
	if s.LeaveOutIfIncomplete(m) {
		t.Fatal("a second mock was left out for one flag")
	}
	s.MarkMockIncomplete("write_error")
	s.MarkMockComplete()
	if s.LeaveOutIfIncomplete(m) {
		t.Fatal("a mock was left out for a flag MarkMockComplete cleared")
	}
	if got := reasons(); len(got) != 1 || got[0] != "memory_pressure" {
		t.Fatalf("mocks left out were said with reasons %q, want the one, with the reason that set its flag", got)
	}
	if c, _ := spans.Counts(); c != 1 {
		t.Fatalf("the spans hold %d exchanges, want the one left out", c)
	}
}

// TakeMockIncomplete, for a parser that decides itself which exchanges a mark
// hits, takes the flag with the reason that set it and reports nothing: that
// parser reports each exchange it leaves out. A clear flag gives nothing, one
// flag is taken once, and a mark set after the take is the next mock's.
func TestTakeMockIncompleteTakesTheFlagWithItsReason(t *testing.T) {
	spans := &syncMock.Spans{}
	mocks := make(chan *models.Mock, 2)
	s := &Session{Orphans: spans, Mocks: mocks, Ctx: context.Background()}

	if r, ok := s.TakeMockIncomplete(); ok || r != "" {
		t.Fatalf("took (%q, %v) from a clear flag", r, ok)
	}
	s.MarkMockIncomplete("memory_pressure")
	s.MarkMockIncomplete("short_write")
	if r, ok := s.TakeMockIncomplete(); !ok || r != "memory_pressure" {
		t.Fatalf("took (%q, %v), want the first reason that set the flag", r, ok)
	}
	if s.IsMockIncomplete() {
		t.Fatal("the flag is still set after it was taken")
	}
	if _, ok := s.TakeMockIncomplete(); ok {
		t.Fatal("one flag was taken twice")
	}
	if c, o := spans.Counts(); c != 0 || o != 0 {
		t.Fatalf("taking the flag reported spans (closed=%d, open=%d): the parser that takes it reports what it leaves out", c, o)
	}

	s.MarkMockIncomplete("write_error")
	at := time.Now().Add(-time.Minute)
	m := &models.Mock{Name: "next"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	if err := s.EmitMock(m); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if len(mocks) != 0 {
		t.Fatal("a mark set after the take did not leave out the next mock")
	}
	if c, _ := spans.Counts(); c != 1 {
		t.Fatalf("the spans hold %d exchanges, want the next mock's", c)
	}

	var none *Session
	if _, ok := none.TakeMockIncomplete(); ok {
		t.Fatal("a nil session took a flag")
	}
}

// The relay marks the flag for every chunk it drops: each memory_pressure drop,
// and every chunk of a desynced connection for the rest of its life. While the
// flag is set those calls are no-ops and must cost nothing, or the agent
// allocates per dropped chunk at the moment it is short of memory.
func TestMarkMockIncompleteCostsNothingWhileTheFlagIsSet(t *testing.T) {
	s := &Session{}
	s.MarkMockIncomplete("memory_pressure")
	allocs := testing.AllocsPerRun(100, func() {
		s.MarkMockIncomplete("per_conn_cap")
	})
	if allocs != 0 {
		t.Fatalf("marking a flag already set allocated %v times per call, want none", allocs)
	}
	if !s.IsMockIncomplete() {
		t.Fatal("the flag was cleared")
	}
}

// The left-out WARN tells the user what to do: that the app's traffic is
// unaffected, what happens to the test cases recorded over the exchange, and
// the knob that fits the reason. The reasons are the relay's drop reasons,
// which it hands MarkMockIncomplete verbatim.
//
// It promises only what holds. A test case is checked against the exchange's
// span when the recording saves it, and the span is reported when the parser
// reaches the exchange, which runs behind the traffic: in proxy mode, with no
// watermark to hold test cases, one can be saved before then (routes'
// TestHandleIncoming_AMockLeftOutReachesOnlyTestCasesNotYetStreamed). So the
// WARN says that test cases not yet saved are left out, and that one saved
// before lacks the mock and fails replay with no_mocks. For per_conn_cap, the
// parser is a full buffer behind, and the mock the flag leaves out is the next
// one it parses, which can be from an exchange before the lost chunk.
//
// desynced is what the relay hands for each later chunk on a direction that
// lost one to per_conn_cap or memory_pressure, when its parser cannot resync:
// it refuses the chunk, and the mock the parser emits next, from what it
// captured before the loss, is left out for it. A re-run with --debug shows
// only reason=desynced, so its WARN points at the loss that stopped the
// capture and that loss's knobs.
func TestTheLeftOutWarnPromisesOnlyWhatHolds(t *testing.T) {
	for _, tc := range []struct{ reason, knob string }{
		{relay.DropMemoryPressure, "--memory-limit"},
		{relay.DropPerConnCap, "record.recordBuffer.maxMemoryPerConnection"},
		{relay.DropDesynced, "record.recordBuffer.maxMemoryPerConnection"},
		{relay.DropDesynced, "--memory-limit"},
		{"short_write", "--debug"},
		{"http decode error: response read failed: invalid content-length", "--debug"},
	} {
		next := leftOutNextStep(leftOutClass(leftOutCause(tc.reason)), true)
		for _, want := range []string{"traffic is unaffected", "not saved yet", "are left out of the recording",
			"saved before then", "lacks this mock", "no_mocks", tc.knob} {
			if !strings.Contains(next, want) {
				t.Errorf("reason %q: next_step %q does not say %q", tc.reason, next, want)
			}
		}
		if strings.Contains(next, "rather than saved without") {
			t.Errorf("reason %q: next_step %q promises the test cases over the exchange are never saved without its mock", tc.reason, next)
		}
	}
	if next := leftOutNextStep(leftOutClass(relay.DropPerConnCap), true); !strings.Contains(next, "a full buffer behind") ||
		!strings.Contains(next, "before the lost chunk") {
		t.Errorf("per_conn_cap: next_step %q does not say the parser was a full buffer behind, nor that the mock left out can be from before the lost chunk", next)
	}
	if strings.Contains(leftOutNextStep(leftOutClass(relay.DropMemoryPressure), true), "--debug") ||
		strings.Contains(leftOutNextStep(leftOutClass(relay.DropPerConnCap), true), "--memory-limit") ||
		strings.Contains(leftOutNextStep(leftOutClass(relay.DropDesynced), true), "--debug") {
		t.Error("a reason with its own knob was given another reason's")
	}
	if next := leftOutNextStep(leftOutClass(relay.DropDesynced), true); !strings.Contains(next, "lost a chunk earlier") ||
		!strings.Contains(next, "reason=per_conn_cap or reason=memory_pressure") || !strings.Contains(next, "captured whole before the loss") {
		t.Errorf("desynced: next_step %q does not say that an earlier per_conn_cap or memory_pressure loss stopped the capture, nor that the mock left out was parsed from before it", next)
	}

	// The WARN itself says only that the exchange was not recorded.
	prev := leftOutWarns
	leftOutWarns = NewWarnLimiters(leftOutWarnEvery)
	t.Cleanup(func() { leftOutWarns = prev })
	core, logs := observer.New(zapcore.WarnLevel)
	s := &Session{Mgr: syncMock.New(nil), Orphans: &syncMock.Spans{}, Logger: zap.New(core)}
	at := time.Now().Add(-time.Minute)
	m := &models.Mock{Name: "left-out"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	s.ReportLeftOut(m, relay.DropPerConnCap)
	w := logs.All()
	if len(w) != 1 {
		t.Fatalf("%d WARN lines, want 1", len(w))
	}
	if strings.Contains(w[0].Message, "left out") || strings.Contains(w[0].Message, "test case") {
		t.Errorf("the WARN %q promises what happens to the test cases: whether they are left out depends on when they were saved", w[0].Message)
	}
	if w[0].ContextMap()["next_step"] != leftOutNextStep(leftOutPerConnCap, true) {
		t.Errorf("the WARN's next_step is %v, want per_conn_cap's", w[0].ContextMap()["next_step"])
	}
}
