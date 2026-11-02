package mock

import (
	"context"
	"strings"
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

// Under declared test workers each call is charged by the keploy instance
// its connection was stamped with, never by its time: two workers' tests
// overlap, a call from either worker's child (no pid of a window's) goes to
// its own worker's test though the other's began later, a call from no
// worker while their tests are open goes to neither and is named with
// them, and a run of one worker is charged by time as before. A recorded
// request carries no instance: one inside both workers' tests is neither's.
func TestCorrelateScopesByWorkerInstance(t *testing.T) {
	windows := []models.ScopeWindow{
		{Name: "a", Start: ts(10), End: ts(30), Instance: 1},
		{Name: "b", Start: ts(15), End: ts(35), Instance: 2},
		{Name: "seq", Start: ts(28), End: ts(29)},
		{Name: "solo", Start: ts(50), End: ts(60)},
	}
	mocks := []capturedMock{
		{name: "seq-call", ts: ts(28)},
		{name: "between", ts: ts(40)},
		{name: "a-child", ts: ts(20), pid: 7, instance: 1},
		{name: "b-child", ts: ts(12), pid: 8, instance: 2},
		{name: "a-late", ts: ts(32), instance: 1},
		{name: "nobody", ts: ts(25)},
		{name: "solo-call", ts: ts(55)},
		{name: "solo-from-a", ts: ts(55), instance: 1},
	}
	got := correlateScopes(windows, mocks)
	require.Equal(t, []string{"a-child"}, names(got["a"]), "a's worker's call is a's, though b began later")
	require.Empty(t, names(got["b"]), "b's worker's call made before b began is no window's, though a held its time")
	require.Equal(t, []string{"solo-call"}, names(got["solo"]))
	require.Equal(t, []string{"seq-call"}, names(got["seq"]), "a run of one worker's test inside workers' tests holds its call")
	require.Len(t, got, 3, "a call from no worker, or from a worker outside its tests, is no test's")
	require.Equal(t, []unattributedCall{{mock: "nobody", tests: []string{"a", "b"}}}, chargeUnattributed(windows, mocks),
		"only the call from no worker made while workers' tests were open is named, with those tests")

	cases := []capturedMock{{name: "both", ts: ts(20)}, {name: "only-a", ts: ts(12)}, {name: "only-solo", ts: ts(55)}}
	byCase := correlateCases(windows, nil, cases, nil)
	require.Equal(t, []string{"only-a"}, byCase["a"].Cases, "a request inside one worker's test is that test's, though it carries no instance")
	require.Equal(t, []string{"only-solo"}, byCase["solo"].Cases)
	require.Len(t, byCase, 2, "a request inside two workers' tests is neither's")
	require.Equal(t, "seq", testAt(windows, 0, 0, ts(28)), "a call from no worker is a run of one worker's test's")
	require.Equal(t, "", testAt(windows, 0, 0, ts(20)), "a call from no worker is no worker's test's")
	require.Equal(t, "a", testAt(windows, 0, 1, ts(20)))
	require.Equal(t, "b", testAt(windows, 0, 2, ts(20)))
	require.EqualValues(t, 2, capturedOf(&models.Mock{Name: "m", SourcePID: 8, Instance: 2}).instance,
		"a recorded mock is correlated by the instance it carries")
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

// Each test open at a call from no test worker's instance, in a run of
// workers, is charged with it as a loss that leaves it without a verdict
// (models.CaptureLossUnattributed): counted per test, naming the latest such
// call, on a copy of its evidence, after what its end already said. A test
// open at no such call, a run of one worker's test, and a worker's own call
// charge nothing.
func TestEachTestOpenAtACallFromNoWorkerIsNotJudged(t *testing.T) {
	agentEv := &models.ScopeEvidence{Reason: "keploy: a miss.", CannotCapture: models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1}}}
	windows := []models.ScopeWindow{
		{Name: "a", Start: ts(10), End: ts(30), Instance: 1, Evidence: agentEv},
		{Name: "b", Start: ts(15), End: ts(35), Instance: 2},
		{Name: "c", Start: ts(40), End: ts(45), Instance: 1},
		{Name: "solo", Start: ts(50), End: ts(60)},
	}
	mocks := []capturedMock{
		{name: "late", ts: ts(33), pid: 9},
		{name: "early", ts: ts(20), pid: 4},
		{name: "a-own", ts: ts(21), instance: 1},
		{name: "c-other", ts: ts(42), instance: 2},
		{name: "solo-nobody", ts: ts(55), pid: 5},
		{name: "between", ts: ts(38), pid: 6},
	}
	got := chargeUnattributed(windows, mocks)
	require.Equal(t, []unattributedCall{{mock: "late", pid: 9, tests: []string{"b"}}, {mock: "early", pid: 4, tests: []string{"a", "b"}}}, got)
	require.Equal(t, models.CaptureLosses{
		{Kind: models.CaptureLossUnattributed, Count: 1, What: "the latest: mock early, from process 4"},
		{Kind: models.CaptureLossUnixSocket, Count: 1},
	}, windows[0].Evidence.CannotCapture, "a: the call made in it, beside what its end charged")
	require.True(t, strings.HasPrefix(windows[0].Evidence.Reason, "keploy: a miss. Also not judged: the application made 1 call(s) from a process in no test worker's keploy instance"), windows[0].Evidence.Reason)
	require.Len(t, agentEv.CannotCapture, 1, "the agent's evidence is not written to")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: 2, What: "the latest: mock late, from process 9"}}, windows[1].Evidence.CannotCapture,
		"b: both calls, the later named, whatever order they were captured in")
	require.True(t, strings.HasPrefix(windows[1].Evidence.Reason, "keploy: not judged: the application made 2 call(s)"), windows[1].Evidence.Reason)
	require.Nil(t, windows[2].Evidence, "c: another worker's call is no loss of its")
	require.Nil(t, windows[3].Evidence, "a run of one worker's test holds the call from no worker")
	_, notJudged := windows[1].Evidence.CannotCapture.Split()
	require.Len(t, notJudged, 1, "the loss leaves the test without a verdict, never failed")
	require.Contains(t, notJudged.NotJudgedRemedy(), models.UnattributedRemedy)
	require.True(t, models.CaptureLossUnattributed.PerTest(), "a replay's refused call is charged to each test open then, never every later test")
}
