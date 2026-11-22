package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The rows below are built from run m1c-x1's recordings (api-server's e2e
// suite under keploy record, 2026-11-10 08:25-08:27 UTC): the recorded and
// round F values, the recorded times and the neighbour recordings of each
// refused field, cut from the run's mocks.yaml.

// rbDoc is mock-1570's answer, a find on clusters reading back the record
// the test inserted at mock-1567, created_at and updated_at at sec.
func rbDoc(sec string) string {
	return `{"cursor":{"firstBatch":[{"_id":"6af2d505c4cc8b2edc7b2d9a","created_at":{"$numberLong":"` + sec + `"},"updated_at":{"$numberLong":"` + sec + `"},"heartbeats":[]}]}}`
}

// rbAnswer is a recording of the call's test answered at at, holding r as
// recorded and f in round F's view of it.
func rbAnswer(name string, at time.Time, r, f string) []Raw {
	a := oAnswer(name, oAt.Sub(at), r)
	return []Raw{a.ServedAs(1).ServedLive(oAnswer(name, oAt.Sub(at), f))}
}

// rbSent is the insert that wrote the moment: its call learned it as its own
// read of the application's clock at res where clock is set.
func rbSent(at time.Time, path, rec, live string, clock bool, res time.Duration) Earlier {
	e := iSent("mock-1567", at, path, String, rec, live)
	if clock {
		e.Seen[0].Clock, e.Seen[0].Offset = true, &models.ClockOffset{Res: res}
	}
	return e
}

// OutdatedFree's insert on app_config_audits (mock-1588) sends app.createdAt
// = time.Now().Unix(), its own read of the clock. 8 ms before, the test's
// insert on clusters (mock-1567) wrote created_at of the same second, a read
// its call learned as the clock at the second; 6 ms before, the find on
// clusters (mock-1570) answered that record back. The answer holds the
// second only because the test wrote it there: two reads of one coarse cut
// are no copy, so the field is the call's read, checked in its window, never
// a value bound to the answer (round F refused it so, "keploy names no place
// of mock-1570 to bind it at"). What stays: the answer alone, a send learned
// as a plain value, a send after the answer and a send at the millisecond
// still bind; another test's answer is still refused; where neither the
// call's window nor the test's span holds the read, the answer still binds.
func TestOutcomeAnAnswerThatReadsBackACoarseClockSendIsNoSource(t *testing.T) {
	const (
		p     = "body.docs.msgs.0.app.createdAt.$numberLong"
		sentP = "body.docs.msgs.0.created_at.$numberLong"
	)
	rAt := time.Date(2026, 11, 10, 8, 25, 41, 712129888, time.UTC)
	fAt := rAt.Add(62 * time.Second)
	rec, live := "1794299141", "1794299203"
	w := iWin(rAt, fAt, 400*time.Microsecond)
	answer := rbAnswer("mock-1570", rAt.Add(-6*time.Millisecond), rbDoc(rec), rbDoc(live))
	sent := rbSent(rAt.Add(-8*time.Millisecond), sentP, rec, live, true, time.Second)
	one := func(what string, in Input) Decision {
		t.Helper()
		ds := Decide(in)
		if len(ds) != 1 {
			t.Fatalf("%s: %d decisions, want one: %s", what, len(ds), oDescribe(ds))
		}
		return ds[0]
	}

	d := one("the send, then its read-back", iCall(p, String, rec, live, rAt, fAt, w, []Earlier{sent}, answer))
	if d.Outcome != OutcomeClock || d.Clock == nil || !d.Clock.Plain() {
		t.Errorf("an answer that reads back the test's coarse clock send: %s; want a plain read of the clock in the call's window", oDescribe([]Decision{d}))
	}
	if d := one("the send alone", iCall(p, String, rec, live, rAt, fAt, w, []Earlier{sent}, nil)); d.Outcome != OutcomeClock {
		t.Errorf("the send alone: %s; want the clock, as with the answer", oDescribe([]Decision{d}))
	}

	bound := func(what string, d Decision, mock string) {
		t.Helper()
		if d.Outcome != OutcomeValue || d.Clock != nil || d.From.Mock != mock {
			t.Errorf("%s: %s; want a Value bound at %s", what, oDescribe([]Decision{d}), mock)
		}
	}
	// R7d: no write behind the answer, so nothing says it reads one back.
	bound("the answer alone", one("the answer alone", iCall(p, String, rec, live, rAt, fAt, w, nil, answer)), "mock-1570")
	plain := rbSent(rAt.Add(-8*time.Millisecond), sentP, rec, live, false, 0)
	bound("the send learned as a plain value", one("plain", iCall(p, String, rec, live, rAt, fAt, w, []Earlier{plain}, answer)), "mock-1570")
	after := rbSent(rAt.Add(-4*time.Millisecond), sentP, rec, live, true, time.Second)
	bound("a send after the answer", one("after", iCall(p, String, rec, live, rAt, fAt, w, []Earlier{after}, answer)), "mock-1570")

	// Neither the call's window nor a span since its test's begin holds the
	// read: the value is a copy, so the answer binds it.
	far := iWin(rAt.Add(-3*time.Second), fAt.Add(-3*time.Second), 400*time.Microsecond)
	bound("a window that does not hold the read", one("far", iCall(p, String, rec, live, rAt, fAt, far, []Earlier{sent}, answer)), "mock-1570")

	// At the millisecond a send's moment is a copy's source (step 4), never a
	// second read of one coarse cut.
	msR, msF := strconv.FormatInt(rAt.Add(-8*time.Millisecond).UnixMilli(), 10), strconv.FormatInt(fAt.Add(-8*time.Millisecond).UnixMilli(), 10)
	msSent := rbSent(rAt.Add(-8*time.Millisecond), sentP, msR, msF, true, time.Millisecond)
	msAns := rbAnswer("mock-1570", rAt.Add(-6*time.Millisecond), msR, msF)
	msW := iWin(rAt, fAt, 10*time.Millisecond)
	if d := one("ms", iCall(p, String, msR, msF, rAt, fAt, msW, []Earlier{msSent}, msAns)); d.Outcome == OutcomeClock || d.Clock != nil {
		t.Errorf("FALSE GREEN? a moment a millisecond send wrote, read back: %s; want no clock", oDescribe([]Decision{d}))
	}

	// Another test's answer, where the call's window does not hold the read
	// in round F: refused naming it, the send beside it or not.
	next := time.Unix(1794299204, 40e6).UTC()
	nw := &Window{Rec: Span{From: rAt.Add(-400 * time.Microsecond), To: rAt}, F: Span{From: next.Add(-30 * time.Millisecond), To: next}}
	in := iCall(p, String, rec, live, rAt, next, nw, []Earlier{sent}, answer)
	in.Scope = func(m string) Scope {
		if m == "mock-1570" {
			return ScopeTest("test-1")
		}
		return ScopeTest("test-2")
	}
	oRefused(t, "another test's answer beside the send", one("other", in), RefusalAnotherScope, "mock-1570")
}

