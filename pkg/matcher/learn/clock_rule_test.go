package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows of keploy.yml's kind: clock (Input.Source): a field the application
// reads from its clock for the call. api-server's app_config_audits insert
// stores the app's creation time, and an earlier test's answer, outside the
// call's scope, holds the same moment at a fine cut by chance: learning
// refuses it as another-scope, so the call was set aside.
// The rule decides the read of the clock where the replay's own check holds
// it in both runs' spans, and refuses it no-clock-read otherwise, naming the
// rule; learning's own facts come first.

const (
	cRule = "app-audit-created-at"
	cPath = "body.docs.msgs.0.app.createdAt.$numberLong"
)

// cClock declares path a read of the clock within o by cRule.
func cClock(path string, o models.ClockOffset) func(string) (Source, bool) {
	return func(f string) (Source, bool) { return Source{Kind: SourceClock, Rule: cRule, Clock: o}, f == path }
}

// cMicro is the declared range of a read at the microsecond: from a
// millisecond before the call's span to its end.
var cMicro = models.ClockOffset{Min: -time.Millisecond, Max: 0, Res: time.Microsecond}

// cRAt and cFAt are when the call reached keploy in each run, cR and cF its
// values (microseconds), 50 ms before.
var (
	cRAt, cFAt = oAt.Add(300 * time.Millisecond), oAt.Add(time.Hour + 300*time.Millisecond)
	cR, cF     = iMicro(cRAt.Add(-50 * time.Millisecond)), iMicro(cFAt.Add(-50 * time.Millisecond))
)

// cAns is an answer recorded 100 ms before the call holding r, which round
// F served changed, holding f in its view.
func cAns(r, f string) Raw {
	ans := func(v string) Raw { return oAnswer("mock-7", 100*time.Millisecond, v) }
	return ans(r).ServedAs(1).ServedChanged(ans(r), ans(f))
}

// cCall is the insert at cPath, recorded at cRAt and seen in round F at
// cFAt, in a window of 60 ms, its value rec and live, and raw the
// recordings before it; outside puts every one of them in another test.
func cCall(rec, live string, raw []Raw, outside bool) Input {
	in := iCall(cPath, String, rec, live, cRAt, cFAt, iWin(cRAt, cFAt, 60*time.Millisecond), nil, raw)
	if outside {
		in.Scope = func(string) Scope { return ScopeTest("test-1") }
	}
	return in
}

// cNamesRule checks every refusal of ds names cRule in its line's tail.
func cNamesRule(t *testing.T, what string, ds []Decision) {
	t.Helper()
	for _, d := range ds {
		if d.Refusal == nil {
			continue
		}
		if r, ok := ParseRefusal(d.Line); d.Refusal.Rule != cRule || !ok || r.Rule != cRule {
			t.Errorf("%s: refusal %q; want its tail to name the rule %s", what, d.Line, cRule)
		}
	}
}

// cRefused checks ds is one refusal of kind naming cRule.
func cRefused(t *testing.T, what string, ds []Decision, kind RefusalKind, names ...string) {
	t.Helper()
	if len(ds) != 1 {
		t.Errorf("FALSE GREEN? %s: %s; want one refusal", what, oDescribe(ds))
		return
	}
	oRefused(t, what, ds[0], kind, names...)
	cNamesRule(t, what, ds)
}

// cRead checks ds is one read of the clock checked as off, its line naming
// cRule.
func cRead(t *testing.T, what string, ds []Decision, off models.ClockOffset) {
	t.Helper()
	if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil {
		t.Errorf("%s: %s; want one read of the clock", what, oDescribe(ds))
		return
	}
	if c := *ds[0].Clock; c.Min != off.Min || c.Max != off.Max || c.Res != off.Res {
		t.Errorf("%s: checked as %+v; want %+v", what, c, off)
	}
	if !strings.Contains(ds[0].Line, cRule) || !strings.Contains(ds[0].Line, "declares it a read of the clock") {
		t.Errorf("%s: the line must name the rule: %q", what, ds[0].Line)
	}
}

