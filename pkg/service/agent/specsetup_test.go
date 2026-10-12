package agent

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// After a test ends, the pool must be the spec's own setup (its suite recording),
// not the whole tape: every beforeAll after a spec's first describe runs in that
// window, and with the whole tape it was answered from other specs' recordings
// and consumed their only copies.
func TestEndScopeStagesTheSpecSetupNotTheWholeTape(t *testing.T) {
	ctx := context.Background()
	a, p := replayAgent(t,
		[]string{"a-t1", "a-s1", "b-t1", "b-s1", "unowned"},
		map[string][]string{
			"specA.spec.ts > d > t1":  {"a-t1"},
			"__suite__:specA.spec.ts": {"a-s1"},
			"specB.spec.ts > d > t1":  {"b-t1"},
			"__suite__:specB.spec.ts": {"b-s1"},
		})

	_, err := a.BeginScopeAck(ctx, "specA.spec.ts > d > t1", 0, 0, time.Time{})
	require.NoError(t, err)
	require.NoError(t, a.EndScope(ctx, "specA.spec.ts > d > t1", 0))
	got := append([]string(nil), p.staged[len(p.staged)-1]...)
	sort.Strings(got)
	require.Equal(t, []string{"a-s1", "unowned"}, got,
		"between tests: own spec's suite plus unowned overflow, nothing from spec B")

	// Ending the suite (spec switch / worker teardown) leaves the setup staged.
	before := len(p.staged)
	require.NoError(t, a.EndScope(ctx, "__suite__:specA.spec.ts", 0))
	require.Equal(t, before, len(p.staged), "a suite end must not open a window on the whole tape")
}

// A spec with no suite recording and no shared requests is narrowed to the
// unowned overflow, never widened to other specs' recordings.
func TestEndScopeWithoutSuiteRecordingKeepsOtherSpecsOut(t *testing.T) {
	ctx := context.Background()
	a, p := replayAgent(t,
		[]string{"c-t1", "b-s1", "unowned"},
		map[string][]string{
			"specC.spec.ts > d > t1":  {"c-t1"},
			"__suite__:specB.spec.ts": {"b-s1"},
		})
	_, err := a.BeginScopeAck(ctx, "specC.spec.ts > d > t1", 0, 0, time.Time{})
	require.NoError(t, err)
	require.NoError(t, a.EndScope(ctx, "specC.spec.ts > d > t1", 0))
	require.Equal(t, []string{"unowned"}, p.staged[len(p.staged)-1])
}

// A test absent from the recording runs inside its own spec's setup pool rather
// than whatever the previous spec left staged.
func TestUnmappedScopeStagesItsOwnSpecSetup(t *testing.T) {
	ctx := context.Background()
	a, p := replayAgent(t,
		[]string{"a-s1", "b-s1"},
		map[string][]string{
			"__suite__:specA.spec.ts": {"a-s1"},
			"__suite__:specB.spec.ts": {"b-s1"},
		})
	_, err := a.BeginScopeAck(ctx, "__suite__:specA.spec.ts", 0, 0, time.Time{})
	require.NoError(t, err)
	ack, err := a.BeginScopeAck(ctx, "specB.spec.ts > d > renamed test", 0, 0, time.Time{})
	require.NoError(t, err)
	require.False(t, ack.Scoped)
	require.Equal(t, []string{"b-s1"}, p.staged[len(p.staged)-1])
}
