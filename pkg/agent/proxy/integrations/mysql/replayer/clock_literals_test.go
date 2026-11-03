package replayer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// clockSQL is a statement an application builds with its clock's now() in
// its text, as Frappe does: a wall clock of no zone, cut at 1µs.
const clockSQL = "SELECT name FROM tabNote WHERE owner = 'a@x.io' AND expire_notification_on > '%s'"

const literalForm = "2006-01-02 15:04:05.000000"

// clockRun is one run's application clock and call: a tracker whose
// application clock is the real one, its test begun at began, and the
// call that reaches keploy at arrived, sent with a read made 1.3ms before,
// 20ms after the run's previous call.
type clockRun struct {
	tr      *learn.Tracker
	began   time.Time
	arrived time.Time
}

func newClockRun(began time.Time) clockRun {
	tr := learn.NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.NewSetAt(began.Add(-time.Second))
	tr.BeginAt(began)
	// The run's previous call, 20ms before this one, starts its span.
	tr.ServedMockAt(&models.Mock{Name: "mock-prev", Kind: models.MySQL}, began.Add(30*time.Millisecond))
	return clockRun{tr: tr, began: began, arrived: began.Add(50 * time.Millisecond)}
}

func (r clockRun) read() time.Time { return r.arrived.Add(-1300 * time.Microsecond) }

// ctx is the command's context: its connection, the run's tracker, and the
// command's arrival; learns says it is a call of record's full round.
func (r clockRun) ctx(learns bool) context.Context {
	ctx, _ := poolConn("c1")
	return replayer.WithCommandForTest(learn.WithTracker(ctx, r.tr), r.arrived, learns)
}

// recordedClock is the recording of clockSQL made with a read 1.3ms before
// its call, and the read the learn engine learns its literal as, where the
// full round's call carries live (learn.ClassifyIn in the call's window).
func recordedClock(t *testing.T, recAt time.Time, f clockRun, live string) (*models.Mock, models.ClockOffset) {
	t.Helper()
	recLit := recAt.Add(-1300 * time.Microsecond).Format(literalForm)
	m := diffMock("mock-note", "c1", []mysql.Request{queryReq(strings.Replace(clockSQL, "%s", recLit, 1))}, recAt, models.LifetimePerTest, "mocks")
	p := "body.query.literals.1"
	rec := learn.Shape{Leaves: map[string]learn.Leaf{p: {Path: p, Type: learn.String, Value: recLit}}}
	lv := learn.Shape{Leaves: map[string]learn.Leaf{p: {Path: p, Type: learn.String, Value: live}}}
	// The recording's half of the call's window starts at its previous
	// call, which the application made 20ms before it.
	prev := diffMock("mock-prev", "c1", []mysql.Request{queryReq("SELECT 1")}, recAt.Add(-20*time.Millisecond), models.LifetimePerTest, "mocks")
	for _, ln := range learn.ClassifyIn(nil, []string{p}, lv, rec, recAt, nil, nil, nil, f.tr.CallWindow(m, []*models.Mock{prev}, f.arrived)) {
		if o, ok := ln.Clock(); ok && (ln.Kind == learn.KindClockRead || ln.Kind == learn.KindClockOffset) {
			return m, o
		}
		t.Fatalf("the learn engine learns %s as %s, not a read of the application's clock: %s", live, ln.Kind, ln.Why)
	}
	t.Fatalf("the learn engine learns nothing at %s", p)
	return nil, models.ClockOffset{}
}