func TestAClockRuleDecidesAReadWhereAnAnswerOutsideTheScopeHoldsTheMoment(t *testing.T) {
	outside := cAns(cR, cF)
	in := cCall(cR, cF, []Raw{outside}, true)
	before := Decide(in)
	if len(before) != 1 || before[0].Refusal == nil || before[0].Refusal.Kind != RefusalAnotherScope {
		t.Fatalf("with no rule: %s; want refused as another-scope", oDescribe(before))
	}
	in.Source = cClock(cPath, cMicro)
	cRead(t, "an answer outside the scope holds the moment", Decide(in), models.ClockOffset{Res: time.Microsecond})

	// An answer of the scope round F served with no view stated (unknown),
	// and one that gave round F's moment as recorded (a pick): the rule
	// settles which source the moment has.
	for _, c := range []struct {
		what string
		x    Raw
		kind RefusalKind
	}{
		{"an answer served with no view stated", oAnswer("mock-7", 100*time.Millisecond, cF).ServedAs(1), RefusalUnknown},
		{"an answer that gave round F's moment as recorded", oAnswer("mock-7", 100*time.Millisecond, cF).ServedAs(1).ServedAsRecorded(), RefusalPick},
	} {
		x := cCall(cR, cF, []Raw{c.x}, false)
		if ds := Decide(x); len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != c.kind {
			t.Fatalf("%s, with no rule: %s; want refused as %s", c.what, oDescribe(ds), c.kind)
		}
		x.Source = cClock(cPath, cMicro)
		cRead(t, c.what, Decide(x), models.ClockOffset{Res: time.Microsecond})
	}

	// A minted rule on the field: minted's checks, never the clock's.
	tf, _ := models.MintedFormOf(cR, cF)
	in.Source = func(f string) (Source, bool) { return Source{Kind: SourceMinted, Rule: cRule, Minted: tf}, f == cPath }
	if ds := Decide(in); len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalNotMinted {
		t.Errorf("a minted rule on the field: %s; want refused not-minted, naming the answer that holds it", oDescribe(ds))
	}

	// No rule of a kind keploy knows, or none for this field: as before.
	for name, src := range map[string]func(string) (Source, bool){
		"a rule of another field": cClock("body.docs.msgs.0.auditedAt", cMicro),
		"a kind keploy does not know": func(string) (Source, bool) {
			return Source{Kind: SourceClock + 1, Rule: cRule, Clock: cMicro}, true
		},
	} {
		in.Source = src
		if got := Decide(in); oDescribe(got) != oDescribe(before) {
			t.Errorf("%s: %s; want as with no rule: %s", name, oDescribe(got), oDescribe(before))
		}
	}

	// A range on one side of zero is a read plus that offset: a look-back
	// three to one seconds back holds no value read at the call.
	in.Source = cClock(cPath, models.ClockOffset{Min: -3 * time.Second, Max: -time.Second, Res: time.Microsecond})
	cRefused(t, "a look-back the value is not", Decide(in), RefusalNoClockRead)

	// A range keploy does not check, and a call no span is known of.
	for name, o := range map[string]models.ClockOffset{
		"a resolution of none": {Min: -time.Millisecond},
		"a max below its min":  {Min: time.Millisecond, Max: -time.Millisecond, Res: time.Microsecond},
	} {
		in.Source = cClock(cPath, o)
		ds := Decide(in)
		cRefused(t, name, ds, RefusalNoClockRead)
		if len(ds) != 1 || !strings.Contains(ds[0].Line, "a range keploy does not check") {
			t.Errorf("%s: %s; want the line to say keploy does not check the range", name, oDescribe(ds))
		}
	}

	// A range all above zero is a read plus that offset (an expiry an hour
	// on), which a value read at the call is not.
	in.Source = cClock(cPath, models.ClockOffset{Min: time.Hour, Max: 2 * time.Hour, Res: time.Microsecond})
	cRefused(t, "an expiry the value is not", Decide(in), RefusalNoClockRead)
	nowin := cCall(cR, cF, []Raw{outside}, true)
	nowin.Window, nowin.Source = nil, cClock(cPath, cMicro)
	cRefused(t, "no span of the call", Decide(nowin), RefusalNoClockRead)
	if ds := Decide(nowin); len(ds) != 1 || !strings.Contains(ds[0].Line, "knows no span of the call") {
		t.Errorf("no span of the call: %s; want the line to say so", oDescribe(ds))
	}

	// A 13-digit number past 2100 reads as no moment: matched by its text.
	const r, f = "9999999999473", "9999999999901"
	num := cCall(r, f, []Raw{cAns(r, f)}, true)
	num.Rec.Leaves[cPath], num.Live.Leaves[cPath] = Leaf{Path: cPath, Noise: cPath, Type: Number, Value: r}, Leaf{Path: cPath, Noise: cPath, Type: Number, Value: f}
	num.Source = cClock(cPath, cMicro)
	ds := Decide(num)
	cRefused(t, "a number keploy reads no moment in", ds, RefusalNoClockRead)
	if len(ds) != 1 || !strings.Contains(ds[0].Line, "reads no moment in its value") {
		t.Errorf("a number keploy reads no moment in: %s; want the line to say so", oDescribe(ds))
	}
}

// The rule never widens what is accepted: a value the replay's check would
// refuse in either run (a cached time, one read a second before the call's
// span) is refused no-clock-read, naming the rule, though learning could
// not tell its source.
func TestAClockRuleRefusesAValueNoReadOfTheClockInARun(t *testing.T) {
	for _, c := range []struct{ what, rec, live string }{
		{"round F's value a cached time, a minute old", cR, iMicro(cFAt.Add(-time.Minute))},
		{"round F's value a second before its span", cR, iMicro(cFAt.Add(-time.Second))},
		{"the recorded value a second before its span", iMicro(cRAt.Add(-time.Second)), cF},
	} {
		in := cCall(c.rec, c.live, []Raw{cAns(c.rec, c.live)}, true)
		ds := Decide(in)
		if len(ds) != 1 || ds[0].Outcome == OutcomeClock {
			t.Fatalf("%s, with no rule: %s; want one word, no read of the clock", c.what, oDescribe(ds))
		}
		in.Source = cClock(cPath, cMicro)
		cRefused(t, c.what, Decide(in), RefusalNoClockRead)
	}
}