// CIJob's session check (mock-3265): aggregate refreshToken {expiration:
// {$gt: now}} at the second, 7.7 ms after the test's insert on users
// (mock-3250) wrote createdAt, learned there as the clock at the second, and
// 2.3 ms after the find on users (mock-3261) answered it back, the record the
// insert wrote. The first three aggregates of the test (mock-3248, 3252,
// 3259), before the find, learned the clock; the fourth was bound to the
// answer and refused, and round F's second pass found the three learned ones
// spent ("made once more than the 3 times"). With the read-back no source,
// all four are the clock; the answer alone (R7d) still binds.
func TestOutcomeAnExpiryFilterAfterAReadBackOfTheTestsWriteIsTheClock(t *testing.T) {
	const (
		p       = "body.cmd.pipeline.0.$match.expiration.$gt.$numberLong"
		insertP = "body.docs.msgs.0.createdAt.$numberLong"
		answerP = "resp.0.0.cursor.firstBatch.0.createdAt.$numberLong"
		record  = "users 6af2d52bc4cc8b2edc7b2db1"
	)
	rAt := time.Unix(1794299179, 655991573).UTC()
	fAt := time.Unix(1794299255, 400000000).UTC()
	itoa := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	insert := func(rec, live string) Earlier {
		m := &models.Mock{Name: "mock-3250", Spec: models.MockSpec{ReqTimestampMock: rAt.Add(-7700 * time.Microsecond), ResTimestampMock: rAt.Add(-7400 * time.Microsecond)}}
		return Earlier{Mock: "mock-3250", At: rAt.Add(-7700 * time.Microsecond), Own: true, Payload: RawOf(m, true).ServedAs(1),
			Seen: []Seen{{Side: Request, Leaf: Leaf{Path: insertP, Noise: insertP, Type: String, Value: rec}, Live: live, Place: insertP,
				Written: true, Record: record, Clock: true, Offset: &models.ClockOffset{Res: time.Second}}}}
	}
	find := func(rec, live string) Earlier {
		m := &models.Mock{Name: "mock-3261", Spec: models.MockSpec{ReqTimestampMock: rAt.Add(-2300 * time.Microsecond), ResTimestampMock: rAt.Add(-2000 * time.Microsecond)}}
		return Earlier{Mock: "mock-3261", At: rAt.Add(-2300 * time.Microsecond), Own: true, Payload: RawOf(m, true).ServedAs(2),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: answerP, Noise: answerP, Type: String, Value: rec}, Live: live, Place: answerP, Record: record}}}
	}
	decide := func(earlier ...Earlier) Decision {
		t.Helper()
		leaf := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		ds := Decide(Input{
			Drifts: oDrifts(), Drift: []string{p}, Live: leaf(itoa(fAt)), Rec: leaf(itoa(rAt)),
			At: rAt, Arrived: fAt, Test: "test-2", Mock: "mock-3265", Mode: ModeOwn, Earlier: earlier,
			Window: v5Window(v5Call(rAt, 20*time.Millisecond, 0), v5Call(fAt, 20*time.Millisecond, 0)), Landed: Landed{Where: LandedOwn},
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
		})
		if len(ds) != 1 {
			t.Fatalf("%d decisions, want one: %s", len(ds), oDescribe(ds))
		}
		return ds[0]
	}
	same, fSame := itoa(rAt), itoa(fAt)
	if d := decide(insert(same, fSame)); d.Outcome != OutcomeClock {
		t.Errorf("the insert alone (mock-3248/3252/3259): %s; want the clock", oDescribe([]Decision{d}))
	}
	if d := decide(insert(same, fSame), find(same, fSame)); d.Outcome != OutcomeClock || d.Clock == nil {
		t.Errorf("the insert and its read-back answer (mock-3265): %s; want the clock, as the three before it", oDescribe([]Decision{d}))
	}
	if d := decide(find(same, fSame)); d.Outcome != OutcomeValue || d.From.Mock != "mock-3261" {
		t.Errorf("R7d the read-back answer with no write behind it: %s; want a Value bound at mock-3261", oDescribe([]Decision{d}))
	}
}

