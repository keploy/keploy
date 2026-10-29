package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A miss whose field diff from its closest recording is only the
// application's clock (a timer's "older than now" filter) is read with Clock
// under the switch, and only when the diff is whole: judging sets its test
// aside on the clock, never as a call keploy did not record.
func TestUnderTheSwitchAMissThatIsTheRecordingWithTheClockMovedSaysSo(t *testing.T) {
	clock := models.MockFieldDiff{Path: "body.filter.dirtyAt.$lte.$date.$numberLong", Kind: models.DiffKindValueChanged,
		Expected: "1798456789123", Actual: "1798456851977"}
	other := models.MockFieldDiff{Path: "body.update.$set.owner", Kind: models.DiffKindValueChanged, Expected: "w1", Actual: "w2"}
	cut := make([]models.MockFieldDiff, mismatch.MaxFieldDiffs)
	for i := range cut {
		cut[i] = clock
	}
	for _, c := range []struct {
		name   string
		on     string
		diffs  []models.MockFieldDiff
		closer bool
	}{
		{"clock only", "1", []models.MockFieldDiff{clock}, true},
		{"clock and another field", "1", []models.MockFieldDiff{clock, other}, false},
		{"a list the parser may have cut", "1", cut, false},
		{"no diff", "1", nil, false},
		{"switch off", "", []models.MockFieldDiff{clock}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.on)
			p := startedDrainProxy(t, 16)
			ctx := context.Background()
			p.BeginTestErrorCapture()
			p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched,
				&models.MockMismatchReport{Protocol: "MongoDB", ActualSummary: "findAndModify ptw_endpoints",
					ClosestMock: "mock-12", FieldDiffs: c.diffs}))
			if ok, why := p.flushOnEvents(ctx, &flushMarker{done: make(chan struct{})}); !ok {
				t.Fatalf("the drain did not route the miss: %s", why)
			}
			got, err := p.GetMockErrors(ctx)
			if err != nil || len(got) != 1 {
				t.Fatalf("read %+v (%v); want the one miss", got, err)
			}
			if got[0].Clock != c.closer {
				t.Fatalf("Clock = %v, want %v (diffs %d)", got[0].Clock, c.closer, len(c.diffs))
			}
		})
	}
}
