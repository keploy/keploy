package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A call read in a pool view and offered the next test's recording in
// flight (the pool view's ahead) is answered by that recording whether its
// consume lands before the test begins or after (heldScope.settled). After,
// the test's view refuses the copy the pool view served (a copy another view
// served answers for nothing there), while the pool view still offers it,
// unanswered: the call reads on in the view in force, where the test's own
// copy answers it. Read on in the pool view, it would be offered the copy
// the view in force refused, and refused again, without end.
func TestAPoolCallRefusedTheNextTestsCopyIsAnsweredInTheViewInForce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, late := range []bool{false, true} {
		when := "at once"
		if late {
			when = "late"
		}
		t.Run(when, func(t *testing.T) {
			a := exactMock("a-0", time.Second, models.LifetimePerTest)
			a.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "/a"}
			mm := stagedManager(t, []*models.Mock{a}, nil)
			mm.ServeExactTest("T1", nil)
			mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, a.Name))
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			first := func(db integrations.MockMemDb) *models.Mock {
				for _, read := range []func() ([]*models.Mock, error){db.GetFilteredMocks, db.GetPerTestMocksInWindow} {
					got, _ := read()
					for _, mk := range got {
						if strings.HasPrefix(mk.Name, "a-") {
							return mk
						}
					}
				}
				return nil
			}
			offered := first(held)
			if offered == nil {
				t.Fatal("precondition: the pool view's call was not offered the next test's recording in flight")
			}
			if late {
				mm.ServeExactTest("T2", []*models.Mock{roundMock(mm, a.Name)})
			}
			got := ""
			ok := held.DeleteFilteredMock(*offered)
			if ok == late {
				t.Fatalf("precondition: the consume of the copy the pool view served settled %v; want %v (late: T2's view refuses it)", ok, !late)
			}
			if ok {
				got = offered.Name
			}
			for i := 0; i < 10 && got == ""; i++ {
				mk := first(held)
				if mk == nil {
					got = "a miss"
				} else if held.DeleteFilteredMock(*mk) {
					got = mk.Name
				}
			}
			if got != "a-0" {
				t.Errorf("the pool view's call was answered %q; want a-0, as a call whose consume lands before T2 begins (\"\": offered a copy it was refused, again and again)", got)
			}
		})
	}
}

// A call read in a pool view that another of the pool view's calls beats to
// the next test's recording in flight misses whether its consume lands
// before that test begins or after (heldScope.settled). After, the test's
// view refuses the copy the pool view served, and the pool view no longer
// offers the recording either (the other call took it there): the call
// reads on in the pool view, and misses there, as at once. Read on in the
// view in force, it would be answered by that test's other recording of the
// same request, one the test made, not the pool view.
func TestAPoolCallBeatenToTheNextTestsCopyMissesAsAtOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, late := range []bool{false, true} {
		when := "at once"
		if late {
			when = "late"
		}
		t.Run(when, func(t *testing.T) {
			var own []*models.Mock
			for i, name := range []string{"a-0", "a-1"} {
				mk := exactMock(name, time.Duration(i+1)*time.Second, models.LifetimePerTest)
				mk.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "/a"}
				own = append(own, mk)
			}
			mm := stagedManager(t, own, nil)
			mm.ServeExactTest("T1", nil)
			mm.ServeExactPool(models.PoolKeyNone, nil, roundMock(mm, "a-0"))
			other, _ := mm.HoldToRound(mm.MarkCallRound())
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			first := func(db integrations.MockMemDb) *models.Mock {
				for _, read := range []func() ([]*models.Mock, error){db.GetFilteredMocks, db.GetPerTestMocksInWindow} {
					got, _ := read()
					for _, mk := range got {
						if strings.HasPrefix(mk.Name, "a-") {
							return mk
						}
					}
				}
				return nil
			}
			offered := first(held)
			if offered == nil || offered.Name != "a-0" {
				t.Fatalf("precondition: the pool view's call was offered %v; want the next test's a-0 in flight", offered)
			}
			if taken := first(other); taken == nil || !other.DeleteFilteredMock(*taken) {
				t.Fatal("precondition: the pool view's other call was not answered a-0")
			}
			if late {
				mm.ServeExactTest("T2", []*models.Mock{roundMock(mm, "a-0"), roundMock(mm, "a-1")})
			}
			if held.DeleteFilteredMock(*offered) {
				t.Fatal("precondition: the call was answered a-0 after the pool view's other call took it")
			}
			got := ""
			for i := 0; i < 10 && got == ""; i++ {
				mk := first(held)
				if mk == nil {
					got = "a miss"
				} else if held.DeleteFilteredMock(*mk) {
					got = mk.Name
				}
			}
			if got != "a miss" {
				t.Errorf("the pool view's call was answered %q; want a miss, as a call whose consume lands before T2 begins: the pool view's other call took a-0", got)
			}
		})
	}
}

// A refused update that names no recording it replaces (UpdateUnFilteredMock
// with a nil old) keeps a late window call reading in its window: no read of
// its own view offers a recording it does not name, so nothing would be
// offered and refused again (heldScope.settled, exactView.offers).
func TestALateWindowCallRefusedAnUpdateOfNoRecordingReadsOnInItsWindow(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	names := []string{"w-0", "w-1"}
	w0 := httpCall("w-0", time.Second, "/poll")
	mm := stagedManager(t, nil, []*models.Mock{w0, httpCall("w-1", 2*time.Second, "/poll")})
	mm.ServeExactWorker(names)
	mm.ServeExactTest("TestA", nil)
	held, _ := mm.HoldToRound(mm.MarkCallRound())
	if got := pollFirst(t, mm, "w-"); got != "w-0" {
		t.Fatalf("precondition: another of TestA's calls was answered %s; want w-0", got)
	}
	mm.ServeExactWorker(names)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	served := *w0
	if held.UpdateUnFilteredMock(nil, &served) {
		t.Fatal("precondition: the pool view took w-0 for TestA's window after TestA's window spent it")
	}
	if sv, ok := integrations.SequenceViewOf(held); !ok || sv.Test != "TestA" || sv.Worker == nil {
		t.Errorf("after the refused update the call walks %q's view (worker band %v); want TestA's, with the worker band", sv.Test, sv.Worker != nil)
	}
}
