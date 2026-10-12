package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func TestRecordBeginHandsBackTheStartItsCapturesCarry(t *testing.T) {
	a := newRecordAgent()
	at := time.Date(2026, 10, 12, 13, 0, 0, 123_000_000, time.UTC)

	ack, err := a.BeginScopeAck(t.Context(), "spec > test", 0, 0, at)
	require.NoError(t, err)
	require.Equal(t, "2026-10-12T13:00:00.123Z", ack.ClockBase,
		"the harness pins the test's clock to its own window start")

	owner, start := a.resolveOwnerWindow(0, at.Add(250*time.Millisecond))
	require.Equal(t, "spec > test", owner)
	require.Equal(t, ack.ClockBase, models.FormatScopeStart(start),
		"a capture in the window must carry the start the begin handed out, or a replay hands back another")
}

func TestScopeClockBaseAnswersTheInstalledStart(t *testing.T) {
	a := newRecordAgent()
	a.SetScopeClock(map[string]string{"spec > test": "2026-10-12T13:00:00.123Z"})
	require.Equal(t, "2026-10-12T13:00:00.123Z", a.ScopeClockBase("spec > test"))
	require.Equal(t, "", a.ScopeClockBase("another test"))

	a.SetScopeClock(nil)
	require.Equal(t, "", a.ScopeClockBase("spec > test"), "a recording without stamps clears the bases")
}
