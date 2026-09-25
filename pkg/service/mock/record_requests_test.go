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
	"go.uber.org/zap"
)

// memTestDB is an in-memory test-case store that remembers what was inserted and deleted.
type memTestDB struct {
	mu       sync.Mutex
	existing []*models.TestCase
	inserted []string
	deleted  []string
}

func (db *memTestDB) InsertTestCase(_ context.Context, tc *models.TestCase, _ string, _ bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.inserted = append(db.inserted, tc.Name)
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
	require.Equal(t, []string{"test-9"}, db.deleted, "a re-record starts from an empty tests directory")
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

func TestCorrelateCasesLeavesACaseOutsideEveryWindowUnmapped(t *testing.T) {
	windows := []models.ScopeWindow{
		{Name: "TestA", Start: ts(10), End: ts(20)},
		{Name: "TestB", Start: ts(20), End: ts(30)},
	}
	cases := []capturedMock{
		{name: "test-1", ts: ts(15)},
		{name: "test-2", ts: ts(25)},
		{name: "test-3", ts: ts(35)},
		{name: "test-4", ts: ts(5)},
	}
	require.Equal(t, map[string][]string{"TestA": {"test-1"}, "TestB": {"test-2"}}, correlateCases(windows, cases))
	require.Empty(t, correlateCases(nil, cases))
}