// A literal of a COM_QUERY the learn step learned as a read of the
// application's clock (MockSpec.Clocks) is checked as one under the proven
// switch: a later replay whose statement carries a read made in its own
// call's span on the application's clock is answered by the recording,
// though its text differs there; one whose literal lies outside it, or
// whose span keploy cannot tell, is refused, and the miss names the
// literal. The other literals are still compared, and without the switch
// the literal is compared as before it. The scan that starts in the open
// test's window and the one over the whole pool give the same answer.
func TestALiteralLearnedAsAClockReadIsCheckedAsOne(t *testing.T) {
	recAt := diffBase.Add(time.Millisecond)
	f := newClockRun(time.Now().UTC().Add(-time.Minute))
	_, o := recordedClock(t, recAt, f, f.read().Format(literalForm))
	later := newClockRun(time.Now().UTC())
	cases := []struct {
		name    string
		proven  bool
		sql     string
		noSpan  bool
		served  bool
		mention string
		// noWhy: refused at a literal that is no read of the clock, the
		// miss gives no reason of one.
		noWhy bool
	}{
		{name: "a read in the call's span", proven: true, sql: strings.Replace(clockSQL, "%s", later.read().Format(literalForm), 1), served: true},
		{name: "the recorded value, no read of this run", proven: true, sql: strings.Replace(clockSQL, "%s", recAt.Add(-1300*time.Microsecond).Format(literalForm), 1), mention: "body.query.literals.1"},
		{name: "a read an hour before the span", proven: true, sql: strings.Replace(clockSQL, "%s", later.read().Add(-time.Hour).Format(literalForm), 1), mention: "body.query.literals.1"},
		{name: "a read in the span, no span known", proven: true, noSpan: true, sql: strings.Replace(clockSQL, "%s", later.read().Format(literalForm), 1), mention: "body.query.literals.1"},
		{name: "a read in the span, another literal changed", proven: true, sql: strings.Replace(strings.Replace(clockSQL, "%s", later.read().Format(literalForm), 1), "a@x.io", "b@x.io", 1), mention: "body.query.literals.0", noWhy: true},
		{name: "a read in the span, the switch off", proven: false, sql: strings.Replace(clockSQL, "%s", later.read().Format(literalForm), 1)},
	}
	for _, tc := range cases {
		for _, fullScan := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "", true: ", the whole pool's scan"}[fullScan], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "0", true: "1"}[tc.proven])
				m, _ := recordedClock(t, recAt, f, f.read().Format(literalForm))
				m.Spec.Clocks = map[string]models.ClockOffset{"body.query.literals.1": o}
				mm := stageOwn(t, []*models.Mock{m}, []*models.Mock{m}, true)
				ctx := later.ctx(false)
				if tc.noSpan {
					ctx, _ = poolConn("c1")
					ctx = replayer.WithCommandForTest(ctx, later.arrived, false)
				}
				_, dctx := poolConn("c1")
				_, ok, miss, err := replayer.MatchCommandForTest(ctx, zap.NewNop(), queryReq(tc.sql), mm, dctx, nil, fullScan)
				if err != nil {
					t.Fatal(err)
				}
				if ok != tc.served {
					t.Fatalf("served %v (miss %s); want %v", ok, miss, tc.served)
				}
				if tc.mention != "" && !strings.Contains(miss, tc.mention) {
					t.Fatalf("the miss (%s) does not name %s", miss, tc.mention)
				}
				if tc.mention != "" && (!strings.Contains(miss, "closestMock:mock-note") || !strings.Contains(miss, "matchPhase:strict_noise_reject")) {
					t.Fatalf("the miss (%s) does not name mock-note the closest, rejected by strict comparison", miss)
				}
				if tc.noWhy && !strings.Contains(miss, " why: nextSteps:") {
					t.Fatalf("the miss (%s) gives a reason; want none", miss)
				}
			})
		}
	}
}

