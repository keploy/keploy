package http

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
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
