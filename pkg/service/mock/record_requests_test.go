package mock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	rec "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// memTestDB is an in-memory test-case store that remembers what was inserted and deleted.
type memTestDB struct {
	mu       sync.Mutex
	existing []*models.TestCase
	inserted []string
	deleted  []string
	// presentAtInsert is how many old cases were still stored when the last new one was inserted.
	presentAtInsert int
}

func (db *memTestDB) InsertTestCase(_ context.Context, tc *models.TestCase, _ string, _ bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.inserted = append(db.inserted, tc.Name)
	db.presentAtInsert = len(db.existing) - len(db.deleted)
	return nil
}

func (db *memTestDB) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return db.existing, nil
}

func (db *memTestDB) DeleteTests(_ context.Context, _ string, ids []string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.deleted = append(db.deleted, ids...)
	return nil
}

func caseAt(name string, at time.Time) *models.TestCase {
	return &models.TestCase{Name: name, Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "/orders", Timestamp: at}}
}

func withRequests(cfg *config.Config) {
	cfg.Mock.RecordRequests = true
	config.SetByPassPorts(cfg, []uint{8080})
}

// mappedCases reads the mapping file back as test name -> case names.
func mappedCases(t *testing.T, dir string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "set", "mappings.yaml"))
	require.NoError(t, err)
	mapping, err := mapdb.DecodeMapping(data, zap.NewNop())
	require.NoError(t, err)
	out := map[string][]string{}
	for _, tc := range mapping.TestCases {
		if len(tc.Cases) > 0 {
			out[tc.ID] = tc.Cases
		}
	}
	return out
}

func TestRecordRequestsFlagReachesTheAgentSetup(t *testing.T) {
	for _, on := range []bool{true, false} {
		instr := newRunnerInstr(t)
		db := &memTestDB{}
		require.NoError(t, recordSetWith(t, zap.NewNop(), instr, nil, db, func(cfg *config.Config) {
			if on {
				withRequests(cfg)
			}
		}))
		opts, read := instr.setup()
		require.Equal(t, on, opts.RecordRequests)
		require.Equal(t, on, read, "the incoming stream is opened exactly when the flag is on")
	}
}

// Each stored case is listed under the flow whose window held its request; one outside every window stays on disk but unlisted.
func TestRecordRequestsStoresCasesAndListsThemPerFlow(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.incoming = []*models.TestCase{
		caseAt("test-1", runnerT0.Add(5*time.Millisecond)),
		caseAt("test-2", runnerT0.Add(15*time.Millisecond)),
		caseAt("test-3", runnerT0.Add(50*time.Millisecond)),
	}
	db := &memTestDB{existing: []*models.TestCase{caseAt("test-9", runnerT0)}}
	dir := t.TempDir()
	require.NoError(t, recordSetWith(t, zap.NewNop(), instr, mapdb.New(zap.NewNop(), dir, ""), db, withRequests))

	require.Equal(t, []string{"test-1", "test-2", "test-3"}, db.inserted)
	require.Equal(t, []string{"test-9"}, db.deleted, "a re-record ends with the old cases gone")
	require.Equal(t, 1, db.presentAtInsert, "the old cases are still there while the new ones are named, so numbering continues past them")
	require.Equal(t, map[string][]string{
		"orders/e2e.TestA": {"test-1"},
		"orders/e2e.TestC": {"test-2"},
	}, mappedCases(t, dir))
}

func TestRecordRequestsTurnOffWhenTheyCannotBeRecorded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		db    TestDB
		tweak func(*config.Config)
		warn  string
	}{
		{"no test-case store", nil, withRequests, "no test-case store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			instr := newRunnerInstr(t, sequentialMarks()...)
			instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
			require.NoError(t, recordSetWith(t, zap.New(core), instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), tc.db, tc.tweak))
			opts, read := instr.setup()
			require.False(t, opts.RecordRequests, "the ingress hooks stay off")
			require.False(t, read)
			require.Equal(t, 1, logs.FilterMessageSnippet(tc.warn).Len())
		})
	}
}

