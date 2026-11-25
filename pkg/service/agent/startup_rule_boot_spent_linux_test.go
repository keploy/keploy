//go:build linux

package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A declared startup: true background rule moves a createIndexes recorded
// at @startup out of mappings.yaml's startup: into worker:, as a once series
// placed at the set's start, required once per run:
//
//	worker:
//	  - name: mock-9
//	    rule: startup-indexes@0123abcd
//	    series: startup-indexes/-/1
//	    once: true
//
// For that mapping keploy mock replay sends the agent the table below in
// both KEPLOY_PROVEN modes (pushScopeTable: poolSlices, workerSlices,
// backgroundNames, workerSeries, onceSeries, hasDeclaredStartupRule), which
// the agent's scope-table route applies as applyScopeTable does here.
//
// The application makes that createIndexes once per process. Answered at its
// boot, with no test open, the recording is spent for the run: the identical
// call inside a test's window is one call more than the application made,
// and the recording does not answer it again, in either mode. A worker:
// recording of no once series still answers a test's window after it
// answered outside every test, as before: the windows and the calls outside
// them read the worker scope apart.
func TestAStartupRuleRecordingAnsweredAtBootIsNotAnsweredAgainInATest(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	const (
		bootName = "mock-9"
		series   = "startup-indexes/-/1"
	)
	tableWith := func(once []string) models.ScopeTableReq {
		return models.ScopeTableReq{
			Mappings: map[string][]string{".::TestAppCRUD": {"mock-0"}},
			Windows: []models.TestWindow{
				{TestCase: ".::TestAppCRUD", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
			},
			Pool:         map[string][]string{models.PoolKeyNone: {bootName}},
			Worker:       map[string][]string{models.PoolKeyNone: {bootName}},
			Background:   []string{bootName},
			Series:       map[string][]string{series: {bootName}},
			Once:         once,
			StartupRules: true,
		}
	}

	for _, mode := range []struct {
		name   string
		proven string
	}{
		{"KEPLOY_PROVEN_unset", ""},
		{"KEPLOY_PROVEN=1", "1"},
	} {
		for _, tc := range []struct {
			name     string
			once     []string
			answered bool
			// mark: the boot consume goes through MarkMockAsUsed, a door
			// that does not evict (the Mongo parser's use mark), not
			// DeleteFilteredMock; with the switch off only.
			mark bool
		}{
			{"once_series_spent_at_boot", []string{series}, false, false},
			{"once_series_spent_at_boot_through_a_door_that_does_not_evict", []string{series}, false, true},
			{"plain_worker_answers_the_windows_apart", nil, true, false},
		} {
			if tc.mark && mode.proven == "1" {
				continue
			}
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				a, _ := wireAgent(t, models.MODE_TEST)
				t.Setenv(provenswitch.Env, mode.proven)
				require.Equal(t, mode.proven == "1", provenswitch.On())
				p := &onceServingProxy{newServingProxy()}
				t.Cleanup(p.mm.Close)
				a.Proxy = p

				boot := mongoIndexMock(bootName, "createIndexes", "test_case_audits", "from-boot", base)
				own := mongoIndexMock("mock-0", "find", "apps", "from-own", base.Add(2*time.Second))
				require.NoError(t, a.StoreMocks(ctx, []*models.Mock{boot, own}, nil))
				applyScopeTable(t, a, tableWith(tc.once))

				if mode.proven == "1" {
					_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
					require.Nil(t, serr)
				} else {
					// The replay's first staging, before any test (replay.go).
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}))
				}

				// Boot: the application's createIndexes, no test open,
				// answered as the Mongo parser answers it: by its background
				// steps where the store serves the set's background there
				// (s12Serving), else by the boot phase's own matching, which
				// reads the start-up tier and consumes through
				// DeleteFilteredMock (falling through to it).
				var answered *models.Mock
				if p.mm.ExactServing() || integrations.S12Background(p.mm) {
					snap, ok := integrations.SnapshotRound(p.mm, integrations.RoundQuery{Kind: models.Mongo})
					require.True(t, ok)
					answered = mockNamed(snap.BackgroundUnused, bootName)
					require.NotNil(t, answered, "the createIndexes recording answers the application's boot")
					require.True(t, integrations.WithinRound(p.mm, snap.At).DeleteFilteredMock(*answered))
				} else {
					startup, err := p.mm.GetStartupMocks()
					require.NoError(t, err)
					answered = mockNamed(startup, bootName)
					require.NotNil(t, answered, "the createIndexes recording answers the application's boot")
					if tc.mark {
						require.True(t, p.mm.MarkMockAsUsed(*answered))
					} else {
						require.True(t, p.mm.DeleteFilteredMock(*answered))
					}
				}

				// A test opens, and its window makes the identical createIndexes.
				if mode.proven == "1" {
					_, serr := a.ProvenBegin(ctx, models.ScopeReq{Name: "TestAppCRUD", CanSkip: true})
					require.Nil(t, serr)
				} else {
					require.NoError(t, a.BeginScope(ctx, ".::TestAppCRUD", 0))
				}
				require.True(t, integrations.S12Background(p.mm), "the test's window is served the set's background")

				snap, ok := integrations.SnapshotRound(p.mm, integrations.RoundQuery{Kind: models.Mongo})
				require.True(t, ok)
				take := integrations.WithinRound(p.mm, snap.At)
				if tc.answered {
					require.NotNil(t, mockNamed(p.mm.Background(models.Mongo), bootName),
						"a worker: recording of no once series is shown to a test's window after it answered outside every test")
					require.NotNil(t, mockNamed(snap.BackgroundUnused, bootName))
					require.True(t, take.DeleteFilteredMock(*answered),
						"a worker: recording of no once series answers a test's window after it answered outside every test")
					return
				}
				require.Nil(t, mockNamed(p.mm.Background(models.Mongo), bootName),
					"a once series' recording answered at boot is not shown again to a test's window")
				require.Nil(t, mockNamed(snap.BackgroundUnused, bootName),
					"a once series' recording answered at boot is not unused in a test's window")
				require.False(t, take.DeleteFilteredMock(*answered),
					"a once series' recording answered at boot must not answer the identical call in a test's window a second time")
				require.False(t, take.DeleteStartupMock(*answered),
					"a once series' recording answered at boot must not answer the identical call in a test's window a second time")
			})
		}
	}
}

