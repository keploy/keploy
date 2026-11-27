package learn

import (
	"strconv"
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

// A minted rule decides the field whole, by minted's checks alone: an
// instant word of it learning refused as another-scope, and a word copied
// from an exchange of the call's own test, are pieces of its value, never
// decided by a clock's checks or bound one by one. This value holds a
// space, so it is refused as of another shape, naming the rule.
func TestAMintedRuleDecidesAFieldWithAnInstantWordWhole(t *testing.T) {
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
	form := models.MintedForm{MinLen: len(cR + " " + wr), MaxLen: len(cR + " " + wr)}
	in.Source = func(p string) (Source, bool) {
		return Source{Kind: SourceMinted, Rule: cRule, Minted: form}, p == cPath
	}
	ds := Decide(in)
	if len(ds) != 1 || ds[0].At.Word != -1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalChangedShape || ds[0].Refusal.Rule != cRule {
		t.Fatalf("a minted rule on the field: %s; want the field refused whole as of another shape, naming the rule", oDescribe(ds))
	}
}

// TestADeclaredClockRuleOverridesACoincidentalClientRequestWord reproduces
// api-server #2677 (TestAnAutoReplayRoundIsReportedByItsClusterOnly):
// deleteApp stamps auditedAt = time.Now().Unix() before its preceding reads,
// so the insert's own call window can start in the next second (within
// app-audit-audited-at's min: -1s, max: 0s, res: 1s), while an earlier client
// test request (test-4 POST /k8s-proxy/autoreplay-rounds) carries the same
// 10-digit Unix second in replayId#7. With the clock rule declared and both
// runs holding CheckClockRead, the declared read goes before the client
// request so auditedAt is decided a Clock, never bound to replayId#7.
// Guardrails: an in-scope DB answer still binds, and a value outside the
// declared clock range still binds to the client request.
func TestADeclaredClockRuleOverridesACoincidentalClientRequestWord(t *testing.T) {
	const auditPath = "body.docs.msgs.0.auditedAt.$numberLong"
	const auditRule = "app-audit-audited-at"
	auditClock := func(p string) (Source, bool) {
		return Source{Kind: SourceClock, Rule: auditRule, Clock: models.ClockOffset{Min: -time.Second, Max: 0, Res: time.Second}}, p == auditPath
	}

	// Recording: test-4 sent at 10:01:31.873Z (replayId#7 = 1794304891);
	// deleteApp stamps auditedAt = 1794304891 at .997Z before its reads, then
	// inserts at 10:01:32.003Z (call window [32.002Z, 32.003Z] — next second).
	// Round F: 90s later with the same 1s straddle across the reads.
	rBegin := time.Date(2026, 11, 10, 10, 1, 31, 788_000_000, time.UTC)
	rTest4At := time.Date(2026, 11, 10, 10, 1, 31, 873_000_000, time.UTC)
	rCall := time.Date(2026, 11, 10, 10, 1, 32, 3_000_000, time.UTC)
	fBegin := rBegin.Add(90 * time.Second)
	fTest4At := rTest4At.Add(90 * time.Second)
	fCall := rCall.Add(90 * time.Second)

	rSec := strconv.FormatInt(rTest4At.Unix(), 10)
	fSec := strconv.FormatInt(fTest4At.Unix(), 10)

	roundReq := func(at time.Time, sec string) *models.HTTPReq {
		return &models.HTTPReq{
			Method:    "POST",
			URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
			Timestamp: at,
			Header:    map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
			Body:      `{"replayId":"e2e-ns-e2e-round-` + strconv.FormatInt(at.UnixNano(), 10) + `-auto-replay-` + sec + `-1"}`,
		}
	}
	earlier := clientPlaced(t, "test-4", roundReq(rTest4At, rSec), roundReq(fTest4At, fSec), fTest4At)

	mkInput := func(recVal, liveVal string, withBegin, withRule bool, reqEarlier []Earlier, raw ...Raw) Input {
		w := &Window{
			Rec: Span{From: rCall.Add(-time.Millisecond), To: rCall},
			F:   Span{From: fCall.Add(-time.Millisecond), To: fCall},
		}
		if withBegin {
			w.Rec.Began = rBegin
			w.F.Began = fBegin
		}
		in := iCall(auditPath, String, recVal, liveVal, rCall, fCall, w.ComparesOffsets(), reqEarlier, raw)
		in.Drifts = oClientDrifts()
		if withRule {
			in.Source = auditClock
		}
		return in
	}

	// Without the clock rule, the call's own window does not hold rSec/fSec,
	// so learning binds to test-4's replayId#7 (the premise of the regression).
	noRule := Decide(mkInput(rSec, fSec, true, false, earlier))
	if len(noRule) != 1 || noRule[0].Outcome != OutcomeValue || noRule[0].From.Mock != "test case test-4" {
		t.Fatalf("without clock rule: %s; want Value bound to test case test-4 (premise)", oDescribe(noRule))
	}

	// Asymmetric (-1s in record, 0s in round F) where test-4's round-F request
	// also carries fCall's second so without the fix it likewise binds to test-4.
	fCallSec := strconv.FormatInt(fCall.Unix(), 10)
	asymEarlier := clientPlaced(t, "test-4", roundReq(rTest4At, rSec), roundReq(fTest4At, fCallSec), fTest4At)
	noRuleAsym := Decide(mkInput(rSec, fCallSec, true, false, asymEarlier))
	if len(noRuleAsym) != 1 || noRuleAsym[0].Outcome != OutcomeValue || noRuleAsym[0].From.Mock != "test case test-4" {
		t.Fatalf("asymmetric without clock rule: %s; want Value bound to test case test-4 (premise)", oDescribe(noRuleAsym))
	}

	// With the clock rule declared (with and without test Began), both runs
	// satisfy CheckClockRead for min:-1s max:0s res:1s, so the declared read
	// goes before test-4 and auditedAt is decided a Clock, bound nowhere, with
	// a 1s epoch-s check (and form "text 9999999999" under the time default).
	for _, tc := range []struct {
		name, recV, liveV string
		reqEarlier        []Earlier
	}{
		{"symmetric -1s/-1s", rSec, fSec, earlier},
		{"asymmetric -1s/0s", rSec, fCallSec, asymEarlier},
	} {
		for _, withBegin := range []bool{true, false} {
			for _, timeForm := range []bool{false, true} {
				in := mkInput(tc.recV, tc.liveV, withBegin, true, tc.reqEarlier)
				in.TimeForm = timeForm
				ds := Decide(in)
				if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || ds[0].From.Mock != "" {
					t.Fatalf("%s (withBegin=%v, timeForm=%v): %s; want OutcomeClock bound nowhere", tc.name, withBegin, timeForm, oDescribe(ds))
				}
				c := ds[0].Clock
				if c.Res != time.Second || c.Layout != "epoch-s" {
					t.Fatalf("%s (withBegin=%v, timeForm=%v): clock %+v; want Res=1s Layout=epoch-s", tc.name, withBegin, timeForm, *c)
				}
				if timeForm && c.Form != "text 9999999999" {
					t.Fatalf("%s (withBegin=%v, timeForm=true): clock %+v; want Form='text 9999999999'", tc.name, withBegin, *c)
				}
				if !timeForm && c.Form != "" {
					t.Fatalf("%s (withBegin=%v, timeForm=false): clock %+v; want empty Form under strict times", tc.name, withBegin, *c)
				}
				for _, want := range []string{auditRule, "test case test-4 body.replayId#7"} {
					if !strings.Contains(ds[0].Line, want) {
						t.Errorf("%s (withBegin=%v, timeForm=%v): Line %q must name %q", tc.name, withBegin, timeForm, ds[0].Line, want)
					}
					if !strings.Contains(c.Why, want) {
						t.Errorf("%s (withBegin=%v, timeForm=%v): Clock.Why %q must name %q", tc.name, withBegin, timeForm, c.Why, want)
					}
				}
			}
		}
	}

	// Strict times (timeForm=false) on a protocol whose replay does not compare
	// inferred offsets (!w.ComparesOffsets): an epoch second ending 20ms before
	// the call window (within min:-1s max:0s res:1s) must be settled cleanly by
	// declaredClock (naming the rule and set-aside client place) instead of
	// emitting a misleading "with no rule, declare it noise" cannot-check refusal.
	t.Run("strict times settles uncompared offset cannot-check when declared range holds", func(t *testing.T) {
		rCall20 := time.Date(2026, 11, 10, 10, 1, 32, 20_000_000, time.UTC)
		fCall20 := rCall20.Add(90 * time.Second)
		mkStrict := func(recVal, liveVal string, withRule bool, reqEarlier []Earlier) Input {
			w := &Window{
				Rec: Span{From: rCall20, To: rCall20.Add(time.Millisecond)},
				F:   Span{From: fCall20, To: fCall20.Add(time.Millisecond)},
			}
			in := iCall(auditPath, String, recVal, liveVal, rCall20, fCall20, w, reqEarlier, nil)
			in.Drifts = oClientDrifts()
			in.TimeForm = false
			if withRule {
				in.Source = auditClock
			}
			return in
		}
		// Premise: without the clock rule, binds to test-4.
		if ds := Decide(mkStrict(rSec, fSec, false, earlier)); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case test-4" {
			t.Fatalf("strict without clock rule: %s; want Value bound to test case test-4", oDescribe(ds))
		}
		// With the clock rule and timeForm=false: settled as OutcomeClock
		// with Res=1s, Layout=epoch-s, empty Form, naming rule + client place.
		ds := Decide(mkStrict(rSec, fSec, true, earlier))
		if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || ds[0].From.Mock != "" {
			t.Fatalf("strict with clock rule (!ComparesOffsets): %s; want OutcomeClock", oDescribe(ds))
		}
		c := ds[0].Clock
		if c.Min != 0 || c.Max != 0 || c.Res != time.Second || c.Layout != "epoch-s" || c.Form != "" {
			t.Fatalf("strict with clock rule: clock %+v; want plain Res=1s Layout=epoch-s Form=''", *c)
		}
		if strings.Contains(ds[0].Line, "with no rule") {
			t.Errorf("strict with clock rule: Line %q must not say 'with no rule'", ds[0].Line)
		}
		for _, want := range []string{auditRule, "test case test-4 body.replayId#7"} {
			if !strings.Contains(ds[0].Line, want) {
				t.Errorf("strict with clock rule: Line %q must name %q", ds[0].Line, want)
			}
			if !strings.Contains(c.Why, want) {
				t.Errorf("strict with clock rule: Clock.Why %q must name %q", c.Why, want)
			}
		}
		// Asymmetric straddle (-800ms in rec, +200ms in F) with no Began and
		// timeForm=false: settled cleanly by declaredClock when declaredClockHolds.
		rCall800 := time.Date(2026, 11, 10, 10, 1, 32, 800_000_000, time.UTC)
		fCall200 := time.Date(2026, 11, 10, 10, 3, 2, 200_000_000, time.UTC)
		wAsym := &Window{
			Rec: Span{From: rCall800, To: rCall800.Add(time.Millisecond)},
			F:   Span{From: fCall200, To: fCall200.Add(time.Millisecond)},
		}
		inAsym := iCall(auditPath, String, rSec, fCallSec, rCall800, fCall200, wAsym, asymEarlier, nil)
		inAsym.Drifts = oClientDrifts()
		inAsym.Source = auditClock
		inAsym.TimeForm = false
		dsAsym := Decide(inAsym)
		if len(dsAsym) != 1 || dsAsym[0].Outcome != OutcomeClock || dsAsym[0].Clock == nil || dsAsym[0].Clock.Res != time.Second || dsAsym[0].Clock.Form != "" {
			t.Fatalf("strict asymmetric straddle: %s; want OutcomeClock Res=1s strict", oDescribe(dsAsym))
		}
		for _, want := range []string{auditRule, "test case test-4 body.replayId#7"} {
			if !strings.Contains(dsAsym[0].Line, want) {
				t.Errorf("strict asymmetric straddle: Line %q must name %q", dsAsym[0].Line, want)
			}
		}
		// Guardrail: if the call's own recorded answer echoes rSec, it must
		// STILL fail closed as RefusalCannotCheck under strict times, AND
		// name both the declared rule and set-aside client place under both
		// strict times and the time default.
		inEcho := mkStrict(rSec, fSec, true, earlier)
		inEcho.Resp = []Leaf{{Path: "body.auditedAt", Noise: "body.auditedAt", Type: String, Value: rSec}}
		dsEcho := Decide(inEcho)
		if len(dsEcho) != 1 || dsEcho[0].Refusal == nil || dsEcho[0].Refusal.Kind != RefusalCannotCheck {
			t.Fatalf("FALSE GREEN? strict with echoed answer: %s; want RefusalCannotCheck", oDescribe(dsEcho))
		}
		for _, want := range []string{auditRule, "test case test-4 body.replayId#7"} {
			if !strings.Contains(dsEcho[0].Line, want) {
				t.Errorf("strict with echoed answer: Line %q must name %q", dsEcho[0].Line, want)
			}
		}
		inEcho.TimeForm = true
		dsEchoForm := Decide(inEcho)
		if len(dsEchoForm) != 1 || dsEchoForm[0].Outcome != OutcomeClock || dsEchoForm[0].Clock == nil || dsEchoForm[0].Clock.Form == "" {
			t.Fatalf("time default with echoed answer: %s; want form-checked OutcomeClock", oDescribe(dsEchoForm))
		}
		for _, want := range []string{auditRule, "test case test-4 body.replayId#7"} {
			if !strings.Contains(dsEchoForm[0].Line, want) || !strings.Contains(dsEchoForm[0].Clock.Why, want) {
				t.Errorf("time default with echoed answer: Line %q / Clock.Why %q must name %q", dsEchoForm[0].Line, dsEchoForm[0].Clock.Why, want)
			}
		}
		if !strings.Contains(dsEchoForm[0].Line, ". Places: test case test-4 body.replayId#7") {
			t.Errorf("time default with echoed answer: Line %q must list set-aside client in Places", dsEchoForm[0].Line)
		}
		// Guardrail: a one-sided declared clock rule (min:-60m1s max:-59m59s,
		// !off.Plain(), Checkable Unit=1h so declaredClockHolds genuinely returns
		// true) on a protocol that does not compare offsets (!w.offsets) must
		// NOT settle RefusalCannotCheck OR strip incoming client requests under
		// strict times.
		hourR := strconv.FormatInt(rCall20.Add(-time.Hour).Unix(), 10)
		hourF := strconv.FormatInt(fCall20.Add(-time.Hour).Unix(), 10)
		oneSidedOff := models.ClockOffset{Min: -time.Hour - time.Second, Max: -time.Hour + time.Second, Res: time.Second}
		oneSided := func(p string) (Source, bool) {
			return Source{Kind: SourceClock, Rule: "hour-lookback", Clock: oneSidedOff}, p == auditPath
		}
		inOneSided := mkStrict(hourR, hourF, false, nil)
		inOneSided.Source = oneSided
		// Prove the one-sided range genuinely holds both runs under CheckClockRead
		// against inOneSided's actual call window so this guardrail genuinely
		// exercises declaredClockComparable (never vacuous).
		oneSidedCheck := oneSidedOff
		oneSidedCheck.Layout = "epoch-s"
		if okR, whyR := CheckClockRead(auditPath, oneSidedCheck, Leaf{Path: auditPath, Type: String, Value: hourR}, inOneSided.Window.Rec, true); !okR {
			t.Fatalf("test setup: one-sided rule must hold recorded run: %s", whyR)
		}
		if okF, whyF := CheckClockRead(auditPath, oneSidedCheck, Leaf{Path: auditPath, Type: String, Value: hourF}, inOneSided.Window.F, true); !okF {
			t.Fatalf("test setup: one-sided rule must hold round F: %s", whyF)
		}
		dsOneSided := Decide(inOneSided)
		if len(dsOneSided) != 1 || dsOneSided[0].Refusal == nil || dsOneSided[0].Refusal.Kind != RefusalCannotCheck {
			t.Fatalf("FALSE GREEN? one-sided rule on !ComparesOffsets window: %s; want RefusalCannotCheck", oDescribe(dsOneSided))
		}
		// With client test-4 holding hourR/hourF on !ComparesOffsets: client is
		// NOT stripped (stays bound to test-4 rather than failing cannot-check).
		hourE := clientPlaced(t, "test-4", roundReq(rTest4At, hourR), roundReq(fTest4At, hourF), fTest4At)
		inOneSidedClient := mkStrict(hourR, hourF, false, hourE)
		inOneSidedClient.Source = oneSided
		if dsOneClient := Decide(inOneSidedClient); len(dsOneClient) != 1 || dsOneClient[0].Outcome != OutcomeValue || dsOneClient[0].From.Mock != "test case test-4" {
			t.Fatalf("one-sided rule with client on !ComparesOffsets window: %s; want Value bound to test case test-4", oDescribe(dsOneClient))
		}
		inOneSided.Window = inOneSided.Window.ComparesOffsets()
		dsOneSidedOK := Decide(inOneSided)
		if len(dsOneSidedOK) != 1 || dsOneSidedOK[0].Outcome != OutcomeClock {
			t.Fatalf("one-sided rule on ComparesOffsets window: %s; want OutcomeClock", oDescribe(dsOneSidedOK))
		}
		// Legibility: a JSON client body {"stamp":"<sec>"} names only the parsed
		// leaf place (test case test-4 body.stamp), never duplicating raw body
		// or body#1 word indices.
		stampReq := func(at time.Time, sec string) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "application/json"},
				Body:      `{"stamp":"` + sec + `"}`,
			}
		}
		stampE := clientPlaced(t, "test-4", stampReq(rTest4At, rSec), stampReq(fTest4At, fSec), fTest4At)
		inStamp := mkStrict(rSec, fSec, false, stampE)
		inStamp.Source = auditClock
		dsStamp := Decide(inStamp)
		if len(dsStamp) != 1 || dsStamp[0].Outcome != OutcomeClock {
			t.Fatalf("JSON client stamp: %s; want OutcomeClock", oDescribe(dsStamp))
		}
		if !strings.Contains(dsStamp[0].Line, "set aside test case test-4 body.stamp") || !strings.Contains(dsStamp[0].Clock.Why, "set aside test case test-4 body.stamp") {
			t.Errorf("JSON client stamp: Line %q / Clock.Why %q must name 'set aside test case test-4 body.stamp'", dsStamp[0].Line, dsStamp[0].Clock.Why)
		}
		if strings.Contains(dsStamp[0].Line, "body#") || strings.Contains(dsStamp[0].Line, ", test case test-4 body") || strings.Contains(dsStamp[0].Clock.Why, "body#") || strings.Contains(dsStamp[0].Clock.Why, ", test case test-4 body") {
			t.Errorf("JSON client stamp: Line %q / Clock.Why %q must not duplicate raw body/body#N alongside leaf body.stamp", dsStamp[0].Line, dsStamp[0].Clock.Why)
		}
		// Non-JSON client body (text/plain, no parsed body.* leaves): the raw
		// body place IS kept and named ("set aside test case test-4 body").
		plainReq := func(at time.Time, sec string) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "text/plain"},
				Body:      sec,
			}
		}
		plainE := clientPlaced(t, "test-4", plainReq(rTest4At, rSec), plainReq(fTest4At, fSec), fTest4At)
		inPlain := mkStrict(rSec, fSec, false, plainE)
		inPlain.Source = auditClock
		dsPlain := Decide(inPlain)
		if len(dsPlain) != 1 || dsPlain[0].Outcome != OutcomeClock {
			t.Fatalf("text/plain client body: %s; want OutcomeClock", oDescribe(dsPlain))
		}
		if !strings.Contains(dsPlain[0].Line, "set aside test case test-4 body") || !strings.Contains(dsPlain[0].Clock.Why, "set aside test case test-4 body") {
			t.Errorf("text/plain client body: Line %q / Clock.Why %q must name 'set aside test case test-4 body'", dsPlain[0].Line, dsPlain[0].Clock.Why)
		}
		// Guardrail: when declaredClockDropOK strips a client request and
		// instantLearned falls through to step 8 RefusalNoClockRead (e.g. a
		// typed Instant whose two runs sit at different offsets before the
		// call window with no test Began, so the offset guard refuses),
		// declaredClock settles the fallback RefusalNoClockRead into
		// OutcomeClock (naming the rule and set-aside client place) instead of
		// regressing to a contradictory RefusalNoClockRead under strict times,
		// while still refusing if the call's own recorded answer echoes it.
		rTyped := time.Date(2026, 11, 10, 10, 1, 0, 0, time.UTC)
		fTyped := time.Date(2026, 11, 10, 10, 2, 0, 0, time.UTC)
		rTypedS := rTyped.UTC().Format(time.RFC3339)
		fTypedS := fTyped.UTC().Format(time.RFC3339)
		typedClientReq := func(at time.Time, stamp string) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "application/json"},
				Body:      `{"replayId":"` + stamp + `"}`,
			}
		}
		typedE := clientPlaced(t, "test-4", typedClientReq(rTest4At, rTypedS), typedClientReq(fTest4At, fTypedS), fTest4At)
		wTyped := &Window{
			Rec: Span{From: rCall20, To: rCall20.Add(time.Millisecond)},
			F:   Span{From: fCall20, To: fCall20.Add(time.Millisecond)},
		}
		inStep8 := iCall(auditPath, Instant, rTypedS, fTypedS, rCall20, fCall20, wTyped, typedE, nil)
		inStep8.Drifts = oClientDrifts()
		// Premise: without the declared clock rule, binds to test-4.
		if dsNoRule := Decide(inStep8); len(dsNoRule) != 1 || dsNoRule[0].Outcome != OutcomeValue || dsNoRule[0].From.Mock != "test case test-4" {
			t.Fatalf("step-8 typed without clock rule: %s; want Value bound to test case test-4 (premise)", oDescribe(dsNoRule))
		}
		inStep8.Source = func(p string) (Source, bool) {
			return Source{Kind: SourceClock, Rule: auditRule, Clock: models.ClockOffset{Min: -time.Minute, Max: 0, Res: time.Minute}}, p == auditPath
		}
		for _, timeForm := range []bool{false, true} {
			in := inStep8
			in.TimeForm = timeForm
			dsStep8 := Decide(in)
			if len(dsStep8) != 1 || dsStep8[0].Outcome != OutcomeClock || dsStep8[0].Clock == nil || dsStep8[0].From.Mock != "" {
				t.Fatalf("step-8 typed (timeForm=%v): %s; want OutcomeClock", timeForm, oDescribe(dsStep8))
			}
			c := dsStep8[0].Clock
			if c.Layout != models.LayoutTyped || c.Res != time.Minute {
				t.Fatalf("step-8 typed (timeForm=%v): clock %+v; want LayoutTyped Res=1m", timeForm, *c)
			}
			if !timeForm && c.Form != "" {
				t.Fatalf("step-8 typed (timeForm=false): clock %+v; want empty Form under strict times", *c)
			}
			if timeForm && c.Form == "" {
				t.Fatalf("step-8 typed (timeForm=true): clock %+v; want non-empty Form under time default", *c)
			}
			for _, want := range []string{auditRule, "test case test-4 body.replayId", ". Places: test case test-4 body.replayId"} {
				if !strings.Contains(dsStep8[0].Line, want) {
					t.Errorf("step-8 typed (timeForm=%v): Line %q must contain %q", timeForm, dsStep8[0].Line, want)
				}
			}
			for _, want := range []string{auditRule, "test case test-4 body.replayId"} {
				if !strings.Contains(c.Why, want) {
					t.Errorf("step-8 typed (timeForm=%v): Clock.Why %q must contain %q", timeForm, c.Why, want)
				}
			}
		}
		// No-client control row on the same step-8 typed-instant geometry:
		// declaredClock settles RefusalNoClockRead identically whether or not
		// a client request arrived (matching HTTP's declaredUnread rule).
		inStep8NoClient := inStep8
		inStep8NoClient.Earlier = nil
		if dsNoC := Decide(inStep8NoClient); len(dsNoC) != 1 || dsNoC[0].Outcome != OutcomeClock || dsNoC[0].Clock == nil || dsNoC[0].Clock.Layout != models.LayoutTyped || dsNoC[0].Clock.Res != time.Minute || dsNoC[0].Clock.Form != "" {
			t.Fatalf("step-8 typed no-client control: %s; want strict OutcomeClock LayoutTyped Res=1m", oDescribe(dsNoC))
		}
		// Non-echo DB refusal after client stripping (RefusalConflict from a
		// Given holding rSec in recorded and another value in round F): still
		// refuses RefusalConflict AND names the declared rule and set-aside
		// client place (matching HTTP's withClientAside on every refusal).
		inConflict := mkStrict(rSec, fSec, true, append(append([]Earlier(nil), earlier...), Earlier{
			Mock: "given.stamp", Given: true, Gave: true,
			Seen: []Seen{{Leaf: Leaf{Path: "stamp", Type: String, Value: rSec}, Live: "9999999999"}},
		}))
		dsConflict := Decide(inConflict)
		if len(dsConflict) != 1 || dsConflict[0].Refusal == nil || dsConflict[0].Refusal.Kind != RefusalConflict {
			t.Fatalf("strict with conflict + stripped client: %s; want RefusalConflict", oDescribe(dsConflict))
		}
		for _, want := range []string{"given.stamp", auditRule, "test case test-4 body.replayId#7"} {
			if !strings.Contains(dsConflict[0].Line, want) {
				t.Errorf("strict with conflict + stripped client: Line %q must name %q", dsConflict[0].Line, want)
			}
		}
		// Secret masking guardrail: when a set-aside client place is
		// secret-named (e.g. header.Authorization), BOTH DB refusals
		// (hideSecrets in instant) and DB bound Value lines (learnedLine via
		// bound's places) mask the recorded/full values as "***".
		secretReq := func(at time.Time, sec string) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "application/json", "Authorization": sec},
				Body:      `{"ok":true}`,
			}
		}
		secretE := clientPlaced(t, "test-4", secretReq(rTest4At, rSec), secretReq(fTest4At, fSec), fTest4At)
		inSecretEcho := mkStrict(rSec, fSec, true, secretE)
		inSecretEcho.Resp = []Leaf{{Path: "body.auditedAt", Noise: "body.auditedAt", Type: String, Value: rSec}}
		dsSecretEcho := Decide(inSecretEcho)
		if len(dsSecretEcho) != 1 || dsSecretEcho[0].Refusal == nil || dsSecretEcho[0].Refusal.Kind != RefusalCannotCheck {
			t.Fatalf("secret client echoed refusal: %s; want RefusalCannotCheck", oDescribe(dsSecretEcho))
		}
		for _, want := range []string{`"***"`, auditRule, "test case test-4 header.Authorization"} {
			if !strings.Contains(dsSecretEcho[0].Line, want) {
				t.Errorf("secret client echoed refusal: Line %q must contain %q", dsSecretEcho[0].Line, want)
			}
		}
		for _, forbid := range []string{rSec, fSec} {
			if strings.Contains(dsSecretEcho[0].Line, forbid) {
				t.Errorf("secret client echoed refusal: Line %q leaked secret value %q", dsSecretEcho[0].Line, forbid)
			}
		}
		ansSecret := func(v string) Raw { return oAnswer("mock-7", 100*time.Millisecond, v) }
		inSecretBound := mkStrict(rSec, fSec, true, secretE)
		inSecretBound.Raw = []Raw{ansSecret(rSec).ServedAs(1).ServedChanged(ansSecret(rSec), ansSecret(fSec))}
		dsSecretBound := Decide(inSecretBound)
		if len(dsSecretBound) != 1 || dsSecretBound[0].Outcome != OutcomeValue || dsSecretBound[0].From.Mock != "mock-7" {
			t.Fatalf("secret client bound value: %s; want Value bound to mock-7", oDescribe(dsSecretBound))
		}
		for _, want := range []string{`"***"`, auditRule, "test case test-4 header.Authorization"} {
			if !strings.Contains(dsSecretBound[0].Line, want) {
				t.Errorf("secret client bound value: Line %q must contain %q", dsSecretBound[0].Line, want)
			}
		}
		for _, forbid := range []string{rSec, fSec} {
			if strings.Contains(dsSecretBound[0].Line, forbid) {
				t.Errorf("secret client bound value: Line %q leaked secret value %q", dsSecretBound[0].Line, forbid)
			}
		}
		// Full-places secret masking guardrail: even when c.aside already has
		// maxLinePlaces (3) non-secret places, dc.clock still appends the
		// secret-named client place to Places so learnedLine masks values as
		// "***" rather than truncating it before secretAt runs.
		inSecretFull := mkStrict(rSec, fSec, true, secretE)
		for i := 1; i <= maxLinePlaces; i++ {
			name := "mock-aside-" + strconv.Itoa(i)
			inSecretFull.Raw = append(inSecretFull.Raw,
				oAnswer(name, time.Duration(100+i)*time.Millisecond, rSec).ServedAs(i).ServedChanged(
					oAnswer(name, time.Duration(100+i)*time.Millisecond, rSec),
					oAnswer(name, time.Duration(100+i)*time.Millisecond, "9999999999"),
				))
		}
		dsSecretFull := Decide(inSecretFull)
		if len(dsSecretFull) != 1 || dsSecretFull[0].Outcome != OutcomeClock || dsSecretFull[0].Clock == nil {
			t.Fatalf("secret client full-aside clock: %s; want OutcomeClock", oDescribe(dsSecretFull))
		}
		for _, want := range []string{`"***"`, auditRule, "test case test-4 header.Authorization"} {
			if !strings.Contains(dsSecretFull[0].Line, want) {
				t.Errorf("secret client full-aside clock: Line %q must contain %q", dsSecretFull[0].Line, want)
			}
		}
		for _, forbid := range []string{rSec, fSec} {
			if strings.Contains(dsSecretFull[0].Line, forbid) {
				t.Errorf("secret client full-aside clock: Line %q leaked secret value %q", dsSecretFull[0].Line, forbid)
			}
		}

		// Echo guard on that settled step-8 path: recorded answer holding
		// rTyped still refuses RefusalCannotCheck under strict times, naming
		// the rule and set-aside client place.
		inStep8Echo := inStep8
		inStep8Echo.Resp = []Leaf{{Path: "body.auditedAt", Noise: "body.auditedAt", Type: Instant, Value: rTypedS}}
		dsStep8Echo := Decide(inStep8Echo)
		if len(dsStep8Echo) != 1 || dsStep8Echo[0].Refusal == nil || dsStep8Echo[0].Refusal.Kind != RefusalCannotCheck {
			t.Fatalf("FALSE GREEN? step-8 typed with echoed answer: %s; want RefusalCannotCheck", oDescribe(dsStep8Echo))
		}
		for _, want := range []string{auditRule, "test case test-4 body.replayId"} {
			if !strings.Contains(dsStep8Echo[0].Line, want) {
				t.Errorf("step-8 typed echoed answer: Line %q must name %q", dsStep8Echo[0].Line, want)
			}
		}

		// Guardrail: out-of-range client timestamp under strict times &
		// !ComparesOffsets stays bound to test-4.
		oldR := strconv.FormatInt(rTest4At.Add(-2*time.Minute).Unix(), 10)
		oldF := strconv.FormatInt(fTest4At.Add(-2*time.Minute).Unix(), 10)
		oldE := clientPlaced(t, "test-4", roundReq(rTest4At, oldR), roundReq(fTest4At, oldF), fTest4At)
		if dsOut := Decide(mkStrict(oldR, oldF, true, oldE)); len(dsOut) != 1 || dsOut[0].Outcome != OutcomeValue || dsOut[0].From.Mock != "test case test-4" {
			t.Fatalf("FALSE GREEN? strict out-of-range: %s; want Value bound to test case test-4", oDescribe(dsOut))
		}
	})

	// Guardrail 1: BOTH runs must satisfy the declared clock range. Whenever
	// either run (or both) is outside the declared range, the client request
	// stays eligible and the value stays bound to test-4 (under strict and
	// default times alike).
	oldRSec := strconv.FormatInt(rTest4At.Add(-2*time.Minute).Unix(), 10)
	oldFSec := strconv.FormatInt(fTest4At.Add(-2*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name, recV, liveV string
	}{
		{"both runs 2m outside range", oldRSec, oldFSec},
		{"only recorded run in range, round F 2m outside", rSec, oldFSec},
		{"recorded run 2m outside, only round F in range", oldRSec, fSec},
	} {
		reqE := clientPlaced(t, "test-4", roundReq(rTest4At, tc.recV), roundReq(fTest4At, tc.liveV), fTest4At)
		for _, timeForm := range []bool{false, true} {
			in := mkInput(tc.recV, tc.liveV, true, true, reqE)
			in.TimeForm = timeForm
			if ds := Decide(in); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case test-4" {
				t.Fatalf("FALSE GREEN? %s (timeForm=%v): %s; want Value bound to test case test-4", tc.name, timeForm, oDescribe(ds))
			}
		}
	}

	// Guardrail 2: an in-scope DB answer holding the same second still binds
	// as Value even when both the clock rule and test-4 are present, and its
	// learned line names both the declared rule and the set-aside client place.
	ans := func(v string) Raw { return oAnswer("mock-7", 100*time.Millisecond, v) }
	dbAns := ans(rSec).ServedAs(1).ServedChanged(ans(rSec), ans(fSec))
	for _, timeForm := range []bool{false, true} {
		inWithDB := mkInput(rSec, fSec, true, true, earlier, dbAns)
		inWithDB.TimeForm = timeForm
		d := oWord(t, Decide(inWithDB), rSec, fSec)
		iValue(t, "in-scope DB answer beside client request and clock rule", d, "mock-7")
		for _, want := range []string{auditRule, "test case test-4 body.replayId#7"} {
			if !strings.Contains(d.Line, want) {
				t.Errorf("in-scope DB answer (timeForm=%v): Line %q must name %q", timeForm, d.Line, want)
			}
		}
	}

	// Guardrail 3: edge cases where declaredClockHolds must return false
	// (never strip client requests): nil window, invalid range (Res=0 or
	// Max<Min), mismatched layouts across runs, and a word inside longer text.
	t.Run("nil window does not strip client request", func(t *testing.T) {
		in := mkInput(rSec, fSec, false, true, earlier)
		in.Window = nil
		if ds := Decide(in); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case test-4" {
			t.Fatalf("nil window: %s; want Value bound to test case test-4", oDescribe(ds))
		}
	})
	for name, bad := range map[string]models.ClockOffset{
		"zero resolution": {Min: -time.Second, Max: 0, Res: 0},
		"max below min":   {Min: time.Second, Max: -time.Second, Res: time.Second},
	} {
		t.Run("invalid range "+name+" does not strip client request", func(t *testing.T) {
			in := mkInput(rSec, fSec, true, true, earlier)
			in.Source = func(p string) (Source, bool) {
				return Source{Kind: SourceClock, Rule: auditRule, Clock: bad}, p == auditPath
			}
			if ds := Decide(in); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case test-4" {
				t.Fatalf("invalid range %s: %s; want Value bound to test case test-4", name, oDescribe(ds))
			}
		})
	}
	t.Run("mismatched layouts across runs does not strip client request", func(t *testing.T) {
		rfc := rTest4At.UTC().Format(time.RFC3339)
		mixReq := func(at time.Time, val string) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
				Body:      `{"stamp":"` + val + `"}`,
			}
		}
		mixE := clientPlaced(t, "test-4", mixReq(rTest4At, rfc), mixReq(fTest4At, fSec), fTest4At)
		in := mkInput(rfc, fSec, true, true, mixE)
		if ds := Decide(in); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case test-4" {
			t.Fatalf("mismatched layouts: %s; want Value bound to test case test-4", oDescribe(ds))
		}
	})
	t.Run("word inside longer text does not strip client request", func(t *testing.T) {
		// Confirms end-to-end that a timestamp word embedded in longer text stays
		// bound to the client request (both the word guard in declaredClockHolds
		// and CheckClockRead reading the whole leaf refuse a compound string).
		in := mkInput("audit-"+rSec, "audit-"+fSec, true, true, earlier)
		if ds := Decide(in); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "test case test-4" {
			t.Fatalf("word inside longer text: %s; want Value bound to test case test-4", oDescribe(ds))
		}
	})
	t.Run("typed Instant leaf strips client request only when declared range holds", func(t *testing.T) {
		typedReq := func(at time.Time, val string) *models.HTTPReq {
			return &models.HTTPReq{
				Method:    "POST",
				URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
				Timestamp: at,
				Header:    map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
				Body:      `{"stamp":"` + val + `"}`,
			}
		}
		mkTyped := func(recT, liveT time.Time, withBegin, withRule bool) Input {
			recS := recT.UTC().Format(time.RFC3339)
			liveS := liveT.UTC().Format(time.RFC3339)
			reqE := clientPlaced(t, "test-4", typedReq(rTest4At, recS), typedReq(fTest4At, liveS), fTest4At)
			w := &Window{
				Rec: Span{From: rCall.Add(-time.Millisecond), To: rCall},
				F:   Span{From: fCall.Add(-time.Millisecond), To: fCall},
			}
			if withBegin {
				w.Rec.Began = rBegin
				w.F.Began = fBegin
			}
			in := iCall(auditPath, Instant, recS, liveS, rCall, fCall, w.ComparesOffsets(), reqE, nil)
			in.Drifts = oClientDrifts()
			if withRule {
				in.Source = auditClock
			}
			return in
		}
		// Premise: without the clock rule, the typed instant binds to test-4.
		dsNoRule := Decide(mkTyped(rTest4At.Truncate(time.Second), fTest4At.Truncate(time.Second), true, false))
		if len(dsNoRule) != 1 || dsNoRule[0].Outcome != OutcomeValue || dsNoRule[0].From.Mock != "test case test-4" {
			t.Fatalf("typed instant without clock rule: %s; want Value bound to test case test-4 (premise)", oDescribe(dsNoRule))
		}
		// In range (with and without test Began): clients stripped,
		// decided Clock with LayoutTyped, naming rule + set-aside client place.
		for _, withBegin := range []bool{true, false} {
			ds := Decide(mkTyped(rTest4At.Truncate(time.Second), fTest4At.Truncate(time.Second), withBegin, true))
			if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil || ds[0].Clock.Layout != models.LayoutTyped || ds[0].From.Mock != "" {
				t.Fatalf("typed instant in range (withBegin=%v): %s; want OutcomeClock LayoutTyped bound nowhere", withBegin, oDescribe(ds))
			}
			for _, want := range []string{auditRule, "test case test-4 body.stamp"} {
				if !strings.Contains(ds[0].Line, want) {
					t.Errorf("typed instant (withBegin=%v): Line %q must name %q", withBegin, ds[0].Line, want)
				}
				if !strings.Contains(ds[0].Clock.Why, want) {
					t.Errorf("typed instant (withBegin=%v): Clock.Why %q must name %q", withBegin, ds[0].Clock.Why, want)
				}
			}
		}
		// Out of range: client request is not stripped and value stays
		// bound to test-4.
		dsOut := Decide(mkTyped(rTest4At.Add(-2*time.Minute).Truncate(time.Second), fTest4At.Add(-2*time.Minute).Truncate(time.Second), true, true))
		if len(dsOut) != 1 || dsOut[0].Outcome != OutcomeValue || dsOut[0].From.Mock != "test case test-4" {
			t.Fatalf("typed instant out of range: %s; want Value bound to test case test-4", oDescribe(dsOut))
		}
	})
}