func TestCorrelateCases(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{
		{Name: "orders/e2e.TestA", Start: at(0), End: at(100)},
		{Name: "orders/e2e.TestA/create", Start: at(10), End: at(40)},
		{Name: "orders/e2e.TestA/delete", Start: at(50), End: at(90)},
		{Name: "orders/e2e.TestB", Start: at(100), End: at(200)},
	}
	steps := []models.ScopeWindow{
		{Name: "create", Start: at(10), End: at(40)},
		{Name: "delete", Start: at(50), End: at(90)},
	}
	cases := []capturedMock{
		{name: "test-1", ts: at(15), end: at(25)},
		{name: "test-2", ts: at(55), end: at(70)},
		{name: "test-3", ts: at(95), end: at(110)},
		{name: "test-4", ts: at(120), end: at(130)},
		{name: "test-5", ts: at(300), end: at(310)},
	}
	for _, tc := range []struct {
		name  string
		mocks []capturedMock
		cases []capturedMock
		want  map[string]models.MappedTestCase
	}{
		{
			name: "each mock goes to the case of its test whose request and response hold it",
			mocks: []capturedMock{
				{name: "mock-1", ts: at(20)},
				{name: "mock-2", ts: at(30)},
				{name: "mock-3", ts: at(60)},
				{name: "mock-6", ts: at(96)},
				{name: "mock-9", ts: at(105)},
				{name: "mock-5", ts: at(125)},
				{name: "mock-7", ts: at(150)},
			},
			cases: cases,
			want: map[string]models.MappedTestCase{
				"orders/e2e.TestA/create": {Cases: []string{"test-1"}, CaseMocks: map[string][]string{"test-1": {"mock-1"}}, CaseSteps: map[string]string{"test-1": "create"}},
				"orders/e2e.TestA/delete": {Cases: []string{"test-2"}, CaseMocks: map[string][]string{"test-2": {"mock-3"}}, CaseSteps: map[string]string{"test-2": "delete"}},
				"orders/e2e.TestA":        {Cases: []string{"test-3"}, CaseMocks: map[string][]string{"test-3": {"mock-6"}}, CaseSteps: map[string]string{"test-3": ""}},
				"orders/e2e.TestB":        {Cases: []string{"test-4"}, CaseMocks: map[string][]string{"test-4": {"mock-5"}}, CaseSteps: map[string]string{"test-4": ""}},
			},
		},
		{
			name:  "a config or connection mock is never a case's",
			mocks: []capturedMock{{name: "mock-4", ts: at(60), boot: true}},
			cases: cases[1:2],
			want: map[string]models.MappedTestCase{
				"orders/e2e.TestA/delete": {Cases: []string{"test-2"}, CaseSteps: map[string]string{"test-2": "delete"}},
			},
		},
		{
			name:  "no cases, nothing to list",
			mocks: []capturedMock{{name: "mock-1", ts: at(20)}},
			want:  map[string]models.MappedTestCase{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, correlateCases(windows, tc.mocks, tc.cases, steps))
		})
	}
	require.Empty(t, correlateCases(nil, nil, cases, steps))
}

func TestStartupMocks(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{
		{Name: "orders/e2e.TestA", Start: at(0), End: at(100)},
		{Name: "orders/e2e.TestB", Start: at(100), End: at(200)},
	}
	mocks := []capturedMock{
		{name: "mock-0", ts: at(-5)},
		{name: "mock-1", ts: at(20)},
		{name: "mock-2", ts: at(60), boot: true},
		{name: "mock-3", ts: at(150)},
		{name: "mock-4", ts: at(250)},
	}
	require.Equal(t, []models.MockEntry{{Name: "mock-0"}, {Name: "mock-2"}, {Name: "mock-4"}}, startupMocks(windows, mocks, nil))
	require.Len(t, startupMocks(nil, mocks, nil), 5)

	// Under the proven wire each pool mock carries the key of the claim it
	// was made under (design v14 §5.2); one made before any claim has none.
	claims := []models.ScopeWindow{{Claim: "orders", Start: at(-1)}, {Claim: "users", Start: at(200)}}
	require.Equal(t, []models.MockEntry{{Name: "mock-0"}, {Name: "mock-2", Key: "orders"}, {Name: "mock-4", Key: "users"}}, startupMocks(windows, mocks, claims))
}

func TestClaimKeyAt(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	claims := []models.ScopeWindow{{Claim: "a", Start: at(0)}, {Claim: "b", Start: at(10)}}
	require.Equal(t, "", claimKeyAt(claims, at(-1)))
	require.Equal(t, "a", claimKeyAt(claims, at(0)), "a call at the claim's own instant is its")
	require.Equal(t, "a", claimKeyAt(claims, at(9)))
	require.Equal(t, "b", claimKeyAt(claims, at(10)))
	require.Equal(t, "b", claimKeyAt(claims, at(1000)), "until the next claim")
	require.Equal(t, "", claimKeyAt(nil, at(5)))
}

