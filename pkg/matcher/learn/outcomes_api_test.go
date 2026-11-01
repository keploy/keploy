package learn

import (
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
