//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// onceServingProxy is a servingProxy whose mock manager takes the replay's
// series and once series (coreAgent.PoolSeriesSetter,
// coreAgent.PoolOnceSetter), as the agent hands them with the pool.
type onceServingProxy struct{ *servingProxy }

func (p *onceServingProxy) SetPoolSeries(series map[string][]string) error {
	p.mm.SetPoolSeries(series)
	return nil
}

func (p *onceServingProxy) SetPoolOnce(once []string) error {
	p.mm.SetPoolOnce(once)
	return nil
}

// The PAT janitor's lease is made once per process, a random few seconds
// after the application starts: its recording was placed after the test it
// landed in when recorded (test-4), and in a replay it lands in an earlier
// one (test-2). As a once series' member it answers there, once: a second
// call of it misses. A worker-scope recording placed beside it that no once
// series holds still waits for its place.
func TestAOnceLeaseAnswersInAnEarlierTestThanItsPlace(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &onceServingProxy{newServingProxy()}
	t.Cleanup(p.mm.Close)
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("w-start", "/start", "start", t0),
		pingMock("w-lease", "/lease", "lease", t0.Add(time.Second)),
		pingMock("w-later", "/later", "later", t0.Add(2*time.Second)),
	}))
	series := map[string][]string{"pat-janitor-lease/-/1": {"w-lease"}}
	once := []string{"pat-janitor-lease/-/1"}
	require.NoError(t, a.SetScopeSeries(ctx, series))
	require.NoError(t, a.SetScopeOnce(ctx, once))
	// As SetScopePool hands them on: the once series first.
	require.NoError(t, p.SetPoolOnce(once))
	require.NoError(t, p.SetPoolSeries(series))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true, Order: []string{"test-1", "test-2", "test-3", "test-4"}, Worker: []models.MockEntry{
			{Name: "w-start"},
			{Name: "w-lease", Place: "test-4"},
			{Name: "w-later", Place: "test-4"},
		}},
	}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2"}}))
	code, _ := p.call(t, "/lease")
	require.Equal(t, 200, code, "the once lease answers in a test before its place")
	code, _ = p.call(t, "/lease")
	require.NotEqual(t, 200, code, "the once lease answers once")
	code, _ = p.call(t, "/later")
	require.NotEqual(t, 200, code, "a recording no once series holds waits for its place")
	code, _ = p.call(t, "/start")
	require.Equal(t, 200, code, "the set's start answers")
}