func stepsMarks() []models.ScopeWindow {
	return []models.ScopeWindow{
		mark("orders/e2e.TestA/create", 10*time.Millisecond, 40*time.Millisecond),
		mark("orders/e2e.TestA", 0, 100*time.Millisecond),
		mark("orders/e2e.TestB", 100*time.Millisecond, 200*time.Millisecond),
	}
}

func TestRecordWritesCaseMocksStepsAndStartup(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	instr := newRunnerInstr(t, stepsMarks()...)
	instr.incoming = []*models.TestCase{
		httpCase("test-1", "POST", "/orders", 201, "{}", at(15)),
		httpCase("test-2", "GET", "/orders", 200, "[]", at(60)),
		httpCase("test-3", "DELETE", "/orders/1", 204, "", at(120)),
	}
	config := mockAt("mock-3", at(120))
	config.Spec.Metadata = map[string]string{"type": "config"}
	instr.mocks = []*models.Mock{
		mockAt("mock-0", at(-5)),
		mockAt("mock-1", at(15)),
		mockAt("mock-2", at(30)),
		config,
		mockAt("mock-4", at(120)),
		mockAt("mock-5", at(150)),
	}
	dir := t.TempDir()
	require.NoError(t, recordSetWith(t, zap.NewNop(), instr, mapdb.New(zap.NewNop(), dir, ""), &memTestDB{}, withRequests))

	data, err := os.ReadFile(filepath.Join(dir, "set", "mappings.yaml"))
	require.NoError(t, err)
	mapping, err := mapdb.DecodeMapping(data, zap.NewNop())
	require.NoError(t, err)
	byID := map[string]models.MappedTestCase{}
	for _, tc := range mapping.TestCases {
		byID[tc.ID] = tc
	}
	create := byID["orders/e2e.TestA/create"]
	require.Equal(t, []string{"test-1"}, create.Cases)
	require.Equal(t, map[string][]string{"test-1": {"mock-1"}}, create.CaseMocks)
	require.Equal(t, map[string]string{"test-1": "create"}, create.CaseSteps)
	require.Equal(t, []string{"mock-1", "mock-2"}, create.MockNames(), "mock-2 is the step's but no case's: loose")
	top := byID["orders/e2e.TestA"]
	require.Equal(t, map[string]string{"test-2": ""}, top.CaseSteps)
	require.Empty(t, top.CaseMocks)
	b := byID["orders/e2e.TestB"]
	require.Equal(t, map[string][]string{"test-3": {"mock-4"}}, b.CaseMocks)
	require.Equal(t, []string{"mock-3", "mock-4", "mock-5"}, b.MockNames())
	require.Equal(t, []string{"mock-0", "mock-3"}, mapping.StartupMockNames())
}

func TestRecordRefusesATestThatRanTwice(t *testing.T) {
	instr := newRunnerInstr(t, append(sequentialMarks(), mark("orders/e2e.TestA", 30*time.Millisecond, 40*time.Millisecond))...)
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
	cfg.Path = t.TempDir()
	dir := t.TempDir()
	store := &countingStore{}
	core, logs := observer.New(zap.DebugLevel)
	err := New(zap.New(core), instr, stubMockDB{}, mapdb.New(zap.NewNop(), dir, ""), store, nil, cfg).Record(context.Background())
	want := "Recording stopped: TestA ran 2 times in orders/e2e.\nEach test in a folder needs its own name. Rename one of them (or drop -count=2), then run keploy mock record again.\nNothing was saved."
	require.EqualError(t, err, want)
	require.ErrorIs(t, err, ErrRecordRefused)
	all := logs.All()
	require.Equal(t, want, all[len(all)-1].Message, "the refusal is the last thing printed")
	require.Equal(t, zap.ErrorLevel, all[len(all)-1].Level)
	require.Zero(t, store.pushes)
	_, statErr := os.Stat(filepath.Join(dir, "set", "mappings.yaml"))
	require.True(t, os.IsNotExist(statErr), "no mapping is written")
}

type hookSpy struct {
	rec.BaseRecordHooks
	mu       sync.Mutex
	calls    []string
	complete []rec.RecordingCompleteContext
	// stoppable says, for each AfterRecordingComplete, whether its context
	// can end (a second interrupt stops the pass through it) and has no
	// deadline.
	stoppable []bool
}

func (h *hookSpy) add(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, s)
}

func (h *hookSpy) BeforeTestCaseInsert(_ context.Context, info *rec.TestCaseContext) error {
	h.add("before-case " + info.TestSetID + " " + info.TestCase.Name)
	return nil
}

func (h *hookSpy) AfterTestCaseInsert(_ context.Context, info *rec.TestCaseContext) error {
	h.add("after-case " + info.TestSetID + " " + info.TestCase.Name)
	return nil
}

