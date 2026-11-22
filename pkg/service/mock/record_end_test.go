package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A record under the proven switch writes into each test's mapping when its
// scope began and ended (Record -> stampBegins, stampEnds), so a reader of
// the recording has each test's recorded duration; without the switch it
// writes neither, as before.
func TestARecordKeepsEachTestsBeginAndEnd(t *testing.T) {
	for _, proven := range []bool{true, false} {
		t.Run(map[bool]string{true: "proven", false: "switch off"}[proven], func(t *testing.T) {
			if proven {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			dir := t.TempDir()
			a := mark("orders::TestA", 0, 10*time.Millisecond)
			a.Outcome, a.Dir = models.ScopeOutcomePassed, "tests"
			b := mark("orders::TestB", 20*time.Millisecond, 45*time.Millisecond)
			b.Outcome, b.Dir = models.ScopeOutcomePassed, "tests"
			instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
				windows: []models.ScopeWindow{a, b}}
			instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path = dir
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			require.NoError(t, New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg).Record(context.Background()))
			set := (&mockService{config: cfg}).setName()
			mp, err := mapdb.New(zap.NewNop(), dir, "").GetMapping(context.Background(), set)
			require.NoError(t, err)
			require.NotNil(t, mp)
			byID := map[string]models.MappedTestCase{}
			for _, tc := range mp.TestCases {
				byID[tc.ID] = tc
			}
			for _, w := range []models.ScopeWindow{a, b} {
				got, ok := byID[w.Name]
				require.True(t, ok, "%s has no mapping entry: %+v", w.Name, mp.TestCases)
				if !proven {
					require.True(t, got.At.IsZero() && got.End.IsZero(), "with the switch off %s got a begin or an end: %+v", w.Name, got)
					continue
				}
				require.True(t, got.At.Equal(w.Start), "%s began at %v, want %v", w.Name, got.At, w.Start)
				require.True(t, got.End.Equal(w.End), "%s ended at %v, want %v", w.Name, got.End, w.End)
			}
		})
	}
}
