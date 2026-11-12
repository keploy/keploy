//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A trailing clean-up of TestT that lands in TestU's window and misses
// (models.UnmatchedCall.TrailsOf: it carries TestT's kept value) is TestT's,
// not TestU's: TestU's end charges it to no test, naming TestU's window, and
// TestU's evidence does not list it. It still counts against the tests, so
// the run's receipt lists it uncharged and the run fails on it; the trail
// mark stays on the call as evidence keeps it. A miss that trails the ending
// test itself is its own call and stays its miss, as an unmarked one does.
func TestTrailsOfMissIsChargedToNoTestAndCounts(t *testing.T) {
	began := time.Now()
	a, p := wireAgent(t, models.MODE_TEST)
	p.misses = []models.UnmatchedCall{
		{Protocol: "MongoDB", ActualSummary: "delete uiTestSetJoins", Destination: "trail:27017", Kind: models.MissInTest, At: began.Add(time.Second), TrailsOf: "o::TestT"},
		{Protocol: "MongoDB", ActualSummary: "delete uiTestSetJoins", Destination: "early:27017", Kind: models.MissInTest, At: began.Add(-time.Second), TrailsOf: "o::TestT"},
		{Protocol: "MongoDB", ActualSummary: "delete uiTestSetJoins", Destination: "own:27017", Kind: models.MissInTest, At: began.Add(time.Second), TrailsOf: "o::TestU"},
		{Protocol: "MongoDB", ActualSummary: "find apps", Destination: "plain:27017", Kind: models.MissInTest, At: began.Add(time.Second)},
	}
	ev, err := a.testEvidence(context.Background(), "o::TestU", false, testBegin{at: began})
	require.NoError(t, err)
	var listed []string
	for _, m := range ev.Misses {
		listed = append(listed, m.Destination)
	}
	require.Equal(t, []string{"own:27017", "plain:27017"}, listed, "the misses TestU's evidence lists")
	uncharged := map[string]string{}
	for _, m := range a.takeChargedMisses() {
		if m.ChargedTo != "" {
			require.Equal(t, "o::TestU", m.ChargedTo)
			continue
		}
		uncharged[m.Destination] = m.WindowOf
		require.True(t, m.CountsAgainstTest(), "%s, a trailing miss, counts against no test", m.Destination)
		require.Equal(t, "o::TestT", m.Sanitized().TrailsOf, "%s: evidence drops the test it trails", m.Destination)
	}
	require.Equal(t, map[string]string{"trail:27017": "o::TestU", "early:27017": ""}, uncharged, "the trailing misses charged to no test, and the window each names")
}