func (h *hookSpy) BeforeMockInsert(_ context.Context, info *rec.MockContext) error {
	h.add("before-mock " + info.TestSetID + " " + info.Mock.Name)
	return nil
}

func (h *hookSpy) AfterMockInsert(_ context.Context, info *rec.MockContext) error {
	h.add("after-mock " + info.TestSetID + " " + info.Mock.Name)
	return nil
}

func (h *hookSpy) AfterRecordingComplete(ctx context.Context, info *rec.RecordingCompleteContext) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.complete = append(h.complete, *info)
	_, hasDeadline := ctx.Deadline()
	h.stoppable = append(h.stoppable, ctx.Done() != nil && !hasDeadline && ctx.Err() == nil)
	return nil
}

func TestRecordPassesCasesAndMocksThroughTheHooks(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.incoming = []*models.TestCase{caseAt("test-1", runnerT0.Add(5*time.Millisecond))}
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(6*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
	cfg.Path = t.TempDir()
	withRequests(cfg)
	dir := t.TempDir()
	hooks := &hookSpy{}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), instr, stubMockDB{}, mapdb.New(zap.NewNop(), dir, ""), nil, hooks, cfg)
	svc.(TestDBSetter).SetTestDB(&memTestDB{})
	require.NoError(t, svc.Record(context.Background()))

	require.ElementsMatch(t, []string{"before-case set test-1", "after-case set test-1", "before-mock set.keploy-staging mock-0", "after-mock set.keploy-staging mock-0"}, hooks.calls)
	require.Less(t, slices.Index(hooks.calls, "before-case set test-1"), slices.Index(hooks.calls, "after-case set test-1"))
	require.Equal(t, []rec.RecordingCompleteContext{{TestSetID: "set", Path: cfg.Path}}, hooks.complete)
	// The post-record pass runs as keploy record's does
	// (rec.RunAfterRecordingHook): live, with no deadline, and under a
	// context a second interrupt can end, so a long pass can be stopped.
	require.Equal(t, []bool{true}, hooks.stoppable, "the hook's context is live, has no deadline, and can be ended")
	_, err := os.Stat(filepath.Join(dir, "set", "mappings.yaml"))
	require.NoError(t, err, "the hook ran after the mappings were written")
}

func TestCorrelateCasesPairsAMockWithACaseOfTheSameFlowAcrossEntries(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return v
	}
	windows := []models.ScopeWindow{
		{Name: "orders/stats.TestOrderLifecycle", Start: at("2026-09-29T22:35:22.100Z"), End: at("2026-09-29T22:35:22.300Z")},
		{Name: "orders/stats.TestOrderLifecycle/get", Start: at("2026-09-29T22:35:22.158Z"), End: at("2026-09-29T22:35:22.200Z")},
		{Name: "orders/stats.TestOther", Start: at("2026-09-29T22:35:22.300Z"), End: at("2026-09-29T22:35:22.400Z")},
	}
	cases := []capturedMock{{name: "get-orders-by-id-2", ts: at("2026-09-29T22:35:22.157977Z"), end: at("2026-09-29T22:35:22.15832Z")}}
	mocks := []capturedMock{
		{name: "mock-27", ts: at("2026-09-29T22:35:22.158056Z")},
		{name: "mock-28", ts: at("2026-09-29T22:35:22.35Z")},
	}
	got := correlateCases(windows, mocks, cases, nil)
	require.Equal(t, map[string]models.MappedTestCase{
		"orders/stats.TestOrderLifecycle": {
			Cases:     []string{"get-orders-by-id-2"},
			CaseMocks: map[string][]string{"get-orders-by-id-2": {"mock-27"}},
			CaseSteps: map[string]string{"get-orders-by-id-2": ""},
		},
	}, got)
	require.Equal(t, map[string][]string{"orders/stats.TestOrderLifecycle/get": {"mock-27"}, "orders/stats.TestOther": {"mock-28"}},
		func() map[string][]string {
			out := map[string][]string{}
			for k, v := range correlateScopes(windows, mocks) {
				for _, e := range v {
					out[k] = append(out[k], e.Name)
				}
			}
			return out
		}(), "mock-27 stays in the subtest's mock_entries")
}

