//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// laxPingMock is pingMock as an HTTP recorder that stated no lifetime wrote
// it, "mocks"-tagged, after DeriveLifetime's lax rule (pkg/models
// lifetime.go, KEPLOY_STRICT_MOCK_WINDOW unset) classified it: a session mock
// served as a cursor that saturates on its last recording. It is set as the
// rule sets it, so the row does not depend on this process's environment.
func laxPingMock(name, path, body string, at time.Time) *models.Mock {
	m := pingMock(name, path, body, at)
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.TestModeInfo = models.TestModeInfo{
		Lifetime:        models.LifetimeSession,
		Consume:         models.ConsumeCursorSaturate,
		LifetimeDerived: true,
	}
	return m
}

// laxProvenAgent is a replaying agent under the proven switch holding a
// recording made before the HTTP recorder stated lifetimes: its two
// "mocks"-tagged calls of GET /q, in the per-test region as keploy test
// stores them (the reader files "mocks" there), each mapped to the test that
// made it.
func laxProvenAgent(t *testing.T) (*Agent, *servingProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, []*models.Mock{
		laxPingMock("mock-a", "/q", "a", t0),
		laxPingMock("mock-b", "/q", "b", t0.Add(3*time.Second)),
	}, nil))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-a"}, "o::TestB": {"mock-b"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	return a, p
}

// Under the proven switch a begin carries no pid (the claim names the test
// process): every begin is pid-less. Over a recording made before the HTTP
// recorder stated lifetimes, whose calls the lax rule made session mocks, a
// begin of a test the run's process claimed is served exactly: each test's
// GET /q is answered by its own recording, in any order, and one call more
// than it recorded fails the test by name. Nothing spins, and nothing misses
// silently. Without the switch the same begin hung the call (the matcher
// retried a claim of the session mock forever, 99% CPU).
//
// The calls wait for the answer, never a timer: a hang ends in go test's own
// -timeout, which names this row.
func TestAProvenBeginServesAPreLifetimeRecordingAsItsTestsRecordedIt(t *testing.T) {
	a, p := laxProvenAgent(t)
	claimSelf(t, a)

	got, e := runTest(t, a, p, "TestB", "/q")
	require.Equal(t, []string{"200 b"}, got, "TestB, run first, is answered by its own recording")
	require.Empty(t, e.Action, "TestB failed: %s", e.Reason)

	got, e = runTest(t, a, p, "TestA", "/q", "/q")
	require.Equal(t, "200 a", got[0], "TestA is answered by its own recording, not TestB's")
	require.True(t, strings.HasPrefix(got[1], "502"), "TestA recorded one call of GET /q: the second has no answer, got %q", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /q to 127.0.0.1:18093 was called 2 times, recorded 1")
	ev := evidenceOf(t, a, "o::TestA")
	require.Len(t, ev.Misses, 1)
	require.Equal(t, models.MissExtraCall, ev.Misses[0].Kind)
}

// A begin before any test process claimed the run is refused by name
// (not_in_run): keploy cannot tell whose calls it would serve. It stages
// nothing, so the claim that follows serves the recording as recorded.
func TestAProvenBeginBeforeTheClaimIsRefusedByName(t *testing.T) {
	a, p := laxProvenAgent(t)
	_, serr := a.ProvenBegin(context.Background(), begin("TestB"))
	require.NotNil(t, serr, "a begin before the claim was served")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)

	claimSelf(t, a)
	got, e := runTest(t, a, p, "TestB", "/q")
	require.Equal(t, []string{"200 b"}, got)
	require.Empty(t, e.Action, "TestB failed: %s", e.Reason)
}
