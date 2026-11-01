package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows in the shapes api-server's e2e suite records (keploy mock's client
// tests against Mongo; API-SERVER-TARGETS.md rows 18, 22, 25-27, 33, 40),
// at learn level: what the one entry decides once an adapter calls it.

// apiCall is a client test's call at path, recorded rec at oAt and seen in
// round F as live, arriving at fAt, in the call's window w.
func apiCall(path, rec, live string, fAt time.Time, w *Window, earlier []Earlier, raw []Raw) Input {
	in := iCall(path, String, rec, live, oAt, fAt, w, earlier, raw)
	in.Drifts = oClientDrifts()
	return in
}

// apiAnswer is another test's find answering one document whose _id is id,
// recorded before before oAt and answered a millisecond later, served by F
// as the n-th serve of the set, in an older test.
func apiAnswer(name string, before time.Duration, id string, n int) Raw {
	return oPg(name, before, "SELECT _id FROM app_configs", []models.PostgresV3Cells{{{Value: id}}}, "SELECT 1").ServedAs(n).ServedInAnOlderTest()
}

// A fresh UUID's 4-hex word ("48d1") stands at another test's answer, given
// before the call in both runs (rows 22, 25, 33, 40: insert app_config_
// audits' _id beside mock-11210). With F's view of that answer read (it
// gave its own fresh id there, which shares nothing with F's new word):
// chance, so the id is fresh. With F's view not stated: keploy cannot tell
// what F gave there, so the word is unknown, refused naming the answer.
func TestOutcomeAPIAFreshUUIDsShortWordAtAnotherTestsAnswer(t *testing.T) {
	const p = "body.documents[0]._id"
	recID, liveID := "1111aaaa-2222-3333-48d1-444455556666", "9999cccc-7777-8888-dddd-000011112222"
	otherRec, otherLive := "bb7ed9ee-670e-48d1-982f-b54b2b73982f", "0c1d2e3f-4a5b-6c7d-8e9f-a0b1c2d3e4f5"
	fAt := oAt.Add(FullRoundLead)
	read := apiAnswer("mock-11210", 2*time.Second, otherRec, 40).ServedLive(oPg("mock-11210", 2*time.Second, "SELECT _id FROM app_configs", []models.PostgresV3Cells{{{Value: otherLive}}}, "SELECT 1"))
	d := oWord(t, Decide(apiCall(p, recID, liveID, fAt, nil, nil, []Raw{read})), "48d1", "dddd")
	costFresh(t, "a UUID word beside another test's answer, F's view read", d)
	whole := Decide(apiCall(p, recID, liveID, fAt, nil, nil, []Raw{read}))
	for _, d := range whole {
		if d.Outcome != OutcomeFresh {
			t.Errorf("FALSE RED? a fresh UUID beside another test's answer, F's view read: %s; want every word fresh", oDescribe([]Decision{d}))
		}
	}
	unstated := apiAnswer("mock-11210", 2*time.Second, otherRec, 40)
	d = oWord(t, Decide(apiCall(p, recID, liveID, fAt, nil, nil, []Raw{unstated})), "48d1", "dddd")
	costRefused(t, "a UUID word beside another test's answer, F's view not stated", d, costUnknown, "mock-11210")
}

// An own insert's ObjectId (insert oauth_authorization_requests' _id, row
// 18) beside a start-up recording F never served whose flags hold one
// character of it: no place holds the id's word, so it is the test's own
// value, fresh at its first send.
func TestOutcomeAPIAnOwnInsertsIdBesideAOneCharacterStartUpPlaceIsFresh(t *testing.T) {
	const p = "body.documents[0]._id"
	recID, liveID := "6720a1b2c3d4e5f6a7b8c9d0", "6721f0e1d2c3b4a596877869"
	mock0 := oPg("mock-0", 5*time.Second, "SELECT flags FROM hello", []models.PostgresV3Cells{{{Value: "6"}}}, "SELECT 1")
	d := oWord(t, Decide(apiCall(p, recID, liveID, oAt.Add(FullRoundLead), nil, nil, []Raw{mock0})), recID, liveID)
	costFresh(t, "an own insert's id beside a 1-character start-up place", d)
}

