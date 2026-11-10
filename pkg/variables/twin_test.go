package variables

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A binder of a recorded api-server set binds each app id twice at one
// place: the test's own variable (appIdN) and its run twin, which keploy
// record carries to the recordings of later tests that hold the made value
// (run-appIdN-carried-K), one recorded value for both. The table binds the
// pairs in one call, under either switch: two names with one recorded value
// are one value, not two variables sharing a live one (TwoValues). The test
// then reads its own name, and a later test reads the twin alone (the run's)
// and binds its own appIdN at another app's id beside it; the later test may
// not bind its own appIdN at the earlier app's id (a copy of the run twin,
// recorded with another value).
func TestTable_ABinderOfAVariableAndItsRunTwinAtOnePlace(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		t.Run("KEPLOY_PROVEN="+sw, func(t *testing.T) {
			t.Setenv(provenswitch.Env, sw)
			var tb Table
			tb.KeepRun()
			tb.KeepEnded()
			tb.StartOpeningTest("TestA", func() {}, nil, nil)
			var binds []Binding
			for i := 0; i < 5; i++ {
				place := fmt.Sprintf("body.docs.msgs.0.apps.%d.app_id", i)
				live, rec := fmt.Sprintf("live-a%d", i), fmt.Sprintf("rec-a%d", i)
				binds = append(binds,
					Binding{Name: fmt.Sprintf("appId%d", i), Value: live, Recorded: rec, Mock: "mock-2255", Place: place},
					Binding{Name: fmt.Sprintf("%sappId%d-carried-3", models.RunVariablePrefix, i), Value: live, Recorded: rec, Mock: "mock-2255", Place: place})
			}
			if bad, with, stale, ok := tb.BindAll(tb.Gen(), binds); !ok {
				t.Fatalf("the binder of a variable and its run twin was refused: %+v beside %+v (stale %v)", bad, with, stale)
			}
			for i, b := range binds {
				got, ok := tb.Lookup(b.Name)
				if !ok || got.Value != b.Value {
					t.Fatalf("bind %d: test A reads %s as %+v (%v)", i, b.Name, got, ok)
				}
			}
			if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{binds[0]}); !ok {
				t.Fatal("test A's later call that sends appId0's value again was refused")
			}
			tb.StartOpeningTest("TestB", func() {}, nil, nil)
			twin := binds[1]
			if got, ok := tb.Lookup(twin.Name); !ok || got.Value != twin.Value {
				t.Fatalf("test B reads the run twin %s as %+v (%v)", twin.Name, got, ok)
			}
			if _, ok := tb.Lookup("appId0"); ok {
				t.Fatal("test A's own appId0 outlived test B's start")
			}
			ownB := Binding{Name: "appId0", Value: "live-b0", Recorded: "rec-b0", Mock: "mock-2400", Place: "body.docs.msgs.0.app_id"}
			if bad, with, _, ok := tb.BindAll(tb.Gen(), []Binding{ownB}); !ok {
				t.Fatalf("test B could not bind its own appId0 beside test A's run twin: %+v beside %+v", bad, with)
			}
			copyA := Binding{Name: "appId1", Value: twin.Value, Recorded: "rec-b1", Mock: "mock-2401", Place: "body.docs.msgs.0.app_id"}
			if _, with, _, ok := tb.BindAll(tb.Gen(), []Binding{copyA}); ok || with.Name != twin.Name {
				t.Fatalf("test B bound its own appId1 at test A's app id, recorded with another value: ok %v, beside %+v", ok, with)
			}
			if b, ended, ok := tb.LookupOf("TestA", "appId0"); !ok || !ended || b.Value != binds[0].Value {
				t.Fatalf("test A's ended appId0 reads %+v (ended %v, %v)", b, ended, ok)
			}
		})
	}
}