// applyScopeTable applies req to a as the agent's scope-table route
// (routes.HandleScopeTable) applies the table keploy mock replay pushes, in
// its order: under the switch the proven wire's fields, and with it off,
// for a set with background recordings, its background serving alone (the
// worker scope, its series and once series, the background names, the
// pool).
func applyScopeTable(t *testing.T, a *Agent, req models.ScopeTableReq) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, req.Mappings))
	require.NoError(t, a.SetRunPlaceWindows(ctx, req.Windows))
	require.NoError(t, a.SetScopeStartupRules(ctx, req.StartupRules))
	if !provenswitch.On() {
		if len(req.Background) > 0 {
			require.NoError(t, a.SetScopeWorker(ctx, req.Worker))
			require.NoError(t, a.SetScopeSeries(ctx, req.Series))
			require.NoError(t, a.SetScopeOnce(ctx, req.Once))
			require.NoError(t, a.SetScopeBackground(ctx, req.Background))
			require.NoError(t, a.SetScopePool(ctx, req.Pool))
		}
		return
	}
	require.NoError(t, a.SetRecordedWindows(ctx, req.Windows))
	require.NoError(t, a.SetScopeWorker(ctx, req.Worker))
	require.NoError(t, a.SetScopeSeries(ctx, req.Series))
	require.NoError(t, a.SetScopeOnce(ctx, req.Once))
	require.NoError(t, a.SetScopeBackground(ctx, req.Background))
	require.NoError(t, a.SetScopePool(ctx, req.Pool))
}

func mockNamed(mocks []*models.Mock, name string) *models.Mock {
	for _, mk := range mocks {
		if mk != nil && mk.Name == name {
			return mk
		}
	}
	return nil
}
