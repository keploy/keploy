package mock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Under the switch keploy mock replay's per-test receipt names, on a test's
// miss that is a recording another test owns (NotOwned, ClosestMock), that
// test (recorded_in), as keploy test's replay does: judging lists it as the
// root of the test's entry. A recording no test owns (the pool's) names
// none, nor does one the test owns itself, nor a miss that is not the
// recording it is closest to (a changed request). With the switch off no
// per-test table is pushed and the receipt names none, whatever the agent's
// windows say.
func TestTheTestsReceiptNamesTheTestThatRecordedAMiss(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			if on {
				provenOn(t)
			}
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-1\n"), 0o644))
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
			misses := []models.UnmatchedCall{
				{Protocol: "HTTP", ActualSummary: "GET /a", Destination: "api:80", NotOwned: true, ClosestMock: "mock-0"},
				{Protocol: "HTTP", ActualSummary: "GET /pool", Destination: "api:80", NotOwned: true, ClosestMock: "mock-2"},
				{Protocol: "HTTP", ActualSummary: "GET /own", Destination: "api:80", NotOwned: true, ClosestMock: "mock-1"},
				{Protocol: "HTTP", ActualSummary: "GET /changed", Destination: "api:80", ClosestMock: "mock-0"},
			}
			instr.windows = []models.ScopeWindow{
				{Name: "o::TestA", Outcome: models.ScopeOutcomePassed},
				{Name: "o::TestB", Outcome: models.ScopeOutcomeFailed, Evidence: &models.ScopeEvidence{Misses: misses}},
			}
			cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			core, logs := observer.New(zap.WarnLevel)
			_ = New(zap.New(core), instr, named("mock-0", "mock-1", "mock-2"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
			tests, err := ReadTestsReceipt(keployDir, "set")
			require.NoError(t, err)
			require.NotNil(t, tests, "the replay writes what each test did")
			var got []string
			for _, tr := range tests.Tests {
				if tr.Evidence == nil {
					continue
				}
				for _, c := range tr.Evidence.Misses {
					got = append(got, tr.Name+" "+c.ActualSummary+" -> "+c.RecordedIn)
				}
			}
			said := logs.FilterMessageSnippet("tests made a call that another test recorded")
			if !on {
				require.Len(t, got, len(misses), "the windows' misses are on the receipt")
				for _, line := range got {
					require.True(t, strings.HasSuffix(line, " -> "), "with the switch off the receipt names no test: %s", line)
				}
				require.Zero(t, said.Len(), "with the switch off the replay says nothing of another test's recording")
				return
			}
			require.Equal(t, []string{
				"o::TestB GET /a -> o::TestA",
				"o::TestB GET /pool -> ",
				"o::TestB GET /own -> ",
				"o::TestB GET /changed -> ",
			}, got, "only the miss that is another test's recording names that test")
			require.Equal(t, 1, said.Len(), "the replay says once which calls another test recorded")
			require.Equal(t, []any{"o::TestB: GET /a (mock-0, recorded in o::TestA)"}, said.All()[0].ContextMap()["calls"])
			// The agent's windows are read, never written: the name is on the
			// receipt's own copy.
			require.Empty(t, instr.windows[1].Evidence.Misses[0].RecordedIn)
		})
	}
}