// Learned facts come first: an answer of the call's scope that holds the
// moment binds it (a Value), a conflict stays, and a value keploy reads no
// instant in is refused no-clock-read; each refusal names the rule.
func TestAClockRuleLeavesWhatLearningPlaced(t *testing.T) {
	in := cCall(cR, cF, []Raw{cAns(cR, cF)}, false)
	in.Source = cClock(cPath, cMicro)
	iValue(t, "an answer of the scope holds the moment", oWord(t, Decide(in), cR, cF), "mock-7")

	// Learning's own read of the clock keeps its check, at its resolution.
	own := cCall(cR, cF, nil, false)
	before := Decide(own)
	own.Source = cClock(cPath, models.ClockOffset{Min: -time.Millisecond, Max: 0, Res: time.Nanosecond})
	if got := Decide(own); len(got) != 1 || len(before) != 1 || got[0].Outcome != OutcomeClock || got[0].Line != before[0].Line || *got[0].Clock != *before[0].Clock {
		t.Errorf("learning's read of the clock: %s; want as with no rule: %s", oDescribe(got), oDescribe(before))
	}

	// The call's own recorded answer holds the moment: a replay would serve
	// that answer's recorded moment, so the read's own refusal stays
	// (cannot-check), though learning first refused it as another-scope.
	echo := cCall(cR, cF, []Raw{cAns(cR, cF)}, true)
	echo.Resp = []Leaf{{Path: "body.app.createdAt", Noise: "body.app.createdAt", Type: String, Value: cR}}
	if ds := Decide(echo); len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalAnotherScope {
		t.Fatalf("an answer that echoes it, with no rule: %s; want refused as another-scope", oDescribe(ds))
	}
	echo.Source = cClock(cPath, cMicro)
	cRefused(t, "the call's own answer echoes it", Decide(echo), RefusalCannotCheck)

	// A start-up recording served as recorded sent the recorded moment in
	// round F too, which the call did not carry: a conflict.
	const p = "body.at"
	rAt := oAt.Add(2*time.Hour + 400*time.Millisecond + 123*time.Microsecond)
	fAt := rAt.Add(time.Hour + 37*time.Second + 211*time.Millisecond + 7*time.Microsecond)
	r, f := iMicro(rAt.Add(-time.Second)), iMicro(fAt.Add(-time.Second))
	m := sentAtMock("mock-5", r, "", rAt.Add(-time.Second), rAt.Add(-900*time.Millisecond))
	conflict := iCall(p, String, r, f, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, []Raw{RawOf(m, true).ServedAs(1).ServedAsRecorded()})
	conflict.Source = cClock(p, models.ClockOffset{Min: -2 * time.Second, Max: 0, Res: time.Microsecond})
	cRefused(t, "a conflict", Decide(conflict), RefusalConflict, "mock-5")

	// No instant in either run: learning makes each word anew.
	words := oInput("body.note", "ab12cd34", "ef56ab78", nil, nil)
	words.Source = cClock("body.note", cMicro)
	ds := Decide(words)
	if len(ds) == 0 {
		t.Fatalf("no instant: no decision")
	}
	for _, d := range ds {
		oRefused(t, "no instant", d, RefusalNoClockRead)
	}
	cNamesRule(t, "no instant", ds)
}

// A minted rule acts on the field's words only through minted: where one
// word of the field is copied from an exchange of the call's own test,
// minted leaves every word as learning decided it, and an instant word
// learning refused as another-scope keeps that refusal, never decided by a
// clock's checks.
func TestAMintedRuleLeavesAnInstantWordToLearning(t *testing.T) {
	const wr, wf = "order-17", "order-18"
	in := cCall(cR+" "+wr, cF+" "+wf, []Raw{cAns(cR, cF)}, false)
	in.Earlier = oServed("mock-231", false, false, Response, oText("body.value.seq", "17"), "18", time.Millisecond)
	in.Scope = func(m string) Scope {
		if m == "mock-7" {
			return ScopeTest("test-1")
		}
		return ScopeTest(in.Test)
	}
	before := Decide(in)
	if len(before) != 2 || before[0].Refusal == nil || before[0].Refusal.Kind != RefusalAnotherScope || before[1].Outcome != OutcomeValue {
		t.Fatalf("with no rule: %s; want the instant refused as another-scope and the word a Value", oDescribe(before))
	}
	in.Source = func(p string) (Source, bool) { return Source{Kind: SourceMinted, Rule: cRule}, p == cPath }
	ds := Decide(in)
	if oDescribe(ds) != oDescribe(before) {
		t.Fatalf("a minted rule on the field: %s; want as learning decided it: %s", oDescribe(ds), oDescribe(before))
	}
	oRefused(t, "the instant word", ds[0], RefusalAnotherScope, "mock-7")
	iValue(t, "the copied word", ds[1], "mock-231")
}
