package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.uber.org/zap"
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
	got := correlateScopes(windows, mocks)

	require.ElementsMatch(t, []string{"a-call"}, names(got["a"]),
		"worker A's call must attribute to test a, not the later-started overlapping window b")
	require.ElementsMatch(t, []string{"b-call"}, names(got["b"]))

	// A PID-less mock (e.g. a child process's call) falls back to the timestamp
	// scan: the innermost (latest-started) containing window wins.
	fallback := correlateScopes(windows, []capturedMock{{name: "orphan", ts: ts(20), pid: 0}})
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

// Every mapping entry keploy mock record writes states its mock's kind, as
// keploy record's and a pulled set's entries do: a test's (correlateScopes),
// a start-up start's and a test's under the boot plan (classify), and the
// pool's (startupMocks). Before, they all wrote `kind: ""`. Under the
// proven switch keploy test reports an unconsumed entry with the entry's
// own kind (unconsumed_mocks), so each came out with none. The dependency
// row's type (depTypeFor), the mismatch report and the runner's MockRef
// fall back to the loaded mock of that name, so they had none for a mock
// no pool loaded. DNS and connection-failure mocks stay out of the check
// by name while the set's mock file holds them; an entry whose mock the
// file no longer holds is left out only by its own kind. Each writer gets
// two kinds, so a writer that hard-codes one fails too. Mutation: drop
// Kind from any one of the three writers, or the kind from Record's
// capture (record.go, caught by TestMockRecordWritesTheKindItCaptured).
func TestMockRecordMappingEntriesStateTheirKind(t *testing.T) {
	windows := []models.ScopeWindow{{Name: "t1", Start: ts(10), End: ts(20)}}
	mocks := []capturedMock{
		{name: "mock-0", kind: string(models.HTTP), ts: ts(15)},
		{name: "mock-1", kind: string(models.Postgres), ts: ts(5)},
		{name: "mock-4", kind: string(models.Mongo), ts: ts(16)},
		{name: "mock-5", kind: string(models.HTTP), ts: ts(25)},
	}
	byTest := correlateScopes(windows, mocks)
	require.Equal(t, []models.MockEntry{{Name: "mock-0", Kind: string(models.HTTP)}, {Name: "mock-4", Kind: string(models.Mongo)}}, byTest["t1"])
	require.Equal(t, []models.MockEntry{{Name: "mock-1", Kind: string(models.Postgres)}, {Name: "mock-5", Kind: string(models.HTTP)}}, startupMocks(windows, mocks, nil))

	starts := []models.ScopeWindow{{App: true, Ref: "s1", Name: "app#1", Dir: "/r", Program: "app", N: 1, Ready: ts(8)}}
	boot := []capturedMock{
		{name: "mock-2", kind: string(models.Mongo), ts: ts(6), ref: "s1", boot: true},
		{name: "mock-3", kind: string(models.HTTP), ts: ts(15), ref: "s1"},
	}
	plan := classify(windows, starts, []models.SuiteSpan{{Dir: "/r", Start: ts(0), End: ts(30)}}, boot)
	kinds := map[string]string{}
	for _, b := range plan.boots {
		for _, e := range b.Mocks {
			kinds[e.Name] = e.Kind
		}
	}
	for _, es := range plan.tests {
		for _, e := range es {
			kinds[e.Name] = e.Kind
		}
	}
	require.Equal(t, map[string]string{"mock-2": string(models.Mongo), "mock-3": string(models.HTTP)}, kinds)
}

// The kind each entry states is the one Record captured from its own
// recorded mock (the capturedMock it builds per mock), through to
// mappings.yaml: a test's entries and the start-up pool's. Mutation: drop
// kind from that capture and both read back `kind: ""`.
func TestMockRecordWritesTheKindItCaptured(t *testing.T) {
	instr := newRunnerInstr(t, mark("orders/e2e.TestA", 0, 10*time.Millisecond))
	pool := mockAt("mock-1", runnerT0.Add(20*time.Millisecond))
	pool.Kind = models.Postgres
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond)), pool}
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, recordSet(t, instr, mapDB, nil))
	got, _, err := mapDB.Get(context.Background(), "set")
	require.NoError(t, err)
	require.Equal(t, map[string][]models.MockEntry{"orders/e2e.TestA": {{Name: "mock-0", Kind: string(models.HTTP)}}}, got)
	startup, err := mapDB.GetStartup(context.Background(), "set")
	require.NoError(t, err)
	require.Equal(t, []models.MockEntry{{Name: "mock-1", Kind: string(models.Postgres)}}, startup)
}
