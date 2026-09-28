package mock

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
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
		instr := newRunnerInstr(t, "")
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
	instr := newRunnerInstr(t, jsonSequential)
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

func TestRecordRequestsNeedsATestStore(t *testing.T) {
	instr := newRunnerInstr(t, "")
	err := recordSetWith(t, zap.NewNop(), instr, nil, nil, withRequests)
	require.ErrorContains(t, err, "test-case store")
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
	require.Equal(t, []models.MockEntry{{Name: "mock-0"}, {Name: "mock-2"}, {Name: "mock-4"}}, startupMocks(windows, mocks))
	require.Len(t, startupMocks(nil, mocks), 5)
}

const jsonSteps = `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"run","Package":"orders/e2e","Test":"TestA/create"}
{"Time":"2026-09-24T10:00:00.04Z","Action":"pass","Package":"orders/e2e","Test":"TestA/create"}
{"Time":"2026-09-24T10:00:00.1Z","Action":"pass","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.1Z","Action":"run","Package":"orders/e2e","Test":"TestB"}
{"Time":"2026-09-24T10:00:00.2Z","Action":"pass","Package":"orders/e2e","Test":"TestB"}
`

func TestRecordWritesCaseMocksStepsAndStartup(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	instr := newRunnerInstr(t, jsonSteps)
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
	instr := newRunnerInstr(t, jsonSequential+`{"Time":"2026-09-24T10:00:00.03Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.04Z","Action":"pass","Package":"orders/e2e","Test":"TestA"}
`)
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -json ./...")
	cfg.Path = t.TempDir()
	dir := t.TempDir()
	store := &countingStore{}
	err := New(zap.NewNop(), instr, stubMockDB{}, mapdb.New(zap.NewNop(), dir, ""), store, nil, cfg).Record(context.Background())
	require.EqualError(t, err, "TestA ran 2 times in orders/e2e; test names must be unique within a folder (check -count, or package e2e and e2e_test both defining it)")
	require.Zero(t, store.pushes)
	_, statErr := os.Stat(filepath.Join(dir, "set", "mappings.yaml"))
	require.True(t, os.IsNotExist(statErr), "no mapping is written")
}