// A miss that turned recordings away at literals learned as reads of the
// application's clock gives the reason of the recording it names the
// closest: of two recordings of the statement, each refused for its own
// reason, the reason of the one it names. Where the named one was refused
// for another reason, the reason of the one refused at a literal is given
// with that recording's name, never as the named one's. The field diffs
// the miss shows are the named recording's too: none where it was turned
// away only at its clock literal, though the scan turned away another
// before it at another literal.
func TestAMissGivesTheClockReasonOfTheRecordingItNames(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	recAt := diffBase.Add(time.Millisecond)
	f := newClockRun(time.Now().UTC().Add(-time.Minute))
	_, o := recordedClock(t, recAt, f, f.read().Format(literalForm))
	later := newClockRun(time.Now().UTC())
	// A read an hour before the call's span: outside it.
	sql := strings.Replace(clockSQL, "%s", later.read().Add(-time.Hour).Format(literalForm), 1)
	apart := o
	apart.Zone = models.ZoneApart
	const outside, noOne = "body.query.literals.1 was learned as", "clock reads in no one zone"
	cases := []struct {
		name string
		// second is the recording the scan meets after mock-note, which it
		// names the closest; its clocks, and the owner its statement names.
		clocks map[string]models.ClockOffset
		owner  string
		// firstOwner is the owner mock-note's statement names (empty: the
		// live one's, a@x.io).
		firstOwner string
		// want is in the miss's reason, never is not; diffs starts the
		// field diffs the miss shows.
		want, never, diffs string
	}{
		{name: "both refused at the clock literal", clocks: map[string]models.ClockOffset{"body.query.literals.1": apart}, owner: "a@x.io", want: "why:body.query.literals.1 was learned at more than one call as " + noOne, never: "mock-note: ", diffs: "fieldDiffs:[] "},
		{name: "the named one refused at another literal", owner: "b@x.io", want: "why:mock-note: " + outside, diffs: "fieldDiffs:[{Path:body.query.literals.0 Kind:value_changed Expected:b@x.io Actual:a@x.io} "},
		{name: "the named one refused at the clock literal alone, the first at another", clocks: map[string]models.ClockOffset{"body.query.literals.1": apart}, owner: "a@x.io", firstOwner: "b@x.io", want: "why:body.query.literals.1 was learned at more than one call as " + noOne, never: "mock-note: ", diffs: "fieldDiffs:[] "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, _ := recordedClock(t, recAt, f, f.read().Format(literalForm))
			first.Spec.Clocks = map[string]models.ClockOffset{"body.query.literals.1": o}
			if tc.firstOwner != "" {
				q := first.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
				q.Query = strings.Replace(q.Query, "a@x.io", tc.firstOwner, 1)
			}
			secondAt := recAt.Add(5 * time.Millisecond)
			secondSQL := strings.Replace(strings.Replace(clockSQL, "%s", secondAt.Add(-1300*time.Microsecond).Format(literalForm), 1), "a@x.io", tc.owner, 1)
			second := diffMock("mock-note-2", "c1", []mysql.Request{queryReq(secondSQL)}, secondAt, models.LifetimePerTest, "mocks")
			second.Spec.Clocks = tc.clocks
			mm := stageOwn(t, []*models.Mock{first, second}, []*models.Mock{first, second}, true)
			_, dctx := poolConn("c1")
			_, ok, miss, err := replayer.MatchForTest(later.ctx(false), zap.NewNop(), queryReq(sql), mm, dctx)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatal("served; want a miss")
			}
			if !strings.Contains(miss, "closestMock:mock-note-2") || !strings.Contains(miss, "matchPhase:strict_noise_reject") {
				t.Fatalf("the miss (%s) does not name mock-note-2 the closest, rejected by strict comparison", miss)
			}
			if !strings.Contains(miss, tc.want) || (tc.never != "" && strings.Contains(miss, tc.never)) {
				t.Fatalf("the miss (%s) does not say %q, or says %q", miss, tc.want, tc.never)
			}
			if !strings.Contains(miss, tc.diffs) {
				t.Fatalf("the miss (%s) does not show the named recording's field diffs %q", miss, tc.diffs)
			}
		})
	}
}

// learnerCalls is a LiteralLearner that learns at each drifted literal what
// learns gives (none: nothing, with why), counting its calls.
type learnerCalls struct {
	n      int
	learns map[string]models.ClockOffset
	// echo learns, at every path it is asked about, the read learns holds
	// at body.query.literals.1. noWhy gives no reason where it learns none.
	echo, noWhy bool
	call        replayer.LiteralCall
}

func (l *learnerCalls) learner(_ context.Context, c replayer.LiteralCall) (map[string]models.ClockOffset, string) {
	l.n++
	l.call = c
	if l.echo {
		out := make(map[string]models.ClockOffset, len(c.Drift))
		for _, p := range c.Drift {
			out[p] = l.learns["body.query.literals.1"]
		}
		return out, ""
	}
	if l.learns == nil && l.noWhy {
		return nil, ""
	}
	if l.learns == nil {
		return nil, "body.query.literals.1 changed, and keploy cannot tell where its value came from"
	}
	return l.learns, ""
}