// SlowStep's insert on testSuiteRun (mock-1784, 08:26:13.954) sends
// started_at, the job's start: atg/core.go reads it about 2.1 s before the
// insert, and the job's insert on ATGJobs (mock-1749, 08:26:11.838) wrote it
// as created_at, a read its call learned as the clock at the second; the
// find on ATGJobs (mock-1765, 08:26:11.868) answered it back. With the
// read-back no source, the field is read in the call's window as the clock
// less about 2 s, a range fitted to how long the job ran in these two runs.
//
// The known risk, pinned: a replay whose job runs the 2.1 s it ran here
// passes, and one whose job runs much longer or shorter goes red. A false
// red, never a false green: a started_at the job's span does not explain is
// refused, and the field is never bound to the answer.
func TestOutcomeSlowStepsStartedAtIsALookBackFittedToItsJob(t *testing.T) {
	const p = "body.docs.msgs.0.started_at.$numberLong"
	rAt := time.Date(2026, 11, 10, 8, 26, 13, 954256788, time.UTC)
	gap := 62 * time.Second // the run's: "1794299171" -> "1794299233"
	fAt := rAt.Add(gap)
	rec, live := "1794299171", "1794299233"
	insertAt := time.Date(2026, 11, 10, 8, 26, 11, 838609524, time.UTC)
	findAt := time.Date(2026, 11, 10, 8, 26, 11, 868910102, time.UTC)
	sent := iSent("mock-1749", insertAt, "body.docs.msgs.0.created_at.$numberLong", String, rec, live)
	sent.Seen[0].Clock, sent.Seen[0].Offset = true, &models.ClockOffset{Res: time.Second}
	doc := func(sec string) string {
		return `{"cursor":{"firstBatch":[{"_id":"5a9bd0a4-5bb4-44c4-b5a3-5223ddcaf806","created_at":{"$numberLong":"` + sec + `"}}]}}`
	}
	answer := rbAnswer("mock-1765", findAt, doc(rec), doc(live))
	// The harness's window: 200 ms of the application's clock ending at the
	// call, the test begun 3 s before the job, in a protocol whose replay
	// compares a read's offset (Mongo's).
	w := iWin(rAt, fAt, 200*time.Millisecond).ComparesOffsets()
	w.Rec.Began, w.F.Began = insertAt.Add(-3*time.Second), insertAt.Add(gap-3*time.Second)
	in := iCall(p, String, rec, live, rAt, fAt, w, []Earlier{sent}, answer)
	in.Drifts = oClientDrifts()
	ds := Decide(in)
	if len(ds) != 1 {
		t.Fatalf("%d decisions, want one: %s", len(ds), oDescribe(ds))
	}
	d := ds[0]
	if d.Outcome == OutcomeValue || d.From.Mock == "mock-1765" {
		t.Fatalf("started_at: %s; want no Value bound to the read-back answer", oDescribe(ds))
	}
	if d.Outcome != OutcomeClock || d.Clock == nil || d.Clock.Plain() {
		t.Fatalf("started_at: %s; want a read of the clock less an offset (the known risk this row pins)", oDescribe(ds))
	}
	t.Logf("started_at learned as %s", d.Clock.String())
	// A replay: the job starts at start, the insert follows by ran, in a
	// window of 200 ms ending at the insert.
	check := func(ran time.Duration) bool {
		call := time.Date(2026, 11, 12, 10, 0, 0, 954256788, time.UTC)
		ok, _ := v5Check(p, *d.Clock, rec, call.Add(-ran), Span{From: call.Add(-200 * time.Millisecond), To: call})
		return ok
	}
	if !check(2115 * time.Millisecond) {
		t.Errorf("a replay whose job runs the 2.1 s it ran when recorded: refused; want it to pass")
	}
	for _, ran := range []time.Duration{4 * time.Second, 500 * time.Millisecond} {
		if check(ran) {
			t.Errorf("FALSE GREEN? a replay whose job ran %v, which no read of the clock less about 2 s explains: passed; want it refused (a false red is the risk, never a false green)", ran)
		}
	}
}
