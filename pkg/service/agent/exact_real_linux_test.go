//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// realServingProxy is exact serving's proxy (a real mock manager behind the
// real HTTP matcher) with mock.real's bookkeeping: a tracker of the REAL
// connections, and keploy's proxy, whose list decides what a recorded mock's
// destination is (proxy.Proxy.RealDestination).
type realServingProxy struct {
	*servingProxy
	tracker realdeps.Tracker
	real    *proxyPkg.Proxy
}

func newRealServingProxy(sp *servingProxy) *realServingProxy {
	return &realServingProxy{servingProxy: sp, real: proxyPkg.New(zap.NewNop(), nil, &config.Config{})}
}

func (p *realServingProxy) RealTracker() *realdeps.Tracker { return &p.tracker }

func (p *realServingProxy) SetRealDeps(ctx context.Context, real []string) error {
	if err := p.real.SetRealDeps(ctx, real); err != nil {
		return err
	}
	p.tracker.Reset()
	return nil
}

func (p *realServingProxy) RealRun() models.RealRun { return p.tracker.Run() }

func (p *realServingProxy) RealDestination(m *models.Mock) (string, bool) {
	return p.real.RealDestination(m)
}

// recordedCall is name, a call recorded as keploy mock record writes it: an
// origin-form URL, the wire's Host (a client writes no port for its scheme's
// default), and destAddr, the address and port the app connected to, which
// the agent stamps ("" for a recording that did not keep it).
func recordedCall(name, path, host, destAddr string) *models.Mock {
	m := pingMock(name, path, "rows", t0.Add(2*time.Second))
	m.Spec.HTTPReq.URL = path
	m.Spec.HTTPReq.Header = map[string]string{"Host": host}
	m.Spec.Metadata = map[string]string{"connID": "7"}
	if destAddr != "" {
		m.Spec.Metadata["destAddr"] = destAddr
	}
	return m
}

// realMock is a recorded call to the mock.real destination db.real:8080,
// made before it was listed.
func realMock(name string) *models.Mock {
	return recordedCall(name, "/rows", "db.real:8080", "10.1.2.3:8080")
}

// One test with every kind of evidence at once (design v14 §3.2, §4, §5.2):
// a mock exact serving serves it, a call it makes to a mock.real destination
// (a REAL row), a connection keploy could neither capture nor relay
// (Incomplete), a call the agent lost (CaptureIncomplete), and, from a
// recording made before mock.real listed that destination, a recorded call
// to it. Each path reports its own: the exact-served mock answers and is no
// evidence; the REAL row stays on the window and is no miss; the stale
// recorded call to the REAL destination is neither served nor judged
// unmade; the connection and the loss fail the test, the loss first.
// Without the connection and the loss, the same test passes.
func TestATestWithAnExactMockARealRowAndACaptureLoss(t *testing.T) {
	for _, tc := range []struct {
		name  string
		taint bool
	}{
		{"with a lost call and an uncaptured connection", true},
		{"with neither", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, sp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/a", "a", t0), realMock("mock-2")},
				map[string][]string{"o::TestA": {"mock-1", "mock-2"}}, map[string][]string{})
			p := newRealServingProxy(sp)
			a.Proxy = p
			ctx := context.Background()
			require.NoError(t, a.SetRealDeps(ctx, []string{"db.real:8080"}))
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			require.Nil(t, serr)
			code, body := sp.call(t, "/a")
			require.Equal(t, 200, code)
			require.Equal(t, "a", body)
			// The test's REAL connection: relayed before any matcher.
			p.tracker.Dialed(1, "db.real:8080", true)
			p.tracker.Sent(1, "db.real:8080")
			lost := models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 2, What: "target_namespace_pids, E2BIG: the map had no room for it, usually because it is full"}}
			if tc.taint {
				p.tracker.Incomplete("10.0.0.7:443", "keploy could not read which name it was opened for")
				a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 2, LastTargetErr: -7}}
			}
			e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			w := windowOf(t, a, "o::TestA")
			require.Equal(t, []models.RealUse{{Dest: "db.real:8080", Conns: 1}}, w.Real, "the REAL row")
			if !tc.taint {
				require.Empty(t, e.Action, "a test whose recorded call to a REAL destination went real passes: %s", e.Reason)
				require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
				if w.Evidence != nil {
					require.Empty(t, w.Evidence.Misses)
					require.Empty(t, w.Evidence.Unconsumed)
				}
				return
			}
			require.Equal(t, models.ScopeActionFail, e.Action)
			require.Equal(t, "keploy: "+lost.Text()+"; keploy could not capture a connection to 10.0.0.7:443 (keploy could not read which name it was opened for).", e.Reason,
				"the loss first, then the connection; nothing about the exact-served mock or the REAL destination")
			require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
			require.Equal(t, lost, w.Evidence.CaptureIncomplete)
			require.Len(t, w.Evidence.Incomplete, 1)
			require.Empty(t, w.Evidence.Misses, "a REAL call is no miss")
			require.Empty(t, w.Evidence.Unconsumed, "the exact-served mock answered; the REAL one is not judged")
		})
	}
}

// A recorded call stays its test's, served and judged, unless the
// connection it was recorded on is one mock.real now lists (design v14 §4,
// §5.3), decided as keploy's proxy decides it: at the port the app
// connected to, never at a port its scheme would suggest. A recording that
// did not keep that port leaves the call the test's: if the replay does not
// make it, the test fails for it, and re-recording ends that.
func TestARecordedCallIsItsTestsUnlessItsConnectionIsListedReal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		list     []string
		stale    *models.Mock
		madeReal string // the REAL connection the replayed test made
		judged   bool
	}{
		{"an HTTPS call to a name listed at 443, made real", []string{"auth.example.com:443"},
			recordedCall("mock-2", "/token", "auth.example.com", "93.184.216.34:443"), "auth.example.com:443", false},
		{"an HTTPS call to a name listed at 80, no longer made", []string{"api.internal:80"},
			recordedCall("mock-2", "/v1/charge", "api.internal", "10.9.0.4:443"), "", true},
		{"an HTTPS call whose port the recording did not keep, made real", []string{"auth.example.com:443"},
			recordedCall("mock-2", "/token", "auth.example.com", ""), "auth.example.com:443", true},
		{"a call to localhost listed, made real", []string{"localhost:8080"},
			recordedCall("mock-2", "/x", "localhost:8080", ""), "localhost:8080", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, sp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/a", "a", t0), tc.stale},
				map[string][]string{"o::TestA": {"mock-1", "mock-2"}}, map[string][]string{})
			p := newRealServingProxy(sp)
			a.Proxy = p
			ctx := context.Background()
			require.NoError(t, a.SetRealDeps(ctx, tc.list))
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			require.Nil(t, serr)
			code, body := sp.call(t, "/a")
			require.Equal(t, 200, code)
			require.Equal(t, "a", body)
			if tc.madeReal != "" {
				p.tracker.Dialed(1, tc.madeReal, true)
				p.tracker.Sent(1, tc.madeReal)
			}
			e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			w := windowOf(t, a, "o::TestA")
			if !tc.judged {
				require.Empty(t, e.Action, "a recorded call to a REAL destination was judged: %s", e.Reason)
				require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
				if w.Evidence != nil {
					require.Empty(t, w.Evidence.Unconsumed)
				}
				return
			}
			require.Equal(t, models.ScopeActionFail, e.Action, "a recorded call the replay did not make passed")
			require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
			require.Equal(t, []string{"mock-2"}, w.Evidence.Unconsumed)
			require.Contains(t, e.Reason, "(mock-2) was never made")
		})
	}
}