// apiFlags is a start-up recording holding v at a field of its own, and the
// set's reading of it as Order.GivenPlaces gives it: served by F where served
// is above zero, at that serve, and told of nothing.
func apiFlags(t *testing.T, v string, served int) (Raw, []GivenPlaces) {
	t.Helper()
	m := oPgMock("mock-0", 5*time.Second, "SELECT flags FROM settings", []models.PostgresV3Cells{{{Value: v}}}, "SELECT 1")
	tr := NewTracker()
	tr.Begin()
	x := RawOf(m, false)
	if served > 0 {
		tr.ServedMock(m)
		x = x.ServedAs(served)
	}
	g, err := tr.Placed().Order.GivenPlaces(m)
	if err != nil {
		t.Fatal(err)
	}
	return x, []GivenPlaces{g}
}

// Row 18 at its real shape: an own insert's _id, a base64url text whose first
// word is one character, beside a start-up recording F never served whose
// flags hold that character at a field of their own. The id's other word
// carries its bits, so the short word is a piece of it, and a recording F
// never served says nothing of a piece that short: fresh, its line stating
// the floor and naming the place. From the floor up the same place stays
// refused, wherever it stands: handed to the call or in the set.
func TestOutcomeAPIAPieceOfAnIdUnderTheFloorAtAPlaceFNeverServedIsNoEvidence(t *testing.T) {
	const p = "body.documents[0]._id"
	fAt := oAt.Add(FullRoundLead)
	for _, tc := range []struct {
		what, r, f string
		bits       string
		fresh      bool
	}{
		{"one letter", "k", "z", "4.7", true},
		{"two letters and digits", "k7", "z4", "10.3", true},
		{"three digits", "417", "902", "10.0", true},
		{"three hex digits", "a1f", "c3e", "12", false},
		{"three letters", "kqz", "pwy", "14.1", false},
		{"a UUID's 4-hex word", "a1f3", "c2e4", "16", false},
	} {
		rec, live := tc.r+"-Rq7Lm2Xv9Tp4Wn8Zc", tc.f+"-Hy3Jd6Fs1Gq5Kw0Pe"
		x, set := apiFlags(t, tc.r, 0)
		given := apiCall(p, rec, live, fAt, nil, nil, nil)
		given.Given = func() ([]GivenPlaces, error) { return set, nil }
		for name, in := range map[string]Input{
			"handed to the call":         apiCall(p, rec, live, fAt, nil, nil, []Raw{x}),
			"among the set's recordings": given,
		} {
			d := oWord(t, Decide(in), tc.r, tc.f)
			what := tc.what + " (" + tc.bits + " bits) " + name
			if !tc.fresh {
				costRefused(t, what, d, costUnknown, "mock-0")
				continue
			}
			costFresh(t, what, d)
			if !strings.Contains(d.Line, "under 11 bits") || !strings.Contains(d.Line, "mock-0") {
				t.Errorf("%s: the line %q does not state the floor and name the place", what, d.Line)
			}
		}
	}
}

