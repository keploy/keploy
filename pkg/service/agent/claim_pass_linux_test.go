//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// passServingProxy is servingProxy, the real mock manager behind exact
// serving, handed the retry cycles as the agent's proxy is
// (coreAgent.ConsumedRewindsSeeder).
type passServingProxy struct{ *servingProxy }

func (p passServingProxy) SeedConsumedRewinds(n uint64) { p.mm.SeedConsumedRewinds(n) }

// notifyKey is one recorded occurrence of an in-answer notification, as the
// Postgres parser claims it (integrations.MockMemDb.ClaimOnce) each time it
// serves the answer that carries it.
const notifyKey = "inv-1\x00mock-pool\x000"

// passFilters is what keploy test sends the agent for a set, in one switch:
// the set's staging, and a test case's opening (and, under exact serving, the
// pool served again as it ends), each carrying the retry cycles the set's
// consumed history was rewound for.
type passFilters struct {
	mm      *proxyPkg.MockManager
	stage   func()
	open    func(name string, at time.Time, rewinds uint64)
	between func(rewinds uint64)
}

// A test run again by a retry cycle (--retry-passing-test) is a new pass over
// the set, so it is delivered, once, a notification its recording carries in
// an answer, as in the first cycle; the deferred streaming tests after the
// last cycle are the same pass, so nothing is delivered twice. Through the
// agent onto the real mock manager, in both switches: under exact serving the
// cycle reaches the manager only because the exact filter's rewinds are
// handed to it (seedConsumedRewinds).
func TestARetryCycleDeliversARecordedNotificationAgainAndTheStreamingTailDoesNot(t *testing.T) {
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		filters func(t *testing.T) passFilters
	}{
		{"switch off: test windows", func(t *testing.T) passFilters {
			t.Setenv(provenswitch.Env, "")
			mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.ResetForReplaySession()
			a := residentAgent(&mgrProxy{mm: mm}, nil, nil)
			return passFilters{
				mm: mm,
				stage: func() {
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}))
				},
				open: func(_ string, at time.Time, rewinds uint64) {
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: at, BeforeTime: at.Add(time.Second), ConsumedRewinds: rewinds}))
				},
				between: func(uint64) {},
			}
		}},
		{"switch on: exact serving", func(t *testing.T) passFilters {
			a, _ := wireAgent(t, models.MODE_TEST)
			p := passServingProxy{newServingProxy()}
			a.Proxy = p
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-pool", "/q", "rows", base.Add(-time.Second))}))
			return passFilters{
				mm: p.mm,
				stage: func() {
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
						AfterTime: models.BaseTime, BeforeTime: time.Now(),
						Exact: &models.ExactServing{Stage: true, Pool: []string{"mock-pool"}},
					}))
				},
				open: func(name string, _ time.Time, rewinds uint64) {
					// The pool is served between test cases, with the
					// cycle's count, before and after each one opens.
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}, ConsumedRewinds: rewinds}))
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: name}, ConsumedRewinds: rewinds}))
				},
				between: func(rewinds uint64) {
					require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}, ConsumedRewinds: rewinds}))
				},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filters(t)
			serve := func() bool { return f.mm.ClaimOnce(notifyKey) }

			f.stage()
			f.open("test-1", base, 0)
			require.True(t, serve(), "the first cycle's test-1 is not delivered the notification")
			require.False(t, serve(), "the answer served again in the same cycle delivered the notification twice")
			f.between(0)
			require.False(t, serve(), "the pool served after test-1 delivered the notification again")
			f.open("test-2", base.Add(2*time.Second), 0)
			require.False(t, serve(), "test-2, in the same cycle, was delivered test-1's notification again")

			// Retry cycles: test-1 again, each with one rewind more (keploy
			// runs up to three cycles; four retries here).
			for cycle := uint64(1); cycle <= 4; cycle++ {
				f.open("test-1", base, cycle)
				require.True(t, serve(), "retry cycle %d's test-1 missed the notification its recording carries", cycle+1)
				require.False(t, serve(), "retry cycle %d delivered the notification twice", cycle+1)
				f.between(cycle)
				require.False(t, serve(), "retry cycle %d: the pool served after test-1 delivered the notification again", cycle+1)
			}

			// The deferred streaming tests run after the last cycle, back at
			// an earlier window, with the last cycle's count.
			f.open("test-stream", base.Add(-time.Minute), 4)
			require.False(t, serve(), "the deferred streaming tail delivered the notification a second time in one pass")

			// Another attempt of the set stages it again: a new pass.
			f.stage()
			f.open("test-1", base, 0)
			require.True(t, serve(), "the next staging's test-1 is not delivered the notification")
		})
	}
}

// Without retry cycles, a set whose streaming tests run last delivers a
// recorded notification once: the tail goes back to an earlier window, which
// starts no pass, in both switches.
func TestTheStreamingTailAloneDoesNotDeliverARecordedNotificationTwice(t *testing.T) {
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	t.Run("switch off: test windows", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		a := residentAgent(&mgrProxy{mm: mm}, nil, nil)
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: base.Add(time.Minute), BeforeTime: base.Add(time.Minute + time.Second)}))
		require.True(t, mm.ClaimOnce(notifyKey))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: base, BeforeTime: base.Add(time.Second)}))
		require.False(t, mm.ClaimOnce(notifyKey), "the streaming tail's earlier window delivered the notification twice")
	})
	t.Run("switch on: exact serving", func(t *testing.T) {
		a, _ := wireAgent(t, models.MODE_TEST)
		p := passServingProxy{newServingProxy()}
		a.Proxy = p
		require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-pool", "/q", "rows", base.Add(-time.Second))}))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), Exact: &models.ExactServing{Stage: true, Pool: []string{"mock-pool"}}}))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}))
		require.True(t, p.mm.ClaimOnce(notifyKey))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-stream"}}))
		require.False(t, p.mm.ClaimOnce(notifyKey), "the streaming tail delivered the notification twice")
	})
}