// Record's full round asks the registered LiteralLearner about a COM_QUERY
// whose recording differs from it only at inline literals, and serves the
// recording only where it learns a read of the application's clock at
// every literal that changed: the consumed copy then carries them as its
// MockSpec.Clocks, never as noise. A learner that learns none, or not all,
// leaves the call refused with its reason, or, where it gives none, with
// the literal it learned none at. A call that is not one of the
// full round, a replay without the proven switch, and a replay with no
// learner registered ask nothing, and are refused as before; so does a call
// after the set's learn step ended, and one whose recording already holds
// the literal as a read of the clock (it is checked as one, and a read
// outside the call's span is refused, whatever the learner would say).
// Detection never learns a literal learned as a read of the clock as noise,
// and the learner is asked once a recording.
func TestRecordsFullRoundLearnsAChangedLiteralOnlyAsAClockRead(t *testing.T) {
	recAt := diffBase.Add(time.Millisecond)
	f := newClockRun(time.Now().UTC())
	_, o := recordedClock(t, recAt, f, f.read().Format(literalForm))
	twoChanged := strings.Replace(strings.Replace(clockSQL, "%s", f.read().Format(literalForm), 1), "a@x.io", "b@x.io", 1)
	cases := []struct {
		name     string
		proven   bool
		fullRnd  bool
		register bool
		learns   map[string]models.ClockOffset
		sql      string
		asked    bool
		served   bool
		mention  string
		// ended: the set's learn step has ended; held: the recording holds
		// the literal as a read of the clock already, and the live one was
		// read an hour before the span; echo: the learner learns a read at
		// every path it is asked about; detect: detection learns noise.
		// strict: strict enforcement compares the request body.
		ended, held, echo, detect, strict, noWhy bool
	}{
		{name: "learns the read", proven: true, fullRnd: true, register: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, asked: true, served: true},
		{name: "learns the read, detection on", proven: true, fullRnd: true, register: true, detect: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, asked: true, served: true},
		{name: "learns the read, strict enforcement on", proven: true, fullRnd: true, register: true, strict: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, asked: true, served: true},
		{name: "the learn step ended", proven: true, fullRnd: true, register: true, ended: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, mention: "body.query.literals.1"},
		{name: "held as a read, read outside the span", proven: true, fullRnd: true, register: true, held: true, echo: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, mention: "body.query.literals.1"},
		{name: "learns none", proven: true, fullRnd: true, register: true, asked: true, mention: "keploy cannot tell where its value came from"},
		{name: "learns none, gives no reason", proven: true, fullRnd: true, register: true, noWhy: true, asked: true, mention: "why:body.query.literals.1 changed, and the learn step learned no read of the application's clock there"},
		{name: "learns one of two changed", proven: true, fullRnd: true, register: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, sql: twoChanged, asked: true, mention: "body.query.literals.0"},
		{name: "not the full round", proven: true, register: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}, mention: "body.query.literals.1"},
		{name: "the switch off", fullRnd: true, register: true, learns: map[string]models.ClockOffset{"body.query.literals.1": o}},
		{name: "no learner", proven: true, fullRnd: true, mention: "body.query.literals.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "0", true: "1"}[tc.proven])
			l := &learnerCalls{learns: tc.learns, echo: tc.echo, noWhy: tc.noWhy}
			if tc.register {
				replayer.RegisterLiteralLearner(l.learner)
				t.Cleanup(func() { replayer.RegisterLiteralLearner(nil) })
			}
			m, _ := recordedClock(t, recAt, f, f.read().Format(literalForm))
			sql := tc.sql
			if sql == "" {
				sql = strings.Replace(clockSQL, "%s", f.read().Format(literalForm), 1)
			}
			if tc.held {
				m.Spec.Clocks = map[string]models.ClockOffset{"body.query.literals.1": o}
				sql = strings.Replace(clockSQL, "%s", f.read().Add(-time.Hour).Format(literalForm), 1)
			}
			mm := stageOwn(t, []*models.Mock{m}, []*models.Mock{m}, true)
			ctx := f.ctx(tc.fullRnd)
			if tc.ended {
				g := newClockRun(f.began)
				g.tr.SetLearnEnded(true)
				ctx = g.ctx(tc.fullRnd)
			}
			eng := replayer.NoiseEngineForTest(tc.detect, tc.strict)
			if !tc.detect && !tc.strict {
				eng = nil
			}
			_, dctx := poolConn("c1")
			_, ok, miss, err := replayer.MatchWithEngineForTest(ctx, zap.NewNop(), queryReq(sql), mm, dctx, eng)
			if err != nil {
				t.Fatal(err)
			}
			if (l.n > 0) != tc.asked || l.n > 1 {
				t.Fatalf("the learner was asked %d times; want asked %v, once", l.n, tc.asked)
			}
			if ok != tc.served {
				t.Fatalf("served %v (miss %s); want %v", ok, miss, tc.served)
			}
			if tc.mention != "" && !strings.Contains(miss, tc.mention) {
				t.Fatalf("the miss (%s) does not say %q", miss, tc.mention)
			}
			if tc.asked {
				want := []string{"body.query.literals.1"}
				if tc.sql == twoChanged {
					want = []string{"body.query.literals.0", "body.query.literals.1"}
				}
				if strings.Join(l.call.Drift, ",") != strings.Join(want, ",") || l.call.Recorded == nil || l.call.Recorded.Name != m.Name || !l.call.Arrived.Equal(f.arrived) {
					t.Fatalf("the learner was asked about %v at %v; want %v of mock-note at %v", l.call.Drift, l.call.Arrived, want, f.arrived)
				}
				if got := l.call.Live["body.query.literals.1"]; got.Value != f.read().Format(literalForm) || got.Type != learn.String {
					t.Fatalf("the live literal is %+v", got)
				}
			}
			if !tc.served {
				return
			}
			states := mm.GetConsumedMocks()
			if len(states) != 1 {
				t.Fatalf("%d consumed; want mock-note", len(states))
			}
			if _, ok := states[0].Clocks["body.query.literals.1"]; !ok && tc.proven {
				t.Fatalf("the consumed mock carries clocks %v; want body.query.literals.1", states[0].Clocks)
			}
			for p := range states[0].ReqBodyNoise {
				if strings.Contains(p, "literals") {
					t.Fatalf("the consumed mock learned %s as noise", p)
				}
			}
		})
	}
}

