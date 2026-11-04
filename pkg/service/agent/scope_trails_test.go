package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// trailsProxy is wireProxy that takes the test each worker recording trails
// (coreAgent.PoolTrailsSetter).
type trailsProxy struct {
	*wireProxy
	trails map[string]string
	calls  int
	err    error
}

var _ coreAgent.PoolTrailsSetter = (*trailsProxy)(nil)

func (p *trailsProxy) SetPoolTrails(trails map[string]string) error {
	if p.err != nil {
		return p.err
	}
	p.trails, p.calls = trails, p.calls+1
	return nil
}

// The table's trails reach the proxy with its tests, when the pool slices
// are served (SetScopePool), as the CLI sent them; a table with none clears
// the previous set's. A proxy that cannot learn by them refuses trails, and
// takes a table without any.
func TestTheScopeTrailsReachTheProxyWithThePool(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &trailsProxy{wireProxy: wp}
	a.Proxy = p
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	sent := map[string]string{"mock-2": "o::TestA"}
	require.NoError(t, a.SetScopeTrails(ctx, sent))
	sent["mock-2"] = "o::Changed" // the caller's map is its own
	require.Zero(t, p.calls, "the trails go to the proxy with the pool, not before")
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Equal(t, map[string]string{"mock-2": "o::TestA"}, p.trails)
	require.Equal(t, map[string][]string{"o::TestA": {"mock-1"}}, wp.poolTests, "with the tests whose lists they read")
	require.NoError(t, a.SetScopeTrails(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Empty(t, p.trails, "a table with no trails leaves the set none")
	require.Equal(t, 2, p.calls)
	p.err = errors.New("no mock manager")
	require.Error(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}), "a proxy that refuses the trails fails the pool")
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
	require.Error(t, a.SetScopeTrails(gone, map[string]string{"mock-2": "o::TestA"}))
	a.provenMu.Lock()
	require.Empty(t, a.proven.trails)
	a.provenMu.Unlock()

	b, _ := wireAgent(t, models.MODE_TEST)
	require.Error(t, b.SetScopeTrails(ctx, map[string]string{"mock-2": "o::TestA"}), "a proxy that cannot learn by trails refuses them")
	require.NoError(t, b.SetScopeTrails(ctx, map[string]string{}), "and takes a table with none")
}
