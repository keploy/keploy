package mock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func ts(sec int) time.Time { return time.Unix(int64(sec), 0) }

func TestCorrelateScopes(t *testing.T) {
	tests := []struct {
		name    string
		windows []models.ScopeWindow
		mocks   []capturedMock
		want    map[string][]string // test name -> mock names, order-independent
	}{
		{
			name: "each mock buckets into its own window",
			windows: []models.ScopeWindow{
				{Name: "t1", Start: ts(10), End: ts(20)},
				{Name: "t2", Start: ts(30), End: ts(40)},
			},
			mocks: []capturedMock{
				{name: "mock-0", ts: ts(15)},
				{name: "mock-1", ts: ts(35)},
			},
			want: map[string][]string{"t1": {"mock-0"}, "t2": {"mock-1"}},
		},
		{
			name: "mock outside every window is dropped (stays reusable at replay)",
			windows: []models.ScopeWindow{
				{Name: "t1", Start: ts(10), End: ts(20)},
			},
			mocks: []capturedMock{
				{name: "boot", ts: ts(5)}, // before any window
				{name: "mock-0", ts: ts(15)},
			},
			want: map[string][]string{"t1": {"mock-0"}},
		},
		{
			name: "overlapping windows resolve to the innermost (latest-started)",
			windows: []models.ScopeWindow{
				{Name: "outer", Start: ts(10), End: ts(40)},
				{Name: "inner", Start: ts(20), End: ts(30)},
			},
			mocks: []capturedMock{
				{name: "mock-0", ts: ts(25)}, // inside both; innermost wins
			},
			want: map[string][]string{"inner": {"mock-0"}},
		},
		{
			name:    "no windows -> empty mapping (suite-level fallback)",
			windows: nil,
			mocks:   []capturedMock{{name: "mock-0", ts: ts(15)}},
			want:    map[string][]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := correlateScopes(tc.windows, tc.mocks)
			require.Len(t, got, len(tc.want), "number of tests with mappings")
			for name, wantNames := range tc.want {
				entries, ok := got[name]
				require.True(t, ok, "expected mapping for %q", name)
				gotNames := make([]string, len(entries))
				for i, e := range entries {
					gotNames[i] = e.Name
				}
				require.ElementsMatch(t, wantNames, gotNames, "mock names for %q", name)
			}
		})
	}
}

func TestMissPolicyValidAndBehaviour(t *testing.T) {
	require.True(t, models.MissPolicy("").Valid())
	require.True(t, models.MissFail.Valid())
	require.True(t, models.MissPassthrough.Valid())
	require.True(t, models.MissRecord.Valid())
	require.False(t, models.MissPolicy("bogus").Valid())

	require.False(t, models.MissFail.PassesThroughOnMiss())
	require.True(t, models.MissPassthrough.PassesThroughOnMiss())
	require.True(t, models.MissRecord.PassesThroughOnMiss())

	require.False(t, models.MissPassthrough.RecordsOnMiss())
	require.True(t, models.MissRecord.RecordsOnMiss())
}

// TestCorrelateScopesParallelPID is the case Design A (per-PID attribution)
// fixes: two workers' scope windows OVERLAP in time, so a pure timestamp scan
// would attribute both workers' mocks to whichever window started later. The
// source PID disambiguates them exactly.
func TestCorrelateScopesParallelPID(t *testing.T) {
	// worker A (pid 100) ran test "a" over [10,30]; worker B (pid 200) ran test
	// "b" over [15,35] — the two windows overlap on [15,30].
	windows := []models.ScopeWindow{
		{Name: "a", Start: ts(10), End: ts(30), PID: 100},
		{Name: "b", Start: ts(15), End: ts(35), PID: 200},
	}
	mocks := []capturedMock{
		{name: "a-call", ts: ts(20), pid: 100}, // in BOTH by time; PID => "a"
		{name: "b-call", ts: ts(25), pid: 200}, // in BOTH by time; PID => "b"
	}
	got, _ := correlateScopes(windows, mocks)

	require.ElementsMatch(t, []string{"a-call"}, names(got["a"]),
		"worker A's call must attribute to test a, not the later-started overlapping window b")
	require.ElementsMatch(t, []string{"b-call"}, names(got["b"]))

	// A PID-less mock (e.g. a child process's call) falls back to the timestamp
	// scan: the innermost (latest-started) containing window wins.
	fallback, _ := correlateScopes(windows, []capturedMock{{name: "orphan", ts: ts(20), pid: 0}})
	require.ElementsMatch(t, []string{"orphan"}, names(fallback["b"]),
		"PID-less mock should fall back to the innermost time window")
}

func names(entries []models.MockEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}
