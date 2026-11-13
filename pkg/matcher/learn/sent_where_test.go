package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// TestSentWhereNamesWhereTheRunSentAPlace: a reason names a place the set's
// start-up sent as the start-up's, one an earlier test sent as an earlier
// test's, and only the running test's own places as earlier in the test.
func TestSentWhereNamesWhereTheRunSentAPlace(t *testing.T) {
	for _, c := range []struct {
		from Place
		want string
	}{
		{Place{Own: true}, "earlier in the test"},
		{Place{}, "earlier in the test"},
		{Place{Client: true, Given: true}, "earlier in the test"},
		{Place{Run: true}, "in an earlier test of the set"},
		{Place{Run: true, InStartUp: true}, "in the test set's start-up"},
		{Place{InStartUp: true}, "in the test set's start-up"},
	} {
		if got := SentWhere(c.from); got != c.want {
			t.Errorf("SentWhere(own %v, run %v, client %v, start-up %v) = %q; want %q", c.from.Own, c.from.Run, c.from.Client, c.from.InStartUp, got, c.want)
		}
	}
}

// TestCallBindsIsTheTestsOwnCallOrTheRunsUnderTheSwitch: a plan binds a
// variable at a call that sent the value whole where the call is the test's
// own, or the run's under the proven switch; a mock of the pool, never.
func TestCallBindsIsTheTestsOwnCallOrTheRunsUnderTheSwitch(t *testing.T) {
	for _, sw := range []string{"1", ""} {
		t.Setenv(provenswitch.Env, sw)
		for _, c := range []struct {
			from Place
			want bool
		}{
			{Place{Own: true}, true},
			{Place{Own: true, Run: true}, true},
			{Place{Run: true}, sw == "1"},
			{Place{Run: true, InStartUp: true}, sw == "1"},
			{Place{}, false},
			{Place{InStartUp: true}, false},
		} {
			if got := c.from.CallBinds(); got != c.want {
				t.Errorf("switch %q: CallBinds(own %v, run %v, start-up %v) = %v; want %v", sw, c.from.Own, c.from.Run, c.from.InStartUp, got, c.want)
			}
		}
	}
}