// A COM_QUERY whose recording differs from it at a query attribute the
// statement reads as well as at an inline literal is no call that differs
// only at literals: record's full round asks the learner nothing about it,
// whatever the learner would learn, and refuses it.
func TestRecordsFullRoundAsksNothingWhereAQueryAttributeChangedToo(t *testing.T) {
	const reads = " AND mysql_query_attribute_string('traceparent') IS NOT NULL"
	const recordedTP, liveTP = "00-a5ff4ff820b14ad28693c1b155015340-0000000000000001-01", "00-60de6a56e2b8444e8e403145868fca9f-0000000000000001-01"
	t.Setenv(provenswitch.Env, "1")
	recAt := diffBase.Add(time.Millisecond)
	f := newClockRun(time.Now().UTC())
	m, o := recordedClock(t, recAt, f, f.read().Format(literalForm))
	q := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
	q.Query += reads
	replayer.SetAttributeForTest(m, recordedTP)
	for _, tc := range []struct {
		name   string
		tp     string
		served bool
	}{
		{name: "the attribute changed too", tp: liveTP},
		{name: "the literal alone changed", tp: recordedTP, served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &learnerCalls{learns: map[string]models.ClockOffset{"body.query.literals.1": o}, echo: true}
			replayer.RegisterLiteralLearner(l.learner)
			t.Cleanup(func() { replayer.RegisterLiteralLearner(nil) })
			mm := stageOwn(t, []*models.Mock{m}, []*models.Mock{m}, true)
			sql := strings.Replace(clockSQL, "%s", f.read().Format(literalForm), 1) + reads
			_, dctx := poolConn("c1")
			_, ok, miss, err := replayer.MatchForTest(f.ctx(true), zap.NewNop(), replayer.AttrQueryReqForTest(t, tc.tp, sql), mm, dctx)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.served || (l.n == 1) != tc.served {
				t.Fatalf("served %v, the learner asked %d times (miss %s); want served and asked %v", ok, l.n, miss, tc.served)
			}
		})
	}
}