// The floor is that one shape, no wider. A short word that is the field's
// value, or changes beside words that stay, may be the very value a recording
// gave ("50", "user-12"), and one round F served with no view may have given
// F's word too: refused at either, any size, handed to the call or in the
// set, naming the place.
func TestOutcomeAPIAShortValueStaysRefusedAtAPlaceKeployCannotReadFsSideOf(t *testing.T) {
	const p = "body.ref"
	fAt := oAt.Add(FullRoundLead)
	at := func(rec, live, v string, served int) map[string]Input {
		x, set := apiFlags(t, v, served)
		given := apiCall(p, rec, live, fAt, nil, nil, nil)
		given.Given = func() ([]GivenPlaces, error) { return set, nil }
		return map[string]Input{"handed to the call": apiCall(p, rec, live, fAt, nil, nil, []Raw{x}), "among the set's recordings": given}
	}
	for _, tc := range []struct{ what, rec, live, r, f string }{
		{"the field's whole value", "50", "7", "50", "7"},
		{"beside a word that stays", "user-12", "user-77", "12", "77"},
		{"beside a short word that changed", "a-12", "b-12", "a", "b"},
	} {
		for name, in := range at(tc.rec, tc.live, tc.r, 0) {
			costRefused(t, tc.what+", F never served it, "+name, oWord(t, Decide(in), tc.r, tc.f), costUnknown, "mock-0")
		}
		for name, in := range at(tc.rec, tc.live, tc.r, 1) {
			costRefused(t, tc.what+", F served it with no view, "+name, oWord(t, Decide(in), tc.r, tc.f), costUnknown, "mock-0")
		}
	}
	for name, in := range at("k-Rq7Lm2Xv9Tp4Wn8Zc", "z-Hy3Jd6Fs1Gq5Kw0Pe", "k", 1) {
		costRefused(t, "a piece of an id, F served it with no view, "+name, oWord(t, Decide(in), "k", "z"), costUnknown, "mock-0")
	}
}

// A word under the floor is no evidence only at a place F never served.
// Where keploy reads F's side, a place that holds it still says what it says.
func TestOutcomeAPIAWordUnderTheFloorAtAPlaceKeployReadsStillCounts(t *testing.T) {
	const p = "body.documents[0]._id"
	const rec, live = "k-Rq7Lm2Xv9Tp4Wn8Zc", "z-Hy3Jd6Fs1Gq5Kw0Pe"
	fAt := oAt.Add(FullRoundLead)
	both := oAnswer("mock-5", time.Second, "k").ServedAs(1).ServedLive(oAnswer("mock-5", time.Second, "z"))
	if d := oWord(t, Decide(apiCall(p, rec, live, fAt, nil, nil, []Raw{both})), "k", "z"); d.Outcome != OutcomeValue {
		t.Errorf("a place that shows the word in both runs: %s; want bound there", oDescribe([]Decision{d}))
	}
	onlyF := oAnswer("mock-5", time.Second, "q").ServedAs(1).ServedLive(oAnswer("mock-5", time.Second, "z"))
	costRefused(t, "a place only F's run took the word at", oWord(t, Decide(apiCall(p, rec, live, fAt, nil, nil, []Raw{onlyF})), "k", "z"), costUnknown, "mock-5")
}

// temp_login_codes' authTime (rows 26, 27: insert temp_login_codes,
// findAndModify oauth_device_codes) equals an earlier test's read of the
// clock in the same second (mock-200's lastUsedAt), in both runs. That
// place is another test's, out of the call's scope: the call's own window
// holds the second, so it is a Clock, never bound to the other test.
func TestOutcomeAPIAnotherTestsSameSecondReadIsNoSource(t *testing.T) {
	const p = "body.documents[0].authTime"
	secs := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
	read := oAt.Add(-20 * time.Millisecond)
	fAt := oAt.Add(FullRoundLead)
	fRead := fAt.Add(-20 * time.Millisecond)
	other := Earlier{Mock: "mock-200", At: oAt.Add(-300 * time.Millisecond), Run: true, Seen: []Seen{{Side: Request,
		Leaf: oText("body.u.$set.lastUsedAt", secs(read)), Live: secs(fRead), Place: "body.u.$set.lastUsedAt"}}}
	d := oWord(t, Decide(apiCall(p, secs(read), secs(fRead), fAt, iWin(oAt, fAt, 100*time.Millisecond), []Earlier{other}, nil)), secs(read), secs(fRead))
	iClock(t, "authTime beside another test's same-second lastUsedAt", d)
	if d.From.Mock != "" {
		t.Errorf("FALSE GREEN? authTime bound to %s, another test's place", d.From.Mock)
	}
}
