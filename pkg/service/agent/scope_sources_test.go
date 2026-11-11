package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

type sourcesProxy struct {
	*wireProxy
	sources map[string][]models.DeclaredSource
	calls   int
	err     error
}

var _ coreAgent.PoolSourcesSetter = (*sourcesProxy)(nil)

func (p *sourcesProxy) SetPoolSources(sources map[string][]models.DeclaredSource) error {
	if p.err != nil {
		return p.err
	}
	p.sources, p.calls = sources, p.calls+1
	return nil
}

// The sources declared rules give recordings' fields go to the proxy with
// the pool slices, a copy of the caller's; a table with none leaves the set
// none. Sources this keploy cannot decide a field by (a kind it does not
// know, a clock with no cut, two on one field) are refused and installed
// never, as are any for a proxy that cannot learn by them; such a proxy
// takes a table with none. A proxy that refuses them fails the pool.
func TestTheScopeSourcesReachTheProxyWithThePool(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &sourcesProxy{wireProxy: wp}
	a.Proxy = p
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	clock := models.DeclaredSource{Rule: "at@0a1b2c3d", Kind: models.DeclaredSourceClock, Field: "body.at", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}}
	sent := map[string][]models.DeclaredSource{"mock-1": {clock}}
	require.NoError(t, a.SetScopeSources(ctx, sent))
	sent["mock-1"][0].Field = "body.changed" // the caller's map is its own
	require.Zero(t, p.calls, "the sources go to the proxy with the pool, not before")
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Equal(t, map[string][]models.DeclaredSource{"mock-1": {clock}}, p.sources)
	require.NoError(t, a.SetScopeSources(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.Empty(t, p.sources, "a table with no sources leaves the set none")
	require.Equal(t, 2, p.calls)

	require.NoError(t, a.SetScopeSources(ctx, map[string][]models.DeclaredSource{"mock-1": {clock}}))
	for _, bad := range [][]models.DeclaredSource{
		{{Rule: "at@0a1b2c3d", Kind: "answer", Field: "body.at"}},
		{{Rule: "at@0a1b2c3d", Kind: models.DeclaredSourceClock, Field: "body.at", Clock: &models.ClockOffset{Min: -time.Second}}},
		{clock, {Rule: "id@0a1b2c3d", Kind: models.DeclaredSourceMinted, Field: "body.at"}},
	} {
		require.Error(t, a.SetScopeSources(ctx, map[string][]models.DeclaredSource{"mock-1": bad}), "sources %v", bad)
		a.provenMu.Lock()
		require.Equal(t, map[string][]models.DeclaredSource{"mock-1": {clock}}, a.proven.sources, "a refused table installs nothing")
		a.provenMu.Unlock()
	}

	p.err = errors.New("no mock manager")
	require.Error(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}), "a proxy that refuses the sources fails the pool")
	p.err = nil

	b, _ := wireAgent(t, models.MODE_TEST)
	require.Error(t, b.SetScopeSources(ctx, map[string][]models.DeclaredSource{"mock-1": {clock}}), "a proxy that cannot learn by sources refuses them")
	require.NoError(t, b.SetScopeSources(ctx, map[string][]models.DeclaredSource{}), "and takes a table with none")
}
