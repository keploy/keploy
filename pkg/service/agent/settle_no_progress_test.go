package agent

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A settle whose watchdog found no progress says what it waited on: each
// connection that held work by its id, and the proxy's accepts of the
// application's connections by what they are. The replay's report of a test
// case keploy's agent did not open carries it. Each kind of give-up is
// models.ErrSettleGaveUp, which the agent's routes say as data, so the
// replay reads it as keploy's own failure.
func TestANoProgressSettleNamesWhatItWaitedOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stalled []string
		want    string
	}{
		{name: "nothing named", want: "keploy's agent made no progress settling the test's calls"},
		{name: "a connection", stalled: []string{"7"},
			want: "keploy's agent made no progress settling the test's calls; nothing moved on connection 7"},
		{name: "the accepts, then a connection", stalled: []string{settleCountsKey, "12"},
			want: "keploy's agent made no progress settling the test's calls; nothing moved on the application's connections keploy's proxy had not yet accepted and settled; connection 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := error(noProgress{stalled: tc.stalled})
			if got := err.Error(); got != tc.want {
				t.Errorf("the give-up says %q; want %q", got, tc.want)
			}
			if !errors.Is(err, errNoProgress) {
				t.Error("the give-up is not errNoProgress")
			}
			if !errors.Is(err, models.ErrSettleGaveUp) {
				t.Error("the give-up is not said as keploy's own (models.ErrSettleGaveUp)")
			}
		})
	}
	if err := error(gaveUp{held: []string{"connection 7"}}); !errors.Is(err, models.ErrSettleGaveUp) {
		t.Errorf("a settle that gave up on held work (%q) is not said as keploy's own (models.ErrSettleGaveUp)", err)
	}
}
