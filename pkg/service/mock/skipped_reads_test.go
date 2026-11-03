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

// gatedInstr is a composeInstr that takes the proven gate (ScopeSkipPusher).
type gatedInstr struct {
	*composeInstr
	skip map[string]string
}

func (g *gatedInstr) PushScopeSkips(_ context.Context, skip map[string]string, _ map[string][]models.ScopeNeed) error {
	g.skip = skip
	return nil
}

func httpMock(name, method string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: name,
		Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: models.Method(method), URL: "http://localhost:8200/v1/secret/data/pat"}}}
}

// A read that a test the gate skips recorded (the app made it once and kept
// the answer: a token it looks up and caches) answers whichever test makes it
// once that test is skipped: it is the worker scope's for the replay, under
// the skipped test's key, at most once and required of no test. Without it
// the read misses in the next test's window, which is set aside in turn, and
// the read moves on to the test after it until no test is kept. The skipped
// test's write stays its own, served to no test, and a test that runs keeps
// its reads.
func TestASkippedTestsReadAnswersWhicheverTestMakesIt(t *testing.T) {
	provenOn(t)
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n        - name: mock-put\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-own\nworker:\n    - name: mock-bg\n      key: o\n"), 0o644))
	instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET"), httpMock("mock-bg", "POST")}}
	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

	require.Equal(t, map[string]string{"o::TestCRUD": "flaky: flipped"}, instr.skip, "the gate is pushed as read")
	require.Equal(t, []string{"mock-put"}, instr.table["o::TestCRUD"], "the skipped test's write stays its own")
	require.Equal(t, []string{"mock-own"}, instr.table["o::TestList"], "a test that runs keeps its reads")
	require.Equal(t, map[string][]string{"o": {"mock-bg", "mock-pat"}}, instr.worker, "the skipped test's read answers whichever test makes it, beside worker:")
	require.ElementsMatch(t, []string{"mock-bg", "mock-pat"}, instr.pool["o"])

	// A replay that skips no test serves the read to its own test alone.
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return nil, nil, nil
	})
	instr2 := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg2 := instrConfig(instr2.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg2.Path, cfg2.Mock.OnMiss = keployDir, "fail"
	require.NoError(t, New(zap.NewNop(), instr2, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg2).Replay(context.Background()))
	require.Equal(t, []string{"mock-pat", "mock-put"}, instr2.table["o::TestCRUD"])
	require.Equal(t, map[string][]string{"o": {"mock-bg"}}, instr2.worker)
}

// readOnly knows a read by its HTTP method alone; a call of any other
// protocol is not known to read only.
func TestReadOnlyKnowsAnHTTPReadByItsMethod(t *testing.T) {
	for _, m := range []string{"GET", "get", "HEAD", "OPTIONS"} {
		require.True(t, readOnly(httpMock("m", m)), m)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", ""} {
		require.False(t, readOnly(httpMock("m", m)), m)
	}
	require.False(t, readOnly(&models.Mock{Kind: models.Mongo, Name: "m", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET"}}}), "a request of another kind is not read as HTTP")
	require.False(t, readOnly(&models.Mock{Kind: models.HTTP, Name: "m"}), "an HTTP mock with no request is not known to read")
	require.False(t, readOnly(nil))
}
