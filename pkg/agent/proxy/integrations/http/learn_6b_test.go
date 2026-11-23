package http

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// startUpValueRefused sends test number tests of the running set a call that
// carries, drifted, the client id the set's start-up call (startUp) sent, and
// reports why it was not refused ("" when it was). The start-up call is a
// data-plane mock of the pool, which the exact view hides while a test is
// open: only the tracker keeps it as an earlier place, and the fake's windows
// put it outside every recorded test, as the pool's.
func startUpValueRefused(t *testing.T, tr *learn.Tracker, startUp *models.Mock, tests int) string {
	t.Helper()
	h, n, opts := learnOn(t)
	ctx := learn.WithTracker(context.Background(), tr)
	own := learnRecorded("mock-1", time.Duration(tests)*time.Second, "GET", "/v1/items", "", map[string]string{"X-Client-Id": "cid-7f3a9e21"}, `{"items":[]}`)
	db := &learnDb{perTest: []*models.Mock{own}, pool: map[time.Time]bool{startUp.Spec.ReqTimestampMock: true}}
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/items", "", map[string]string{"X-Client-Id": "cid-0b5d4c88"}), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served == nil && miss != nil && miss.res.Reason == models.MissRequestChanged {
		return ""
	}
	var noise map[string][]string
	if len(db.consumed) > 0 {
		noise = db.consumed[0].Spec.ReqBodyNoise
	}
	return fmt.Sprintf("served %v, miss %+v, noise %v", served != nil, miss, noise)
}

// beginTests begins tests tests of a set, ending the one before each.
func beginTests(tr *learn.Tracker, tests int) {
	for i := 0; i < tests; i++ {
		if i > 0 {
			tr.End()
		}
		tr.Begin()
	}
}

// A value the app minted at start-up and sent in a start-up call (a client
// id), then again in a test's call, flowed from a call of the pool: it is
// refused in every test, never learned as noise in the second test on.
func TestLearnMatchAStartUpValueIsRefusedInEveryTest(t *testing.T) {
	for _, tests := range []int{1, 2, 3} {
		startUp := learnRecorded("mock-s", -time.Minute, "POST", "/v1/register", `{"client":"cid-7f3a9e21"}`, nil, `{"ok":true}`)
		tr := learn.NewTracker()
		tr.ServedMock(startUp) // served at start-up: no test open
		beginTests(tr, tests)
		if why := startUpValueRefused(t, tr, startUp, tests); why != "" {
			t.Errorf("test %d: the start-up client id was not refused: %s", tests, why)
		}
	}
}

// The same in the second test set an agent replays (round-2 review of the
// fix): its start-up mock reuses the first set's names ("mock-0" again), and
// is still an earlier place of each of its tests, whether the replay began
// the set (NewSet, at Proxy.Mock) or not (each mock is kept by its identity,
// not its name).
func TestLearnMatchAStartUpValueOfTheSecondSetIsRefusedInEveryTest(t *testing.T) {
	for _, newSet := range []bool{true, false} {
		for _, tests := range []int{1, 2} {
			tr := learn.NewTracker()
			// Set 1: its start-up call, then one test.
			tr.ServedMock(learnRecorded("mock-0", -2*time.Hour, "POST", "/v1/register", `{"client":"cid-11112222"}`, nil, `{"ok":true}`))
			tr.Begin()
			tr.End()
			if newSet {
				tr.NewSet()
			}
			// Set 2: its start-up call, recorded under the same name.
			startUp := learnRecorded("mock-0", -time.Minute, "POST", "/v1/register", `{"client":"cid-7f3a9e21"}`, nil, `{"ok":true}`)
			tr.ServedMock(startUp)
			beginTests(tr, tests)
			if why := startUpValueRefused(t, tr, startUp, tests); why != "" {
				t.Errorf("NewSet %v, set 2, test %d: set 2's start-up client id was not refused: %s", newSet, tests, why)
			}
		}
	}
}

// A set replayed against an app kept alive from the previous one: the
// previous set's last test sent a value of the run (a client id minted at the
// app's start), and the next set's start-up call carries it again, recorded
// as the previous set's call was. The previous set's exchange, under a mock
// name the next set numbers afresh, is no earlier place of the next set's: the
// start-up call is paired as it would be with nothing served before it, never
// refused as a use of the previous set's mock-3.
func TestLearnMatchTheNextSetsStartUpIsNotExplainedByThePreviousSet(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.NewSet()
	tr.Begin() // set 1's last test
	ctx := learn.WithTracker(context.Background(), tr)
	last := learnRecorded("mock-3", time.Second, "POST", "/v1/items", `{"client":"cid-7f3a9e21"}`, nil, `{"ok":true}`)
	db := &learnDb{perTest: []*models.Mock{last}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/items", `{"client":"cid-0b5d4c88"}`, nil), db, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("set 1's last test's call is paired: served %v, miss %+v, err %v", served != nil, miss, err)
	}
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Mock != "mock-3" {
		t.Fatalf("set 1's last test does not keep its call: %+v", earlier)
	}
	tr.End()
	tr.NewSet()
	opts.LearnPool = true
	boot := learnRecorded("mock-0", -time.Minute, "POST", "/v1/register", `{"client":"cid-7f3a9e21"}`, nil, `{"ok":true}`)
	boot.TestModeInfo.Lifetime = models.LifetimeSession
	bdb := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{boot}}, pool: map[time.Time]bool{boot.Spec.ReqTimestampMock: true}, observe: tr.ServedMock}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", `{"client":"cid-0b5d4c88"}`, nil), bdb, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served == nil || miss != nil {
		why := ""
		if miss != nil {
			why = miss.res.Why
		}
		t.Fatalf("set 2's start-up call is not paired (%q): the previous set's mock-3 explained its client id", why)
	}
}