func TestDeclaredClockStrictEchoRefusal(t *testing.T) {
	rCall := time.Date(2026, 11, 10, 10, 1, 32, 3_000_000, time.UTC)
	fCall := rCall.Add(90 * time.Second)
	wPlain := &Window{
		Rec: Span{From: rCall.Add(-time.Millisecond), To: rCall},
		F:   Span{From: fCall.Add(-time.Millisecond), To: fCall},
	}
	mkLeaf := func(sec time.Time) Leaf {
		return Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: Number, Value: strconv.FormatInt(sec.Unix(), 10)}
	}
	plainRule := Source{
		Kind:  SourceClock,
		Rule:  "pat-created-at",
		Clock: models.ClockOffset{Min: -time.Second, Max: 0, Res: time.Second},
	}
	unread := func(rec, live time.Time, aside string) Learned {
		const rawWhy = "no clock read by the app matched it; declare it noise in keploy.yml"
		return Learned{
			Kind:        KindUnaccounted,
			Recorded:    mkLeaf(rec),
			Live:        mkLeaf(live),
			Why:         rawWhy,
			unreadWhy:   rawWhy,
			clientAside: aside,
		}
	}

	t.Run("in-range comparable rule offers mock.times default remedy and names set-aside client", func(t *testing.T) {
		got := DeclaredClockStrictEchoRefusal(unread(rCall.Add(-time.Second), fCall.Add(-time.Second), "test case test-4 body.replayId#7"), plainRule, wPlain)
		for _, want := range []string{"pat-created-at", "Leave mock.times at its default", "call's window", "set aside test case test-4 body.replayId#7"} {
			if !strings.Contains(got.Why, want) {
				t.Errorf("got.Why = %q; want %q", got.Why, want)
			}
		}
		if strings.Contains(got.Why, "declare it noise") {
			t.Errorf("got.Why = %q; must not advise declaring noise", got.Why)
		}
	})

	t.Run("nil call window keeps windowless hooks/begin diagnosis unchanged", func(t *testing.T) {
		const winWhy = "keploy saw no read of the app's clock for its call (keploy's clock hooks did not report the application's clock, or no earlier call and no begin of the test bounds the read)"
		orig := Learned{
			Kind:      KindUnaccounted,
			Recorded:  mkLeaf(rCall.Add(-time.Second)),
			Live:      mkLeaf(fCall.Add(-time.Second)),
			Why:       winWhy,
			unreadWhy: winWhy,
		}
		got := DeclaredClockStrictEchoRefusal(orig, plainRule, nil)
		if got.Why != orig.Why {
			t.Errorf("got.Why = %q; want unchanged windowless diagnosis %q", got.Why, orig.Why)
		}
	})

	t.Run("non-instant values keep original diagnosis unchanged", func(t *testing.T) {
		const rawWhy = "no clock read by the app matched it (an instant, its characters 3 to 12)"
		for _, tc := range []struct {
			name      string
			rec, live Leaf
		}{
			{
				name: "time of day",
				rec:  Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: String, Value: "10:01:31"},
				live: Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: String, Value: "10:03:01"},
			},
			{
				name: "text-embedded epoch",
				rec:  Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: String, Value: "v-" + strconv.FormatInt(rCall.Add(-time.Second).Unix(), 10)},
				live: Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: String, Value: "v-" + strconv.FormatInt(fCall.Add(-time.Second).Unix(), 10)},
			},
			{
				name: "typed time of day",
				rec:  TimeOfDayLeaf("body.data.createdAt", "body.data.createdAt", 10*time.Hour+time.Minute+31*time.Second, time.Second),
				live: TimeOfDayLeaf("body.data.createdAt", "body.data.createdAt", 10*time.Hour+3*time.Minute+1*time.Second, time.Second),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				orig := Learned{
					Kind:      KindUnaccounted,
					Recorded:  tc.rec,
					Live:      tc.live,
					Why:       rawWhy,
					unreadWhy: rawWhy,
				}
				got := DeclaredClockStrictEchoRefusal(orig, plainRule, wPlain)
				if got.Why != orig.Why {
					t.Errorf("got.Why = %q; want unchanged %q", got.Why, orig.Why)
				}
			})
		}
	})

	t.Run("incompatible layouts across runs keep original diagnosis unchanged", func(t *testing.T) {
		const rawWhy = "no clock read by the app matched it; declare it noise in keploy.yml"
		orig := Learned{
			Kind:      KindUnaccounted,
			Recorded:  Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: Number, Value: strconv.FormatInt(rCall.Add(-time.Second).Unix(), 10)},
			Live:      Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: Number, Value: strconv.FormatInt(fCall.Add(-time.Second).UnixMilli(), 10)},
			Why:       rawWhy,
			unreadWhy: rawWhy,
		}
		got := DeclaredClockStrictEchoRefusal(orig, plainRule, wPlain)
		if got.Why != orig.Why {
			t.Errorf("got.Why = %q; want unchanged %q", got.Why, orig.Why)
		}
	})

	t.Run("out-of-range value names range failure and omits mock.times default remedy", func(t *testing.T) {
		got := DeclaredClockStrictEchoRefusal(unread(rCall.Add(-2*time.Minute), fCall.Add(-2*time.Minute), ""), plainRule, wPlain)
		for _, want := range []string{"pat-created-at", "the recorded value is no such read", "Adjust keploy.yml's rule pat-created-at"} {
			if !strings.Contains(got.Why, want) {
				t.Errorf("got.Why = %q; want %q", got.Why, want)
			}
		}
		for _, bad := range []string{"declare it noise", "mock.times at its default"} {
			if strings.Contains(got.Why, bad) {
				t.Errorf("got.Why = %q; must not contain %q", got.Why, bad)
			}
		}
	})

	t.Run("non-comparable offset rule on non-offset window omits mock.times default remedy", func(t *testing.T) {
		offsetRule := Source{
			Kind:  SourceClock,
			Rule:  "pat-offset",
			Clock: models.ClockOffset{Min: -60*time.Minute - time.Second, Max: -60*time.Minute + time.Second, Res: time.Second},
		}
		got := DeclaredClockStrictEchoRefusal(unread(rCall.Add(-time.Hour), fCall.Add(-time.Hour), ""), offsetRule, wPlain)
		for _, want := range []string{"pat-offset", "does not compare an offset rule on this window", "Adjust keploy.yml's rule pat-offset"} {
			if !strings.Contains(got.Why, want) {
				t.Errorf("got.Why = %q; want %q", got.Why, want)
			}
		}
		for _, bad := range []string{"declare it noise", "mock.times at its default"} {
			if strings.Contains(got.Why, bad) {
				t.Errorf("got.Why = %q; must not contain %q", got.Why, bad)
			}
		}
	})

	t.Run("non-unread refusal (e.g. earlier DB answer mock-7) is left untouched", func(t *testing.T) {
		orig := Learned{
			Kind:     KindUnaccounted,
			Recorded: mkLeaf(rCall.Add(-time.Second)),
			Live:     mkLeaf(fCall.Add(-time.Second)),
			Why:      "its recorded value stands in mock-7",
		}
		got := DeclaredClockStrictEchoRefusal(orig, plainRule, wPlain)
		if got.Why != orig.Why {
			t.Errorf("got.Why = %q; want unchanged %q", got.Why, orig.Why)
		}
	})
}