func TestRecordAndReplayPutASetupCallInTheParentTest(t *testing.T) {
	setup := runnerT0.Add(flowSetupAt)
	instr := newRunnerInstr(t, flowMarks()...)
	instr.incoming = []*models.TestCase{httpCase("test-1", "POST", "/apps", 201, "{}", setup)}
	instr.mocks = []*models.Mock{
		mockAt("mock-0", runnerT0.Add(1500*time.Millisecond)),
		mockAt("mock-1", setup.Add(100*time.Microsecond)),
	}
	dir := t.TempDir()
	require.NoError(t, recordSetWith(t, zap.NewNop(), instr, mapdb.New(zap.NewNop(), dir, ""), &memTestDB{}, withRequests))

	data, err := os.ReadFile(filepath.Join(dir, "set", "mappings.yaml"))
	require.NoError(t, err)
	mapping, err := mapdb.DecodeMapping(data, zap.NewNop())
	require.NoError(t, err)
	require.Len(t, mapping.TestCases, 4)
	var flow models.MappedTestCase
	for _, tc := range mapping.TestCases {
		if tc.ID == "e2e/orders.TestFlow" {
			flow = tc
		}
	}
	require.Equal(t, []string{"test-1"}, flow.Cases)
	require.Equal(t, map[string]string{"test-1": ""}, flow.CaseSteps, "the call before the first subtest is the parent's")
	require.Equal(t, map[string][]string{"test-1": {"mock-1"}}, flow.CaseMocks)
	require.Equal(t, []string{"mock-1"}, flow.MockNames())
	require.Equal(t, []string{"mock-0"}, mapping.StartupMockNames(), "a call between two tests' marks belongs to no test")

	recorded := map[string][]*models.TestCase{"e2e/orders.TestFlow": {httpCase("test-1", "POST", "/apps", 201, "{}", setup)}}
	actual := []*models.TestCase{httpCase("", "POST", "/apps", 201, "{}", setup.Add(time.Millisecond))}
	out := pairCases(flowMarks(), recorded, actual, func(*models.TestCase, *models.HTTPResp) (bool, *models.Result) { return true, nil })
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Actual, "replay sees the request where record put it")
	require.True(t, out[0].Passed)
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(data)
		return err
	}))
	return out
}