// A set replayed against an app kept alive from the previous set (docker
// compose reuses the stack across sets; --keep-app-alive) never makes its
// start-up call again: its recorded start-up mock is never served, and the
// exact view hides it in a test. It is still an earlier place, read from what
// the store stages (round-2 review of the fix): the value that flowed from it
// when recorded is refused, never learned as noise.
func TestLearnMatchAnUnservedStartUpRecordingIsAnEarlierPlace(t *testing.T) {
	for _, tests := range []int{1, 2} {
		h, n, opts := learnOn(t)
		startUp := learnRecorded("mock-0", -time.Minute, "POST", "/v1/register", `{"client":"cid-7f3a9e21"}`, nil, `{"ok":true}`)
		tr := learn.NewTracker()
		tr.NewSet() // the set's Mock: nothing served at start-up, the app is warm
		beginTests(tr, tests)
		ctx := learn.WithTracker(context.Background(), tr)
		own := learnRecorded("mock-1", time.Duration(tests)*time.Second, "GET", "/v1/items", "", map[string]string{"X-Client-Id": "cid-7f3a9e21"}, `{"items":[]}`)
		db := &learnDb{perTest: []*models.Mock{own}, pool: map[time.Time]bool{startUp.Spec.ReqTimestampMock: true}, staged: []*models.Mock{startUp, own}}
		served, miss, err := h.learnMatch(ctx, live("GET", "/v1/items", "", map[string]string{"X-Client-Id": "cid-0b5d4c88"}), db, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
			t.Errorf("test %d: the recorded start-up client id was not refused: served %v, miss %+v", tests, served != nil, miss)
		}
	}
}

// startUpValueCall is a test's call, in the set's first test or its second
// (tests), that carries a client id the set's start-up call sent, which the
// learn step itself paired with its drifted live value, with the proven
// switch set to sw: what learnMatch served and missed, and its recordings.
func startUpValueCall(t *testing.T, tests int, sw string) (*models.Mock, *learnMiss, *learnDb) {
	t.Helper()
	h, n, opts := learnOn(t)
	t.Setenv(provenswitch.Env, sw)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	boot := learnRecorded("mock-s", -time.Minute, "POST", "/v1/register", `{"client":"cid-7f3a9e21"}`, nil, `{"ok":true}`)
	boot.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{boot}}, pool: map[time.Time]bool{boot.Spec.ReqTimestampMock: true}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/register", `{"client":"cid-0b5d4c88"}`, nil), db, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("the drifted start-up call is paired: served %v, miss %+v, err %v", served != nil, miss, err)
	}
	beginTests(tr, tests)
	own := learnRecorded("mock-1", time.Duration(tests)*time.Second, "GET", "/v1/items", "", map[string]string{"X-Client-Id": "cid-7f3a9e21"}, `{"items":[]}`)
	tdb := &learnDb{perTest: []*models.Mock{own}, pool: map[time.Time]bool{boot.Spec.ReqTimestampMock: true}}
	served, miss, err := h.learnMatch(ctx, live("GET", "/v1/items", "", map[string]string{"X-Client-Id": "cid-0b5d4c88"}), tdb, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	return served, miss, tdb
}

// A start-up call the learn step itself paired, carrying a value the run made
// (a client id minted at start-up), is the run's exchange (learn.Earlier.Run):
// an earlier place, with its live value, of every test of the set. A test's
// call that carries that value uses the run's variable, bound at the
// start-up's call, in its first test and in later ones, by one name: it is
// neither learned as noise nor refused, so no test is set aside over it.
func TestLearnMatchADriftedStartUpValueIsTheRunsVariableInEveryTest(t *testing.T) {
	for _, tests := range []int{1, 2} {
		served, miss, tdb := startUpValueCall(t, tests, "1")
		if served == nil || miss != nil {
			t.Fatalf("test %d: the start-up client id is not followed: served %v, miss %+v", tests, served != nil, miss)
		}
		name := models.RunVariablePrefix + "client"
		use, bind := tdb.consumed[0].Spec.Learned(), tdb.consumed[0].Spec.LearnedBinds()["mock-s"]
		if len(use) != 1 || len(use[name].Use) != 1 || len(bind) != 1 || bind[name].Bind != "body.client" {
			t.Errorf("test %d: the call uses %+v and binds %+v at the start-up, want the run's %s", tests, use, bind, name)
		}
		if len(tdb.consumed[0].Spec.ReqBodyNoise) != 0 {
			t.Errorf("test %d: the client id was learned as noise too: %v", tests, tdb.consumed[0].Spec.ReqBodyNoise)
		}
	}
}

// Without the proven switch a test's call that carries the start-up's value
// is refused there, in its first test and in later ones, as it was before the
// run's variables: only a test's own calls carry its variables. It is never
// learned as noise.
func TestLearnMatchWithoutTheSwitchADriftedStartUpValueIsRefusedInEveryTest(t *testing.T) {
	for _, tests := range []int{1, 2} {
		served, miss, tdb := startUpValueCall(t, tests, "0")
		if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "only a test's own calls carry its variables") {
			t.Errorf("test %d: the start-up client id was not refused as the run's: served %v, miss %+v", tests, served != nil, miss)
		}
		if len(tdb.consumed) != 0 || len(tdb.perTest) != 1 {
			t.Errorf("test %d: the refused call consumed its recording", tests)
		}
	}
}
