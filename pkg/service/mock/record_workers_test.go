package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// A judged record's per-test file says how many test workers the record's
// test processes declared at their claims, the most any declared: whatever
// judges the record and replays one test worker at a time then knows the
// record's own command ran more. A record of one test process says none.
// Before, the file said nothing of it, and a judge reran the command's
// workers in a round that refuses more than one.
func TestAJudgedRecordSaysHowManyTestWorkersItsClaimsDeclared(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []models.ScopeWindow
		want   int
	}{
		{"two workers of four", []models.ScopeWindow{
			{Claim: "orders", Start: runnerT0, End: runnerT0, Worker: 1, Workers: 4},
			{Claim: "billing", Start: runnerT0.Add(time.Millisecond), End: runnerT0.Add(time.Millisecond), Worker: 2, Workers: 4},
		}, 4},
		{"one test process", []models.ScopeWindow{{Claim: "orders", Start: runnerT0, End: runnerT0}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			judgedBuild(t)
			dir := t.TempDir()
			a := mark("orders::TestA", 2*time.Millisecond, 10*time.Millisecond)
			a.Outcome, a.Dir = models.ScopeOutcomePassed, "orders"
			b := mark("billing::TestB", 10*time.Millisecond, 20*time.Millisecond)
			b.Outcome, b.Dir = models.ScopeOutcomePassed, "billing"
			runner := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
				windows: append(append([]models.ScopeWindow{}, tc.claims...), a, b)}
			runner.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond)), mockAt("mock-1", runnerT0.Add(15*time.Millisecond))}
			instr := &lossRunner{runnerInstr: runner, lossCounter: &lossCounter{}}
			cfg := instrConfig(runner.composeInstr, utils.Native, "go test -p 4 -json ./...")
			cfg.Path = dir
			set := (&mockService{config: cfg}).setName()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, set), 0o755))
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			require.NoError(t, svc.Record(context.Background()))
			got, err := ReadRecordTestsReceipt(dir, set)
			require.NoError(t, err)
			require.NotNil(t, got, "no per-test file was written")
			require.Len(t, got.Tests, 2)
			require.Equal(t, tc.want, got.Workers)
		})
	}
}

// claimedWorkers is the most any claim declared, wherever that claim is.
func TestClaimedWorkersIsTheMostAnyClaimDeclared(t *testing.T) {
	require.Equal(t, 0, claimedWorkers(nil))
	require.Equal(t, 3, claimedWorkers([]models.ScopeWindow{{Claim: "a", Workers: 3}, {Claim: "b", Workers: 2}}))
	require.Equal(t, 3, claimedWorkers([]models.ScopeWindow{{Claim: "a", Workers: 2}, {Claim: "b", Workers: 3}}))
}
