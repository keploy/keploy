package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// asideNames is the names of the mocks in mks that are set-aside recordings,
// each with its test.
func asideNames(mks []*models.Mock) []string {
	var out []string
	for _, mk := range mks {
		if mk.TestModeInfo.SetAsideFrom != "" {
			out = append(out, mk.Name+"<"+mk.TestModeInfo.SetAsideFrom)
		}
	}
	return out
}

// A set-aside test's recordings are offered to every test view installed
// after it, in no read but SetAsideMocks, by kind, marked with their test;
// none is required of the test, carried into it, or counted as its own
// consume; one that answered is named for the test it answered in, and is
// offered to no later test; the round's snapshot holds it as no test's own
// (never learned against).
func TestASetAsideRecordingIsServedAfterItsTestUnchecked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := exactMock("own", 0, models.LifetimeSession)
	aside := exactMock("aside", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, aside})
	mm.ServeExactAside("Q", []*models.Mock{aside})
	mm.ServeExactTest("T", []*models.Mock{own})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test: a set-aside recording is in no read", pt, "own")
	sa := mm.SetAsideMocks(models.HTTP)
	if got := asideNames(sa); !slices.Equal(got, []string{"aside<Q"}) {
		t.Fatalf("set-aside copies %v", got)
	}
	if len(mm.SetAsideMocks(models.Mongo)) != 0 {
		t.Fatal("SetAsideMocks gave a recording of another kind")
	}
	snap, _ := mm.RoundSnapshot(integrations.RoundQuery{})
	for _, mk := range sa {
		if mk.Name == "aside" {
			if snap.Owner(mk) != integrations.OwnerSetAside || snap.OwnedByOpenTest(mk) {
				t.Fatalf("the round holds a set-aside recording as %v", snap.Owner(mk))
			}
			if !mm.MarkMockAsUsed(*mk) {
				t.Fatal("the set-aside recording did not answer the call that is it")
			}
		}
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || len(consumed) != 0 || !slices.Equal(unconsumed, []string{"own"}) {
		t.Fatalf("outcome %q consumed %v unconsumed %v: only the test's own count", test, consumed, unconsumed)
	}
	if st, served, ok := mm.ExactTestServedAside(); !ok || st != "T" || !slices.Equal(served, []string{"aside served from Q (set aside)"}) {
		t.Fatalf("served aside %q %v %v", st, served, ok)
	}
	if len(mm.SetAsideMocks("")) != 0 {
		t.Fatal("a set-aside recording that answered is still offered")
	}
	// Answered once: the next test is not offered it.
	mm.ServeExactTest("U", nil)
	if got := mm.SetAsideMocks(""); len(got) != 0 {
		t.Fatalf("a set-aside recording another test answered with is offered again: %v", asideNames(got))
	}
	if _, served, _ := mm.ExactTestServedAside(); len(served) != 0 {
		t.Fatalf("a test that was served nothing aside names %v", served)
	}
}

// What is not served aside: a recording that places or binds variables (its
// values are its own test's), one the open test owns (its own, required), and
// anything after a session reset.
func TestWhatASetAsideTestDoesNotServe(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	vars := exactMock("vars", 0, models.LifetimeSession)
	vars.Spec.Variables = models.MockVariables{"v": models.MockVariable{Recorded: "kiwi", Bind: "body.item"}}
	shared := exactMock("shared", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{vars, shared})
	mm.ServeExactAside("Q", []*models.Mock{vars, shared})
	mm.ServeExactTest("T", []*models.Mock{shared})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "shared")
	if got := asideNames(append(pt, mm.SetAsideMocks("")...)); len(got) != 0 {
		t.Fatalf("set-aside copies %v", got)
	}
	if _, _, unconsumed, _ := mm.ExactTestOutcome(); !slices.Equal(unconsumed, []string{"shared"}) {
		t.Fatalf("the test's own recording is required whoever else recorded it: %v", unconsumed)
	}
	mm.ResetForReplaySession()
	if got := mm.setAsideRecordings(); len(got) != 0 {
		t.Fatalf("a session reset kept %d set-aside recordings", len(got))
	}
}

// A set-aside recording whose request holds a read of the application's
// clock the learn step learned (MockSpec.Clocks) is offered to the tests
// after it, unlike one that places or binds variables: a clock read's value
// comes from the clock at the call, not from its own test's other calls, so
// the parser checks it against the later call's own window, as it does in
// its own test (the HTTP parser's row:
// TestASetAsideRecordingsClockReadIsCheckedInTheLaterCallsWindow). Not
// offering it would leave the later test's call unanswered (a false red);
// serving it unchecked would be a false green.
func TestASetAsideRecordingWithALearnedClockReadIsOffered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clock := exactMock("clock", 0, models.LifetimeSession)
	clock.Spec.Clocks = map[string]models.ClockOffset{"body.since": {Res: time.Second}}
	mm := stagedManager(t, nil, []*models.Mock{clock})
	mm.ServeExactAside("Q", []*models.Mock{clock})
	mm.ServeExactTest("T", nil)
	sa := mm.SetAsideMocks(models.HTTP)
	if got := asideNames(sa); !slices.Equal(got, []string{"clock<Q"}) {
		t.Fatalf("set-aside copies %v; want the recording with a learned clock read offered", got)
	}
	if len(sa[0].Spec.Clocks) != 1 {
		t.Fatalf("the offered copy lost its learned clock read: %v", sa[0].Spec.Clocks)
	}
}
