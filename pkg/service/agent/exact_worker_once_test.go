package agent

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording of a once series answers one call in the run wherever it
// lands, so the run reaches it before its place: a start-up job's lease
// placed after the test that first recorded it is shown from the set's
// start. Under the background rules it comes after every place the run has
// reached, so an identical recording placed where the run is answers first;
// once its own place is reached it takes that place's turn. A worker-scope
// recording that is no once series' member stays gated by its place.
func TestAOnceMemberIsReachedBeforeItsPlace(t *testing.T) {
	worker := []models.MockEntry{
		{Name: "boot"},
		{Name: "lease", Place: "T3"},
		{Name: "t1-insert", Place: "T1"},
		{Name: "t3-insert", Place: "T3"},
	}
	once := map[string]bool{"lease": true}
	bg := &testBackground{noSibling: map[string]bool{}, idle: map[string]bool{}}
	t1 := map[string]bool{"T1": true}
	t1At := map[string]int{"T1": 1}
	for _, tc := range []struct {
		name    string
		reached map[string]bool
		at      map[string]int
		bg      *testBackground
		once    map[string]bool
		want    string
	}{
		{"today's rules: the once member before its place, in recorded order", t1, t1At, nil, once, "boot,lease,t1-insert"},
		{"the background rules: the once member before its place, after every reached place", t1, t1At, bg, once, "boot,t1-insert,lease"},
		{"no once series: the recording stays gated by its place", t1, t1At, bg, nil, "boot,t1-insert"},
		{"its place reached: the once member takes that place's turn", map[string]bool{"T1": true, "T3": true}, map[string]int{"T1": 1, "T3": 2}, bg, once, "boot,t1-insert,lease,t3-insert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(reachableWorker(worker, tc.reached, tc.at, tc.bg, tc.once), ","); got != tc.want {
				t.Fatalf("reached %s; want %s", got, tc.want)
			}
		})
	}
}

// The once members are the names the proxy holds as once
// (proxy.MockManager.SetPoolSeries): a name two series hold belongs to the
// first in series order, so it is a once member only when that series is
// once; an empty name is none, and a series no once id names gives none.
func TestOnceMembersAreTheNamesTheProxyHoldsOnce(t *testing.T) {
	series := map[string][]string{
		"a/-/1": {"x", "shared"},
		"b/-/1": {"shared", "y", ""},
		"c/-/1": {"z"},
	}
	got := onceMembers(series, []string{"b/-/1", "c/-/1"})
	var names []string
	for _, n := range []string{"", "x", "shared", "y", "z"} {
		if got[n] {
			names = append(names, n)
		}
	}
	if strings.Join(names, ",") != "y,z" {
		t.Fatalf("once members %q; want y and z (shared is a/-/1's, which is not once)", names)
	}
	if len(onceMembers(series, nil)) != 0 {
		t.Fatalf("with no once series, members %v", onceMembers(series, nil))
	}
}
