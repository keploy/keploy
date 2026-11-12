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

type timeFormProxy struct {
	*wireProxy
	on    bool
	calls int
	err   error
}

var _ coreAgent.PoolTimeFormSetter = (*timeFormProxy)(nil)

func (p *timeFormProxy) SetPoolTimeForm(on bool) error {
	if p.err != nil {
		return p.err
	}
	p.on, p.calls = on, p.calls+1
	return nil
}

// Whether the learn step checks a changed time by its form alone goes to
// the proxy with the pool slices, not before; a table that turns it off
// turns it off. A proxy that refuses it fails the pool. A proxy that
// cannot take it is handed nothing and takes the table: it decides times
// by the clock checks alone. It waits for the wire: a caller that went away
// while it waited installs nothing.
func TestTheScopeTimeDefaultReachesTheProxyWithThePool(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &timeFormProxy{wireProxy: wp}
	a.Proxy = p
	storeMocks(a, httpMock("mock-1", "GET", "http://h/a"), httpMock("mock-2", "GET", "http://h/b"))
	ctx := context.Background()
	a.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	require.NoError(t, a.SetScopeTimeForm(ctx, true))
	require.Zero(t, p.calls, "the time default goes to the proxy with the pool, not before")
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.True(t, p.on)
	require.NoError(t, a.SetScopeTimeForm(ctx, false))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
	require.False(t, p.on, "a table that turns it off turns it off")
	require.Equal(t, 2, p.calls)

	require.NoError(t, a.SetScopeTimeForm(ctx, true))
	p.err = errors.New("no mock manager")
	require.Error(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}), "a proxy that refuses the time default fails the pool")
	p.err = nil

	// It waits for the wire as every other part of the table does: a
	// caller that went away while it waited installs nothing.
	require.NoError(t, a.SetScopeTimeForm(ctx, false))
	require.NoError(t, a.wire.lock(ctx))
	gone, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, a.SetScopeTimeForm(gone, true), "a caller that went away while it waited")
	a.wire.unlock()
	a.provenMu.Lock()
	require.False(t, a.proven.timeForm, "and installs nothing")
	a.provenMu.Unlock()

	b, _ := wireAgent(t, models.MODE_TEST)
	require.NoError(t, b.SetScopeTimeForm(ctx, true), "a proxy that cannot take it takes the table")
	b.scopeTable = map[string][]string{"o::TestA": {"mock-1"}}
	require.NoError(t, b.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-2"}}))
}
