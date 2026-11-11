package learn

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// Rows of keploy.yml's kind: minted (Input.Source): a field whose whole
// value the application makes anew at its call. api-server's app_config_audits
// and service-account token inserts carry a v4 UUID _id; a 16-bit group of it
// stands, by chance, in another id of an earlier place, and learning refuses
// that group as unknown, so the call was set aside. The rule decides the id
// made anew whole, bound at the call, unless learning placed it, keploy could
// not read what it checks, the value is no plain string or number, round F's
// value has another shape, or an earlier place holds the whole value.

const (
	mRule      = "config-audit-id"
	mPath      = "body.docs.msgs.0._id"
	mRec, mLiv = "1111aaaa-2222-48d1-9444-444455556666", "9999cccc-7777-4a2e-8888-000011112222"
)

// mForm is the form of mRule's values: a UUID's, as the set's recordings
// give it (no prefix, 36 bytes).
var mForm = models.MintedForm{MinLen: 36, MaxLen: 36}

// mB64 is the form of a key of 12 bytes in standard base64.
var mB64 = models.MintedForm{MinLen: 16, MaxLen: 16}

// mMinted declares path minted by mRule, of the form mForm.
func mMinted(path string) func(string) (Source, bool) {
	return mMintedAs(path, mForm)
}

// mMintedAs declares path minted by mRule, of the form f.
func mMintedAs(path string, f models.MintedForm) func(string) (Source, bool) {
	return func(p string) (Source, bool) { return Source{Kind: SourceMinted, Rule: mRule, Minted: f}, p == path }
}

// mChance is an earlier call of the test whose text holds the id's
// 32-bit group and its 16-bit one inside another id: learning refuses the
// group as unknown (TestOutcomeAPieceOfAFreshIdAtAPlaceShowingNothingOfItsLongWordsIsChance).
func mChance() Earlier {
	return oCarried("mock-46", "body.note", "owner 1111aaaa and bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e", 2*time.Second, time.Second, 1)
}

