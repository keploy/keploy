//go:build linux

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// seriesProxy is wireProxy that serves the series of the recordings a
// declared rule placed (coreAgent.PoolSeriesSetter).
type seriesProxy struct {
	*wireProxy
	series map[string][]string
	calls  int
	err    error
}

var _ coreAgent.PoolSeriesSetter = (*seriesProxy)(nil)

func (p *seriesProxy) SetPoolSeries(series map[string][]string) error {
	if p.err != nil {
		return p.err
	}
	p.series, p.calls = series, p.calls+1
	return nil
}

// The table's series reach the proxy when the pool slices are served
// (SetScopePool), as the CLI sent them; a table with none clears the
// previous set's. A proxy that cannot serve them refuses series, and takes a
// table without any; one that fails to install them fails the pool.
func TestTheScopeSeriesReachTheProxyWithThePool(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &seriesProxy{wireProxy: wp}
	a.Proxy = p
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	sent := map[string][]string{"r/o::TestA/1": {"mock-2"}}
	require.NoError(t, a.SetScopeSeries(ctx, sent))
	sent["r/o::TestA/1"][0] = "mock-changed" // the caller's map is its own
	require.Zero(t, p.calls, "the series go to the proxy with the pool, not before")
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Equal(t, map[string][]string{"r/o::TestA/1": {"mock-2"}}, p.series)
	require.NoError(t, a.SetScopeSeries(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Empty(t, p.series, "a table with no series leaves the set none")
	require.Equal(t, 2, p.calls)
	p.err = errors.New("no mock manager")
	require.Error(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}), "a proxy that refuses the series fails the pool")
	p.err = nil

	// A wait for the wire that ends with its caller installs nothing.
	require.NoError(t, a.wire.lock(ctx))
	t.Cleanup(func() {
		select {
		case <-a.wire.ch:
		default:
		}
	})
	gone, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, a.SetScopeSeries(gone, map[string][]string{"r/o::TestA/1": {"mock-2"}}))
	a.provenMu.Lock()
	require.Empty(t, a.proven.series)
	a.provenMu.Unlock()

	b, _ := wireAgent(t, models.MODE_TEST)
	require.Error(t, b.SetScopeSeries(ctx, map[string][]string{"r/o::TestA/1": {"mock-2"}}), "a proxy that cannot serve series refuses them")
	require.NoError(t, b.SetScopeSeries(ctx, map[string][]string{}), "and takes a table with none")
}