// A literal record's full round refuses is a request that changed since it
// was recorded: the learn step learned no read of the application's clock
// where it changed, or a literal learned as one holds no read of its call's
// span. The miss Replay returns then carries judging's reason,
// models.MissRequestChanged, and its next steps give the refusal's reason
// and name it, as the HTTP learn step's misses do, so judging lists the test
// as one whose request changed, never as one whose call had no recorded
// answer. A refusal outside the full round (keploy test), without the
// switch, after the set's learn step ended, or where the full round asks no
// learner, gives no reason: only record's full round, where the learn step
// runs, gives one.
func TestARefusedLiteralOfRecordsFullRoundIsARequestThatChanged(t *testing.T) {
	const learnerWhy = "body.query.literals.1 changed, and keploy cannot tell where its value came from"
	recAt := diffBase.Add(time.Millisecond)
	cases := []struct {
		name                      string
		proven, fullRnd, register bool
		held, ended               bool
		reason                    string
		says                      string
	}{
		{name: "the learner learns none", proven: true, fullRnd: true, register: true, reason: models.MissRequestChanged, says: learnerWhy},
		{name: "held as a read, read outside the span", proven: true, fullRnd: true, register: true, held: true, reason: models.MissRequestChanged, says: "body.query.literals.1"},
		{name: "held as a read, read outside the span, no learner", proven: true, fullRnd: true, held: true, reason: models.MissRequestChanged, says: "body.query.literals.1"},
		{name: "held as a read, read outside the span, keploy test", proven: true, held: true},
		{name: "not the full round", proven: true, register: true},
		{name: "the learn step ended", proven: true, fullRnd: true, register: true, ended: true},
		{name: "the switch off", fullRnd: true, register: true},
		{name: "no learner", proven: true, fullRnd: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "0", true: "1"}[tc.proven])
			l := &learnerCalls{}
			if tc.register {
				replayer.RegisterLiteralLearner(l.learner)
				t.Cleanup(func() { replayer.RegisterLiteralLearner(nil) })
			}
			// The run's test began a second ago: Replay reads the command
			// as it arrives, after the run's previous call.
			f := newClockRun(time.Now().UTC().Add(-time.Second))
			f.tr.SetLearnEnded(tc.ended)
			m, o := recordedClock(t, recAt, f, f.read().Format(literalForm))
			sql := strings.Replace(clockSQL, "%s", f.read().Format(literalForm), 1)
			if tc.held {
				m.Spec.Clocks = map[string]models.ClockOffset{"body.query.literals.1": o}
				sql = strings.Replace(clockSQL, "%s", f.read().Add(-time.Hour).Format(literalForm), 1)
			}
			mm := stageOwn(t, []*models.Mock{replayer.NativeHandshakeForTest(), m}, []*models.Mock{m}, true)
			ctx, _ := poolConn("c1")
			ctx = learn.WithTracker(ctx, f.tr)
			_, done := replayer.ReplayCommandsInForTest(t, ctx, mm, models.OutgoingOptions{Learn: tc.fullRnd}, append([]byte{mysql.COM_QUERY}, sql...))
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the command was answered; want a miss that ends the connection")
			}
			var r interface {
				MismatchReport() *models.MockMismatchReport
			}
			if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
				t.Fatalf("Replay returned %v; want a miss carrying a mismatch report", err)
			}
			rep := r.MismatchReport()
			if rep.Reason != tc.reason {
				t.Fatalf("the miss's reason is %q (next steps %q); want %q", rep.Reason, rep.NextSteps, tc.reason)
			}
			if tc.reason == "" {
				if strings.Contains(rep.NextSteps, models.MissRequestChanged) {
					t.Fatalf("the miss gives no reason, and its next steps name one: %q", rep.NextSteps)
				}
				return
			}
			if !strings.Contains(rep.NextSteps, tc.says) || !strings.Contains(rep.NextSteps, "("+models.MissRequestChanged+")") {
				t.Fatalf("the miss's next steps (%q) do not give the refusal's reason %q and name %s", rep.NextSteps, tc.says, models.MissRequestChanged)
			}
		})
	}
}
