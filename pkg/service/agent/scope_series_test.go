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

// repeatProxy is seriesProxy that counts the calls each series answered
// past its recorded count (coreAgent.SeriesRepeatReader).
type repeatProxy struct {
	*seriesProxy
	repeats map[string]int64
	err     error
}

var _ coreAgent.SeriesRepeatReader = (*repeatProxy)(nil)

func (p *repeatProxy) SeriesRepeats() (map[string]int64, error) {
	return p.repeats, p.err
}

// The mock stats carry the proxy's count of each series' repeats, with
// SeriesCounted set. A proxy that cannot count them, or whose read fails
// (no mock manager yet), leaves SeriesCounted false and never fails the
// stats: the loaded count the compose bring-up checks stays read.
func TestTheMockStatsCarryTheSeriesRepeats(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"))
	p := &repeatProxy{seriesProxy: &seriesProxy{wireProxy: wp}, repeats: map[string]int64{"r/o::TestA/1": 3}}
	a.Proxy = p
	stats, err := a.MockStats(context.Background())
	require.NoError(t, err)
	require.True(t, stats.SeriesCounted)
	require.Equal(t, map[string]int64{"r/o::TestA/1": 3}, stats.SeriesRepeats)
	loaded := stats.Loaded

	p.err = errors.New("no mock manager")
	stats, err = a.MockStats(context.Background())
	require.NoError(t, err)
	require.False(t, stats.SeriesCounted)
	require.Nil(t, stats.SeriesRepeats)
	require.Equal(t, loaded, stats.Loaded)

	a.Proxy = p.seriesProxy
	stats, err = a.MockStats(context.Background())
	require.NoError(t, err)
	require.False(t, stats.SeriesCounted)
	require.Equal(t, loaded, stats.Loaded)
}

// onceProxy is seriesProxy that also serves once series
// (coreAgent.PoolOnceSetter), noting the order the pool hands them.
type onceProxy struct {
	*seriesProxy
	once  []string
	order []string
	err   error
}

var _ coreAgent.PoolOnceSetter = (*onceProxy)(nil)

func (p *onceProxy) SetPoolOnce(once []string) error {
	if p.err != nil {
		return p.err
	}
	p.once, p.order = once, append(p.order, "once")
	return nil
}

func (p *onceProxy) SetPoolSeries(series map[string][]string) error {
	p.order = append(p.order, "series")
	return p.seriesProxy.SetPoolSeries(series)
}

// The table's once series reach the proxy with the pool (SetScopePool),
// before the series (the index the proxy builds reads them), as the CLI
// sent them; a table with none clears the previous set's. A proxy that
// cannot serve them refuses once series, at the table and at the pool, and
// takes a table without any; one that fails to install them fails the pool.
func TestTheScopeOnceSeriesReachTheProxyBeforeTheSeries(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &onceProxy{seriesProxy: &seriesProxy{wireProxy: wp}}
	a.Proxy = p
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	sent := []string{"pat/-/1"}
	require.NoError(t, a.SetScopeSeries(ctx, map[string][]string{"pat/-/1": {"mock-2"}}))
	require.NoError(t, a.SetScopeOnce(ctx, sent))
	sent[0] = "changed" // the caller's slice is its own
	require.Empty(t, p.order, "the once series go to the proxy with the pool, not before")
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Equal(t, []string{"pat/-/1"}, p.once)
	require.Equal(t, []string{"once", "series"}, p.order)
	require.NoError(t, a.SetScopeOnce(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Empty(t, p.once, "a table with no once series leaves the set none")
	p.err = errors.New("no mock manager")
	require.Error(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}), "a proxy that refuses the once series fails the pool")
	p.err = nil

	b, bp := wireAgent(t, models.MODE_TEST)
	b.Proxy = &seriesProxy{wireProxy: bp}
	require.Error(t, b.SetScopeOnce(ctx, []string{"pat/-/1"}), "a proxy that cannot serve once series refuses them")
	require.NoError(t, b.SetScopeOnce(ctx, nil), "and takes a table with none")
	b.proven.once = []string{"pat/-/1"} // as if the proxy changed under the table
	storeMocks(b, httpMock("mock-2", "GET", "http://h/b"))
	b.scopeTable = map[string][]string{"o::TestA": {}}
	require.Error(t, b.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}), "and the pool refuses them too")
}
