package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// x2Mappings is the shape of the m1c-x2 record's mappings.yaml
// (r24/runs/m1c-x2/out/staged-set/.../api-server-e2e/mappings.yaml), cut to
// the recordings this file needs: TestGetCurrentUser lists its own call
// (mock-3734); startup: lists the app's first call (mock-0); worker: lists
// the schema reconcile poll recorded twice after the last test (mock-3733,
// mock-3735) and mock-463, a clean-up delete that trails
// TestListAppsPagination. Beside them: TestCRUD, which the replay's gate
// skips, lists a read (mock-pat) and a write (mock-put), and worker: also
// lists mock-dup, which TestGetCurrentUser lists too.
const x2Mappings = `version: api.keploy.io/v1beta1
kind: TestMocksMapping
test_set_id: set
tests:
    - id: .::TestGetCurrentUser
      dir: .
      key: .
      mock_entries:
        - name: mock-3734
        - name: mock-dup
    - id: .::TestListAppsPagination
      dir: .
      key: .
      mock_entries:
        - name: mock-326
    - id: .::TestCRUD
      dir: .
      key: .
      mock_entries:
        - name: mock-pat
        - name: mock-put
startup:
    - name: mock-0
      kind: Mongo
      key: .
worker:
    - name: mock-463
      kind: Mongo
      key: .
      trails: .::TestListAppsPagination
    - name: mock-3733
      kind: Mongo
      key: .
    - name: mock-3735
      kind: Mongo
      key: .
    - name: mock-dup
      kind: Mongo
      key: .
`

// keploy mock replay names the set's background recordings to the agent
// (models.ScopeTableReq.Background), as keploy test's staging does
// (models.Mapping.BackgroundMockNames): worker:, then startup:. Before, the
// table named none, so no miss of a keploy mock run was ever marked
// background (TRACE-M2 section 0 point 2). A read this replay moved to
// worker: from the test its gate skips (mock-pat) is a test's call, never
// the application's own work, and is not one; nor is a worker: entry a test
// lists too (mock-dup), which is that test's. A replay whose gate skips
// nothing names the same background.
func TestKeployMockReplayNamesTheSetsBackgroundRecordings(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(x2Mappings), 0o644))
	mongo := func(name string) *models.Mock {
		return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.Mongo, Name: name}
	}
	db := namedMockDB{mocks: []*models.Mock{mongo("mock-0"), mongo("mock-326"), mongo("mock-463"), mongo("mock-3733"), mongo("mock-3734"), mongo("mock-3735"), mongo("mock-dup"), httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT")}}
	for _, skip := range []map[string]string{{".::TestCRUD": "flaky: flipped"}, nil} {
		RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
			return skip, nil, nil
		})
		t.Cleanup(func() { RegisterScopeSkipSource(nil) })
		instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
		cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
		cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = 0 })
		require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

		require.Equal(t, []string{"mock-463", "mock-3733", "mock-3735", "mock-0"}, instr.background,
			"the table names the set's worker: and startup: recordings, not a skipped test's read nor a recording a test lists (gate skips %v)", skip)
		if skip != nil {
			require.Equal(t, map[string]string{"mock-pat": ".::TestCRUD"}, instr.skippedReads, "the skipped test's read is served from worker:")
		}
	}
}

// A set with no mappings file, or one that lists neither worker: nor
// startup:, names no background recording: every miss stays its test's.
func TestBackgroundNamesOfASetWithNone(t *testing.T) {
	require.Nil(t, backgroundNames(nil, map[string][]string{".": {"mock-0"}}))
	require.Nil(t, backgroundNames(&models.Mapping{}, map[string][]string{".": {"mock-0"}}))
	require.Nil(t, backgroundNames(&models.Mapping{Worker: []models.MockEntry{{Name: "mock-gone"}}}, map[string][]string{".": {"mock-0"}}), "a worker: name no pool slice holds is no background recording")
}