func TestARefusedRecordLeavesTheSetExactlyAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		marks []models.ScopeWindow
		err   string
	}{
		{"a test ran twice", append(sequentialMarks(), mark("orders/e2e.TestA", 30*time.Millisecond, 40*time.Millisecond)), "TestA ran 2 times"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			set := filepath.Join(dir, "set")
			require.NoError(t, os.MkdirAll(filepath.Join(set, "tests"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(set, "mocks.yaml"), []byte("# old mocks\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(set, "mappings.yaml"), []byte("# old mappings\ntests: []\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(set, "tests", "test-1.yaml"), []byte("# old case\nname: test-1\n"), 0o644))
			before := snapshotTree(t, dir)

			instr := newRunnerInstr(t, tc.marks...)
			instr.incoming = []*models.TestCase{caseAt("", runnerT0.Add(5*time.Millisecond))}
			instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
			cfg.Path = dir
			withRequests(cfg)
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			svc.(TestDBSetter).SetTestDB(testdb.New(zap.NewNop(), dir))
			require.ErrorContains(t, svc.Record(context.Background()), tc.err)
			require.Equal(t, before, snapshotTree(t, dir))
		})
	}
}

// refusingTestDB is the real test-case store, whose disk refuses the second
// test case it is handed (a full disk).
type refusingTestDB struct {
	*testdb.TestYaml
	mu    sync.Mutex
	calls int
}

func (d *refusingTestDB) InsertTestCase(ctx context.Context, tc *models.TestCase, set string, enableLog bool) error {
	d.mu.Lock()
	d.calls++
	refuse := d.calls == 2
	d.mu.Unlock()
	if refuse {
		return errors.New("write the test case: no space left on device")
	}
	return d.TestYaml.InsertTestCase(ctx, tc, set, enableLog)
}

// refusesMocksDB is the real mock store, whose disk refuses every mock.
type refusesMocksDB struct{ *mockdb.MockYaml }

func (refusesMocksDB) InsertMock(context.Context, *models.Mock, string) error {
	return errors.New("write the mock: no space left on device")
}

// keploy mock record --record-requests whose test-case store refused a test
// case (a full disk): the run ends as a recording that is NOT saved, said at
// ERROR with how many of the test cases handed to the store it did not write
// and the store's first error; the set is left exactly as it was. The exit
// code is the test command's own when it failed, else keploy's 1. Before,
// the case was said at ERROR and left out, and the set was saved without
// it, exit 0: a recording short of a test case looked complete. When the
// mock store refused mocks too, the run says both.
func TestRecordRequests_ATestCaseTheStoreRefusedEndsAsARecordingNotSaved(t *testing.T) {
	for _, tc := range []struct {
		name        string
		run         models.AppError
		code        int
		refuseMocks bool
	}{
		{"the test command passed", models.AppError{}, utils.ExitKeployError, false},
		{"the test command failed with 3", models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}, 3, false},
		{"the mock store refused its mock too", models.AppError{}, utils.ExitKeployError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			set := filepath.Join(dir, "set")
			require.NoError(t, os.MkdirAll(filepath.Join(set, "tests"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(set, "mocks.yaml"), []byte("# old mocks\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(set, "tests", "test-1.yaml"), []byte("# old case\nname: test-1\n"), 0o644))
			before := snapshotTree(t, dir)

			instr := newRunnerInstr(t, sequentialMarks()...)
			instr.runResult = tc.run
			instr.incoming = []*models.TestCase{
				caseAt("", runnerT0.Add(5*time.Millisecond)),
				caseAt("", runnerT0.Add(15*time.Millisecond)),
				caseAt("", runnerT0.Add(50*time.Millisecond)),
			}
			instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
			cfg.Path = dir
			withRequests(cfg)
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			core, logs := observer.New(zapcore.InfoLevel)
			var db MockDB = mockdb.New(zap.NewNop(), dir, "")
			said := recordDrainWords.casesNotWritten
			if tc.refuseMocks {
				db, said = refusesMocksDB{mockdb.New(zap.NewNop(), dir, "")}, recordDrainWords.storeNotWritten
			}
			svc := New(zap.New(core), instr, db, mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			svc.(TestDBSetter).SetTestDB(&refusingTestDB{TestYaml: testdb.New(zap.NewNop(), dir)})
			err := svc.Record(context.Background())
			require.ErrorContains(t, err, "1 of the 3 test cases handed to the test-case store were not written")
			require.ErrorContains(t, err, "write the test case: no space left on device", "the store's own error was not said")
			if tc.refuseMocks {
				require.ErrorIs(t, err, models.ErrMocksNotWritten)
				require.ErrorContains(t, err, "1 of the 1 mocks handed to the mock store were not written")
			} else {
				require.ErrorContains(t, err, "the recording was not saved: 1 of the 3 test cases")
			}
			require.Equal(t, tc.code, utils.ErrCode)
			require.Equal(t, before, snapshotTree(t, dir), "the set was not left exactly as it was")
			require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(said).Len(), "the recording not saved was not said at ERROR")
		})
	}
}

// refusesTwoTestDB is the real test-case store, whose disk refuses the first
// and the third test case it is handed, each with its own error.
type refusesTwoTestDB struct {
	*testdb.TestYaml
	mu    sync.Mutex
	calls int
}

func (d *refusesTwoTestDB) InsertTestCase(ctx context.Context, tc *models.TestCase, set string, enableLog bool) error {
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	switch n {
	case 1:
		return errors.New("write the test case: no space left on device (the first refusal)")
	case 3:
		return errors.New("write the test case: disk quota exceeded (the second refusal)")
	}
	return d.TestYaml.InsertTestCase(ctx, tc, set, enableLog)
}

// keploy mock record --record-requests whose test-case store refused two of
// four test cases: the run counts every refusal ("2 of the 4"), and the error
// it says is the first refusal's, not a later one's. The exit code is
// keploy's 1 and the set is left exactly as it was.
func TestRecordRequests_EveryTestCaseTheStoreRefusedIsCountedAndTheFirstErrorSaid(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "set")
	require.NoError(t, os.MkdirAll(filepath.Join(set, "tests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(set, "mocks.yaml"), []byte("# old mocks\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(set, "tests", "test-1.yaml"), []byte("# old case\nname: test-1\n"), 0o644))
	before := snapshotTree(t, dir)

	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.incoming = []*models.TestCase{
		caseAt("", runnerT0.Add(5*time.Millisecond)),
		caseAt("", runnerT0.Add(15*time.Millisecond)),
		caseAt("", runnerT0.Add(30*time.Millisecond)),
		caseAt("", runnerT0.Add(50*time.Millisecond)),
	}
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(&refusesTwoTestDB{TestYaml: testdb.New(zap.NewNop(), dir)})
	err := svc.Record(context.Background())
	require.ErrorContains(t, err, "2 of the 4 test cases handed to the test-case store were not written")
	require.ErrorContains(t, err, "(the first refusal)", "the first error was not the one said")
	require.NotContains(t, err.Error(), "(the second refusal)", "a later error was said as the first")
	require.Equal(t, utils.ExitKeployError, utils.ErrCode)
	require.Equal(t, before, snapshotTree(t, dir), "the set was not left exactly as it was")
}

func TestAnAcceptedRecordReplacesTheSetAndLeavesNoCopy(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "set")
	require.NoError(t, os.MkdirAll(set, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(set, "mocks.yaml"), []byte("# old mocks\n"), 0o644))
	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
	cfg.Path = dir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg).Record(context.Background()))
	data, err := os.ReadFile(filepath.Join(set, "mocks.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(data), "# old mocks")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the set itself remains")
}

func TestRecordRequestsNeedNoAppPort(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	require.NoError(t, recordSetWith(t, zap.NewNop(), instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), &memTestDB{}, func(cfg *config.Config) { cfg.Mock.RecordRequests = true }))
	opts, read := instr.setup()
	require.True(t, opts.RecordRequests, "the tests' calls to the app are found at connect time, so no port is needed")
	require.True(t, read)
}

// Under the proven switch a record leaves what each test did against real
// infra, kept or not: a runner that failed makes the record restore the set
// (here, remove a fresh one), and the per-test results are written after
// that restore, so they survive it. With the switch off nothing is written.
// A record whose command failed leaves its per-test results, and under the
// switch keeps its recording: judging commits it test by test, keeping the
// previous recording of the test that failed. With the switch off nothing is
// kept, as before.
func TestAFailedRecordLeavesItsPerTestResults(t *testing.T) {
	for _, proven := range []bool{true, false} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
				judgedBuild(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			dir := t.TempDir()
			passed, failed := mark("orders::TestA", 0, 10*time.Millisecond), mark("orders::TestB", 10*time.Millisecond, 20*time.Millisecond)
			passed.Outcome, failed.Outcome = models.ScopeOutcomePassed, models.ScopeOutcomeFailed
			live := models.LiveRow{Test: "orders::TestA", Exe: "/usr/bin/node", Port: 3000}
			instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 1")}),
				windows: []models.ScopeWindow{passed, {Live: &live}, failed}}
			instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
			cfg.Path = dir
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			_ = svc.Record(context.Background())
			set := (&mockService{config: cfg}).setName()
			got, err := ReadRecordTestsReceipt(dir, set)
			require.NoError(t, err)
			if !proven {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got, "the per-test results were not left for judging")
			require.Equal(t, []TestReceipt{
				{Name: "orders::TestA", Outcome: models.ScopeOutcomePassed},
				{Name: "orders::TestB", Outcome: models.ScopeOutcomeFailed},
			}, got.Tests)
			require.Equal(t, []models.LiveRow{live}, got.Live, "the record's rows are left with its per-test results")
			_, err = os.Stat(filepath.Join(dir, set, "mocks.yaml"))
			require.NoError(t, err, "under the switch the recording is kept for judging to commit test by test")
		})
	}
}

// Under the switch a record that cannot read what each test did keeps
// nothing, and fails saying so; with it off the record keeps its mocks with
// no per-test windows, as before the switch existed.
func TestARecordThatCannotReadItsTestsKeepsNothing(t *testing.T) {
	for _, proven := range []bool{true, false} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			dir := t.TempDir()
			instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
			instr.windowsErr = errors.New("connection reset by peer")
			instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path = dir
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			err := svc.Record(context.Background())
			set := (&mockService{config: cfg}).setName()
			_, statErr := os.Stat(filepath.Join(dir, set, "mocks.yaml"))
			if !proven {
				require.NoError(t, err)
				require.NoError(t, statErr, "the switch off: the mocks are kept")
				return
			}
			require.ErrorContains(t, err, "keploy could not read what each test did from its agent, so nothing was kept")
			require.True(t, os.IsNotExist(statErr), "a record that could not read its tests kept a recording")
		})
	}
}

