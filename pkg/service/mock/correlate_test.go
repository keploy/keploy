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
			got := correlateScopes(tc.windows, tc.mocks)
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