// mInsert is a recording of an insert into jobEvents recorded before oAt by
// before (after it where before is negative), whose document holds id.
func mInsert(name string, before time.Duration, id string, served bool) Raw {
	at := oAt.Add(-before)
	return RawOf(&models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"insert":"jobEvents","ordered":true,"$db":"api-server"} }`,
			`{ SectionSingle identifier: documents , msgs: [ {"_id":"` + id + `","kind":"AGENT_REQ"} ] }`,
		}}}},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}, served)
}

// mAnsweredAfter is mInsert asked before oAt by before and answered after
// it by after.
func mAnsweredAfter(name string, before, after time.Duration, id string) Raw {
	r := mInsert(name, before, id, false)
	r.answered = oAt.Add(after)
	return r
}

// mFresh checks ds is the field decided made anew whole by mRule, rec and
// live its values, f the form its replay checks.
func mFresh(t *testing.T, what string, ds []Decision, rec, live string, f models.MintedForm) {
	t.Helper()
	if len(ds) != 1 || ds[0].Outcome != OutcomeFresh || ds[0].At.Word != -1 || ds[0].Recorded != rec || ds[0].Full != live {
		t.Errorf("%s: %s; want the field made anew whole", what, oDescribe(ds))
		return
	}
	if m := ds[0].Minted; m == nil || m.Rule != mRule || !reflect.DeepEqual(m.Form, f) {
		t.Errorf("%s: minted %+v; want rule %s, form %+v", what, ds[0].Minted, mRule, f)
	}
	if !strings.Contains(ds[0].Line, mRule) || !strings.Contains(ds[0].Line, "declares it minted") {
		t.Errorf("%s: the line must name the rule: %q", what, ds[0].Line)
	}
}

// mRefused checks ds is one refusal of the field whole as kind, naming
// mRule in its line's tail and each of names.
func mRefused(t *testing.T, what string, ds []Decision, kind RefusalKind, names ...string) {
	t.Helper()
	if len(ds) != 1 || ds[0].At.Word != -1 {
		t.Errorf("FALSE GREEN? %s: %s; want one refusal of the field whole", what, oDescribe(ds))
		return
	}
	oRefused(t, what, ds[0], kind, names...)
	mNamesRule(t, what, ds)
}

// mNamesRule checks every refusal of ds names mRule in its line's tail.
func mNamesRule(t *testing.T, what string, ds []Decision) {
	t.Helper()
	for _, d := range ds {
		if d.Refusal == nil {
			continue
		}
		if r, ok := ParseRefusal(d.Line); d.Refusal.Rule != mRule || !ok || r.Rule != mRule || !strings.Contains(d.Line, `"rule":"`+mRule+`"`) {
			t.Errorf("%s: refusal %q; want its tail to name the rule %s", what, d.Line, mRule)
		}
	}
}

func TestAMintedRuleDecidesANewIdWholeWhereLearningRefusedAPieceOfIt(t *testing.T) {
	in := oInput(mPath, mRec, mLiv, []Earlier{mChance()}, nil)
	before := Decide(in)
	if oWord(t, before, "48d1", "4a2e").Outcome != OutcomeRefused {
		t.Fatalf("with no rule: %s; want the id's group refused", oDescribe(before))
	}
	in.Source = mMinted(mPath)
	mFresh(t, "a piece of the id stands in another id", Decide(in), mRec, mLiv, mForm)

	// No rule of a kind keploy knows, or none for this field: as before.
	for name, src := range map[string]func(string) (Source, bool){
		"a rule of another field": mMinted("body.docs.msgs.0.appId"),
		"a kind keploy does not know": func(string) (Source, bool) {
			return Source{Kind: SourceClock + 1, Rule: mRule}, true
		},
	} {
		in.Source = src
		if got := Decide(in); oDescribe(got) != oDescribe(before) || got[0].Minted != nil {
			t.Errorf("%s: %s; want as with no rule: %s", name, oDescribe(got), oDescribe(before))
		}
	}

	// Every word learned made anew: one value now, its check the rule's.
	in = oInput(mPath, mRec, mLiv, nil, nil)
	if ds := Decide(in); len(ds) < 2 {
		t.Fatalf("with no rule and no earlier place: %s; want each word made anew", oDescribe(ds))
	}
	in.Source = mMinted(mPath)
	mFresh(t, "every word made anew", Decide(in), mRec, mLiv, mForm)

	// A number the application mints.
	num := oInput(mPath, "4821375", "9057713", nil, nil)
	num.Rec.Leaves[mPath], num.Live.Leaves[mPath] = Leaf{Path: mPath, Noise: mPath, Type: Number, Value: "4821375"}, Leaf{Path: mPath, Noise: mPath, Type: Number, Value: "9057713"}
	digits := models.MintedForm{MinLen: 7, MaxLen: 7}
	num.Source = mMintedAs(mPath, digits)
	mFresh(t, "a number", Decide(num), "4821375", "9057713", digits)

	// A prefix is cut to what both runs' values share: one recorded value
	// alone was all prefix (up to its last '-'). A prefix both runs keep
	// stays.
	whole := models.MintedForm{Prefix: mRec[:24], MinLen: 36, MaxLen: 36}
	in = oInput(mPath, mRec, mLiv, nil, nil)
	in.Source = mMintedAs(mPath, whole)
	mFresh(t, "one recorded value's form", Decide(in), mRec, mLiv, mForm)
	const kRec, kLiv = "kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", "kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ"
	kForm := models.MintedForm{Prefix: "kepreq_", MinLen: 50, MaxLen: 50}
	in = oInput(mPath, kRec, kLiv, nil, nil)
	in.Source = mMintedAs(mPath, kForm)
	mFresh(t, "a prefixed id", Decide(in), kRec, kLiv, kForm)
	// A recorded value that lacks the rule's prefix cuts it too: the
	// prefix is no literal part of the values made there.
	in = oInput(mPath, "kepcod"+kRec[6:], kLiv, nil, nil)
	in.Source = mMintedAs(mPath, kForm)
	mFresh(t, "a recorded value with no prefix", Decide(in), "kepcod"+kRec[6:], kLiv, models.MintedForm{MinLen: 50, MaxLen: 50})
	// The lengths widen to both runs' values: a generator of more than one
	// length (the form of the set's recordings, 7 to 9 digits, and round
	// F's of 10).
	num = oInput(mPath, "4821375", "9057713012", nil, nil)
	num.Rec.Leaves[mPath], num.Live.Leaves[mPath] = Leaf{Path: mPath, Noise: mPath, Type: Number, Value: "4821375"}, Leaf{Path: mPath, Noise: mPath, Type: Number, Value: "9057713012"}
	num.Source = mMintedAs(mPath, models.MintedForm{MinLen: 7, MaxLen: 9})
	mFresh(t, "a longer number in round F", Decide(num), "4821375", "9057713012", models.MintedForm{MinLen: 7, MaxLen: 10})
}

// Learning's decision of the whole value comes first: a value read as the
// clock keeps learning's decision, and so does a refusal of what keploy
// could not read (at the field or any word of it) or check whole; each
// refusal names the rule. A piece of the value learning bound to an
// earlier place does not count: the rule decides the value whole.
func TestAMintedRuleLeavesWhatLearningPlacedOrCouldNotRead(t *testing.T) {
	shown := oCarried("mock-48", "body.code", "48d1", "4a2e", 2*time.Second, time.Second, 1)
	in := oInput(mPath, mRec, mLiv, []Earlier{shown}, nil)
	if d := oWord(t, Decide(in), "48d1", "4a2e"); d.Outcome != OutcomeValue || d.From.Mock != "mock-48" {
		t.Errorf("a place shows the id's group in both runs, no rule: %s; want learning's Value bound at mock-48", oDescribe([]Decision{d}))
	}
	in.Source = mMinted(mPath)
	mFresh(t, "a place shows the id's group in both runs", Decide(in), mRec, mLiv, mForm)
	// The whole value bound to an earlier place: learning's Value.
	whole := oInput(mPath, "48d1", "4a2e", []Earlier{shown}, nil)
	whole.Source = mMintedAs(mPath, models.MintedForm{MinLen: 4, MaxLen: 4})
	if ds := Decide(whole); len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "mock-48" || ds[0].Minted != nil {
		t.Errorf("a place shows the whole value in both runs: %s; want learning's Value bound at mock-48", oDescribe(ds))
	}

	const p = "body.updatedAt"
	rAt, fAt := oAt.Add(500*time.Microsecond), oAt.Add(time.Hour+600*time.Microsecond)
	rec, live := iMs(rAt.Add(-200*time.Microsecond)), iMs(fAt.Add(-300*time.Microsecond))
	clock := iCall(p, String, rec, live, rAt, fAt, iWin(rAt, fAt, 800*time.Microsecond), nil, nil)
	clock.Source = mMinted(p)
	iClock(t, "a read of the clock", oWord(t, Decide(clock), rec, live))

	unread := oInput(mPath, mRec, mLiv, []Earlier{{Mock: "mock-7", At: oAt.Add(-time.Second), Own: true, Unread: "its payload was lost"}}, nil)
	unread.Source = mMinted(mPath)
	ds := Decide(unread)
	oRefused(t, "an earlier place keploy could not read", ds[0], RefusalUnread, "mock-7")
	mNamesRule(t, "an earlier place keploy could not read", ds)

	// A recording of the set keploy could not read back: no exchange of the
	// call lacks its payload, so only the minted check keeps the refusal.
	set := oInput(mPath, mRec, mLiv, nil, nil)
	set.Given = func() ([]GivenPlaces, error) { return nil, errors.New("mock-31 is gone") }
	set.Source = mMinted(mPath)
	ds = Decide(set)
	mRefused(t, "a recording of the set keploy could not read back", ds, RefusalUnread)
	if ds[0].Minted != nil || !strings.Contains(ds[0].Line, "mock-31 is gone") {
		t.Errorf("a recording of the set keploy could not read back: %s; want learning's unread refusal kept", oDescribe(ds))
	}

	unnamed := oInput(mPath, mRec, mLiv, nil, nil)
	l := unnamed.Rec.Leaves[mPath]
	l.Unnamed = true
	unnamed.Rec.Leaves[mPath] = l
	unnamed.Source = mMinted(mPath)
	ds = Decide(unnamed)
	oRefused(t, "a field no path names alone", ds[0], RefusalCannotCheck)
	mNamesRule(t, "a field no path names alone", ds)
}

// A minted value is one string or number read as written, and round F's
// keeps the recorded one's shape.
func TestAMintedRuleRefusesAValueOfAnotherKindOrShape(t *testing.T) {
	typed := func(rt, lt Type, r, l string) Input {
		in := oInput(mPath, r, l, nil, nil)
		in.Rec.Leaves[mPath], in.Live.Leaves[mPath] = Leaf{Path: mPath, Noise: mPath, Type: rt, Value: r}, Leaf{Path: mPath, Noise: mPath, Type: lt, Value: l}
		in.Source = mMinted(mPath)
		return in
	}
	mRefused(t, "a number in the recording, a string in round F", Decide(typed(Number, String, "4821375", "9057713")), RefusalNotMinted)
	mRefused(t, "a string in the recording, a number in round F", Decide(typed(String, Number, "4821375", "9057713")), RefusalNotMinted)
	mRefused(t, "a boolean", Decide(typed(Bool, Bool, "true", "false")), RefusalNotMinted)

	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	stepped := oInput(mPath, b64(mRec), b64(mLiv), nil, nil)
	stepped.PlaceOf = func(l Leaf, answer bool) (string, bool) { return l.Path + "|b64", true }
	stepped.Source = mMinted(mPath)
	mRefused(t, "a value a step opens", Decide(stepped), RefusalNotMinted)

	// The form names no character after the prefix: round F's value of
	// any printable characters keeps it, and a length the set's recordings
	// did not show widens it.
	for name, live := range map[string]string{
		"a ':' for a '-'":  "9999cccc:7777-4a2e-8888-000011112222",
		"a '+' for a '-'":  "9999cccc+7777-4a2e-8888-000011112222",
		"a capital":        "9999cccC-7777-4a2e-8888-000011112222",
		"a character more": mLiv + "0",
	} {
		in := oInput(mPath, mRec, live, nil, nil)
		in.Source = mMinted(mPath)
		mFresh(t, name, Decide(in), mRec, live, models.MintedForm{MinLen: 36, MaxLen: len(live)})
	}
	// Whitespace, a character that is not printable, or nothing after the
	// prefix: refused, in either run.
	for name, c := range map[string]struct {
		r, f string
		form models.MintedForm
	}{
		"a space for a '-' in round F":            {mRec, "9999cccc 7777-4a2e-8888-000011112222", mForm},
		"a tab for a '-' in the recording":        {"1111aaaa\t2222-48d1-9444-444455556666", mLiv, mForm},
		"a control byte in round F":               {mRec, "9999cccc\x017777-4a2e-8888-000011112222", mForm},
		"a byte that is no UTF-8 in round F":      {mRec, "9999cccc\xff7777-4a2e-8888-000011112222", mForm},
		"round F's value the rule's prefix alone": {"kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", "kepreq_", models.MintedForm{Prefix: "kepreq_", MinLen: 50, MaxLen: 50}},
	} {
		in := oInput(mPath, c.r, c.f, nil, nil)
		in.Source = mMintedAs(mPath, c.form)
		mRefused(t, name, Decide(in), RefusalChangedShape)
	}
}

// An earlier place that holds the whole value in a run is where the
// application copied it from: refused, naming it. One the recording gave
// the application only after the call, or one round F did not give it,
// is no such place; one round F gave with no view stated may be.
func TestAMintedRuleRefusesAValueAnEarlierPlaceHoldsWhole(t *testing.T) {
	refused := func(what string, earlier []Earlier, raw []Raw, caps Caps, name string) {
		t.Helper()
		in := oInput(mPath, mRec, mLiv, earlier, raw)
		in.Caps, in.Source = caps, mMinted(mPath)
		mRefused(t, what, Decide(in), RefusalNotMinted, name)
	}
	fresh := func(what string, raw []Raw) {
		t.Helper()
		in := oInput(mPath, mRec, mLiv, nil, raw)
		in.Source = mMinted(mPath)
		mFresh(t, what, Decide(in), mRec, mLiv, mForm)
	}
	caps := Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}
	sent := func(r, f string) []Earlier {
		return []Earlier{oCarried("mock-5", "body.ref", r, f, 2*time.Second, time.Second, 1)}
	}
	refused("the recorded value at an earlier send", sent(mRec, "other"), nil, caps, "mock-5")
	refused("round F's value at an earlier send", sent("other", mLiv), nil, caps, "mock-5")
	refused("the recorded value base64-encoded at an earlier send", sent(base64.StdEncoding.EncodeToString([]byte(`{"id":"`+mRec+`"}`)), "other"), nil, caps, "mock-5")
	refused("the recorded value in a recording made before the call", nil, []Raw{mInsert("mock-6", time.Second, mRec, false)}, caps, "mock-6")
	fresh("the recorded value in a recording made after the call", []Raw{mInsert("mock-6", -time.Second, mRec, false)})
	fresh("round F's value in a recording round F did not give", []Raw{mInsert("mock-6", time.Second, mLiv, false)})
	refused("the recorded value sent before the call in a recording answered after it", nil, []Raw{mAnsweredAfter("mock-6", time.Second, time.Second, mRec)}, caps, "mock-6")
	other := "0c1d2e3f-4a5b-4c7d-8e9f-0a2b0b1c2d3e"
	refused("round F's value in one of the views round F served", nil, []Raw{mInsert("mock-6", time.Second, other, true).ServedChanged(mInsert("mock-6", time.Second, mLiv, true), mInsert("mock-6", time.Second, other, true))}, caps, "mock-6")
	refused("a recording round F gave with no view stated", nil, []Raw{mInsert("mock-6", time.Second, "0c1d2e3f-4a5b-4c7d-8e9f-0a2b0b1c2d3e", true)},
		Caps{BindAtWord: true, AnswerAtWord: true}, "mock-6")

	const plus, plusLive = "q3+Zb7Lr0Tf/9XcV", "Xk2N/w9Ft4Qz+Lm8"
	for _, tc := range []struct{ what, r, f string }{
		{"a value of the standard base64 alphabet at an earlier send", plus, plusLive},
	} {
		in := oInput(mPath, tc.r, tc.f, []Earlier{oCarried("mock-5", "body.ref", "key "+tc.r+" end", "other", 2*time.Second, time.Second, 1)}, nil)
		in.Source = mMintedAs(mPath, mB64)
		mRefused(t, tc.what, Decide(in), RefusalNotMinted, "mock-5")
	}
}

// A minted field binds the value wherever it stands among the call's
// fields: another field that sends the same value uses it.
func TestAMintedFieldBindsTheValueTheCallSendsAtAnotherField(t *testing.T) {
	const (
		rec  = "q3+Zb7Lr0Tf/9XcVw2Kd8Hn+Pa4Me6Gs1Jy5Ru0Io7U="
		live = "Xk2Nw9/Ft4Qz+Lm8Hc3Vb6Dp1Sg7Ja0Ey5Ro2Ui9Tn+="
		a, b = "body.docs.msgs.0.access_key", "body.docs.msgs.0.key_copy"
	)
	in := oInput(a, rec, live, nil, nil)
	in.Drift = []string{a, b}
	in.Rec.Leaves[b], in.Live.Leaves[b] = Leaf{Path: b, Noise: b, Type: String, Value: rec}, Leaf{Path: b, Noise: b, Type: String, Value: live}
	ds := Decide(in)
	if len(ds) != 2 || ds[0].Outcome != OutcomeFresh || ds[1].Outcome != OutcomeValue || ds[1].From.Path != a {
		t.Fatalf("with no rule, a key sent at two fields: %s; want the first bound, the second its use", oDescribe(ds))
	}
	in.Source = mMintedAs(b, mB64)
	ds = Decide(in)
	if len(ds) != 2 || ds[0].Outcome != OutcomeValue || ds[0].From.Path != b || ds[1].Outcome != OutcomeFresh || ds[1].Minted == nil {
		t.Errorf("a key sent at two fields, the second minted: %s; want the minted field bound, the other its use", oDescribe(ds))
	}
	in.Source = mMintedAs(a, mB64)
	ds = Decide(in)
	if len(ds) != 2 || ds[0].Outcome != OutcomeFresh || ds[0].Minted == nil || ds[1].Outcome != OutcomeValue || ds[1].From.Path != a {
		t.Errorf("a key sent at two fields, the first minted: %s; want the minted field bound, the other its use", oDescribe(ds))
	}
	in.Source = func(f string) (Source, bool) {
		return Source{Kind: SourceMinted, Rule: mRule, Minted: mB64}, f == a || f == b
	}
	ds = Decide(in)
	if len(ds) != 2 || ds[0].Outcome != OutcomeFresh || ds[0].Minted == nil || ds[1].Outcome != OutcomeValue || ds[1].From.Path != a {
		t.Errorf("a key sent at two fields, both minted: %s; want the first bound, the other its use", oDescribe(ds))
	}
}

// Round F's call that landed in another window learns clock reads only: a
// minted field there is refused, naming the rule.
func TestAMintedFieldOfACallThatLandedElsewhereIsRefusedNamingTheRule(t *testing.T) {
	in := oInput(mPath, mRec, mLiv, nil, nil)
	in.Mode, in.Landed, in.Source = ModeElsewhere, Landed{Where: LandedBetween}, mMinted(mPath)
	ds := Decide(in)
	oRefused(t, "a minted field of a call that landed between tests", ds[0], RefusalLandedElsewhere)
	mNamesRule(t, "a minted field of a call that landed between tests", ds)
}

// TestCITrustIsNarrowedBoundedAndExpires's insert on app_config_audits
// mints a v4 UUID _id; a 16-bit group of it stood, in both runs, in an
// earlier answer's id list, so learning bound the group there, and the
// Mongo learn step, which names no place of that answer to bind it at,
// refused it as keploy's limit: the call was set aside though a rule
// declared the id minted. Under the rule the id's words are not decided
// one by one: the rule decides the id made anew whole.
func TestAMintedRuleDecidesAnIdWholeWhoseGroupAnEarlierAnswerShows(t *testing.T) {
	const (
		rec, live = "294e693b-0963-42b8-a8ba-8cdf7fb6d3f1", "22ee59fa-7c10-412d-b6e2-90ad3f5c7e18"
		other     = "ab4e1a5f-ccca-48d0-8fb3-144c278387a7"
	)
	list := func(session string, ids ...string) *models.Mock {
		rows := []models.PostgresV3Cells{{{Value: session}}}
		for _, id := range ids {
			rows = append(rows, models.PostgresV3Cells{{Value: id}})
		}
		return oPgMock("mock-47", 97*time.Second, "SELECT id FROM suites", rows, "SELECT 3")
	}
	// One listed id holds the recorded group in the recording and round
	// F's in round F's view, at one spot.
	raw := RawOf(list("s-1", "d4927bdf-978e-42b8-86de-eeb6c761151c", other), true).ServedAs(1).
		ServedLive(RawOf(list("s-2", "d4927bdf-978e-412d-86de-eeb6c761151c", other), true))
	in := oInput(mPath, rec, live, nil, []Raw{raw})
	if d := oWord(t, Decide(in), "42b8", "412d"); d.Outcome != OutcomeValue || d.From.Mock != "mock-47" {
		t.Fatalf("with no rule: %s; want the group bound to mock-47", oDescribe([]Decision{d}))
	}
	in.Source = mMinted(mPath)
	mFresh(t, "a group an earlier answer shows", Decide(in), rec, live, mForm)
}

// Two ObjectIDs one process makes share its 5 random bytes (and their
// counters run on): an earlier send of one, in each run, leaves the
// other's whole value unheld, and the rule decides it made anew whole.
func TestAMintedRuleDecidesAnObjectIdWholeBesideAnotherOfItsProcess(t *testing.T) {
	const (
		rec, live   = "6730a1c2e4b0d1f2a3c4d5e7", "6730a2f0b7c8e9d0f1a2b3c5"
		recA, liveA = "6730a1c1e4b0d1f2a3c4d5e6", "6730a2efb7c8e9d0f1a2b3c4"
	)
	sent := oCarried("mock-5", "body.ref", recA, liveA, 2*time.Second, time.Second, 1)
	in := oInput(mPath, rec, live, []Earlier{sent}, nil)
	in.Source = mMintedAs(mPath, models.MintedForm{MinLen: 24, MaxLen: 24})
	mFresh(t, "an ObjectID beside another of its process", Decide(in), rec, live, models.MintedForm{MinLen: 24, MaxLen: 24})
}