// Under the switch a record whose tests made no call keeps them, as tests
// with no mocks: judging needs their (empty) recordings. With the switch off
// the existing set is left unchanged, as before.
func TestARecordOfTestsWithoutCallsKeepsThemUnderTheSwitch(t *testing.T) {
	for _, proven := range []bool{true, false} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
				judgedBuild(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			dir := t.TempDir()
			w := mark("orders::TestPure", 0, 10*time.Millisecond)
			w.Outcome, w.Dir = models.ScopeOutcomePassed, "orders"
			instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
				windows: []models.ScopeWindow{w}}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path = dir
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			require.NoError(t, svc.Record(context.Background()))
			set := (&mockService{config: cfg}).setName()
			raw, err := os.ReadFile(filepath.Join(dir, set, "mappings.yaml"))
			if !proven {
				require.True(t, os.IsNotExist(err), "the switch off: a record without calls keeps nothing")
				return
			}
			require.NoError(t, err)
			require.Contains(t, string(raw), "orders::TestPure")
		})
	}
}

// judgedBuild stands for a build that judges what the record keeps.
func judgedBuild(t *testing.T) {
	t.Helper()
	JudgeRecords()
	t.Cleanup(func() { recordsJudged.Store(false) })
}

// Under the switch, in a build no judge runs in, a failed record keeps
// nothing, as before the switch: no one would commit it test by test, and
// the previous recording must not be lost.
func TestAFailedRecordNoOneJudgesKeepsNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	failed := mark("orders::TestB", 0, 10*time.Millisecond)
	failed.Outcome = models.ScopeOutcomeFailed
	instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 1")}),
		windows: []models.ScopeWindow{failed}}
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
	cfg.Path = dir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	_ = svc.Record(context.Background())
	set := (&mockService{config: cfg}).setName()
	_, err := os.Stat(filepath.Join(dir, set, "mocks.yaml"))
	require.True(t, os.IsNotExist(err), "a record no one judges kept a failed run's recording")
	require.NoDirExists(t, filepath.Join(dir, set), "the fresh set's folder came back")
}

// Under the proven switch a test ID that ran more than once is retried: its
// last attempt is its recording, failed if any attempt failed, and the calls
// of its earlier attempts are no test's and no pool's.
func TestFoldRetries(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{
		{Name: "o::TestA", Start: at(0), End: at(10), Outcome: models.ScopeOutcomeFailed},
		{Name: "o::TestB", Start: at(10), End: at(20), Outcome: models.ScopeOutcomePassed},
		{Name: "o::TestA", Start: at(20), End: at(30), Outcome: models.ScopeOutcomePassed},
	}
	kept, earlier, retried := foldRetries(windows)
	require.Equal(t, map[string]int{"o::TestA": 2}, retried)
	require.Len(t, kept, 2)
	require.Equal(t, "o::TestB", kept[0].Name)
	require.Equal(t, at(20), kept[1].Start, "the last attempt is the recording")
	require.Equal(t, models.ScopeOutcomeFailed, kept[1].Outcome, "failed if any attempt failed")
	require.Equal(t, []models.ScopeWindow{windows[0]}, earlier)

	mocks := []capturedMock{{name: "mock-0", ts: at(5)}, {name: "mock-1", ts: at(25)}, {name: "mock-2", ts: at(35)}}
	require.Equal(t, []models.MockEntry{{Name: "mock-2"}}, startupMocks(kept, withoutRetries(mocks, earlier), nil),
		"an earlier attempt's call is in no pool")

	_, _, none := foldRetries(windows[:2])
	require.Empty(t, none)

	// An earlier attempt's subtests go with it.
	withSubs := append([]models.ScopeWindow{}, windows...)
	withSubs = append(withSubs,
		models.ScopeWindow{Name: "o::TestA/sub", Start: at(2), End: at(8)},
		models.ScopeWindow{Name: "o::TestA/sub", Start: at(22), End: at(28)})
	kept, earlier, _ = foldRetries(withSubs)
	require.Len(t, earlier, 2)
	require.Equal(t, at(2), earlier[1].Start)
	var subs []time.Time
	for _, w := range kept {
		if w.Name == "o::TestA/sub" {
			subs = append(subs, w.Start)
		}
	}
	require.Equal(t, []time.Time{at(22)}, subs)
}

// Under the proven switch a test's mapping lists data-plane mocks only: a
// protocol-phase mock made inside a test (a token fetch, a DNS lookup, a
// MySQL ping) goes to the pool, which serves it to every test (design v14
// §5.2). Without the switch nothing is marked protocol-phase, and mappings
// are as they were.
func TestAMappingListsDataPlaneMocksOnly(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{{Name: "o::TestA", Start: at(0), End: at(100)}}
	mocks := []capturedMock{
		{name: "mock-0", ts: at(10)},
		{name: "mock-1", ts: at(20), protocol: true},
		{name: "mock-2", ts: at(30)},
	}
	require.Equal(t, map[string][]models.MockEntry{"o::TestA": {{Name: "mock-0"}, {Name: "mock-2"}}}, correlateScopes(windows, dataPlane(mocks)))
	require.Equal(t, []models.MockEntry{{Name: "mock-1"}}, startupMocks(windows, mocks, nil))
	for i := range mocks {
		mocks[i].protocol = false
	}
	require.Equal(t, map[string][]models.MockEntry{"o::TestA": {{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-2"}}}, correlateScopes(windows, dataPlane(mocks)))
	require.Empty(t, startupMocks(windows, mocks, nil))
}
