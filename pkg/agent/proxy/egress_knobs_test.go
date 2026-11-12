package proxy

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// egressLines is every "real egress opened by" line logged so far.
func egressLines(logs *observer.ObservedLogs) []string {
	var out []string
	for _, e := range logs.All() {
		if strings.HasPrefix(e.Message, agent.RealEgressPrefix) {
			out = append(out, e.Message)
		}
	}
	return out
}

// replayProxy is a replay agent's proxy whose log is observed, ready for
// Proxy.Mock.
func replayProxy(t *testing.T, globalPassthrough bool) (*Proxy, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	cfg.Agent.GlobalPassthrough = globalPassthrough
	p := New(zap.New(core), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	return p, logs
}

// A replay session that opens nothing logs no egress line: the default replay
// (mocking on, a miss fails) never claims the network was opened.
func TestADefaultReplaySessionNamesNoRealEgress(t *testing.T) {
	p, logs := replayProxy(t, false)
	for _, opts := range []models.OutgoingOptions{
		{Mocking: true},
		{Mocking: true, OnMiss: models.MissFail},
	} {
		if err := p.Mock(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
	}
	p.logAgentEgressKnobs()
	if got := egressLines(logs); len(got) != 0 {
		t.Fatalf("a default replay logged real egress: %q", got)
	}
}

// Each session knob that dials the real destination in a replay is named when
// the session starts, with what it opens.
func TestEachReplaySessionKnobThatOpensRealEgressIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts models.OutgoingOptions
		knob string
	}{
		{"mocking off", models.OutgoingOptions{Mocking: false}, "test.mocking: false"},
		{"onMiss passthrough", models.OutgoingOptions{Mocking: true, OnMiss: models.MissPassthrough}, "onMiss passthrough"},
		{"onMiss record", models.OutgoingOptions{Mocking: true, OnMiss: models.MissRecord}, "onMiss record"},
		{"opportunistic TLS intercept", models.OutgoingOptions{Mocking: true, OpportunisticTLSIntercept: true}, "opportunisticTLSIntercept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, logs := replayProxy(t, false)
			if err := p.Mock(context.Background(), tc.opts); err != nil {
				t.Fatal(err)
			}
			got := egressLines(logs)
			if len(got) != 1 || !strings.HasPrefix(got[0], agent.RealEgressPrefix+tc.knob) {
				t.Fatalf("logged %q, want one line naming %q", got, tc.knob)
			}
		})
	}
}

// keploy test starts a session per test set with the same options: the knobs
// are named once, and again only when a session opens something else.
func TestTheSameSessionKnobsAreNamedOncePerChange(t *testing.T) {
	p, logs := replayProxy(t, false)
	off := models.OutgoingOptions{Mocking: false}
	for range 3 {
		if err := p.Mock(context.Background(), off); err != nil {
			t.Fatal(err)
		}
	}
	if got := egressLines(logs); len(got) != 1 {
		t.Fatalf("three test sets with mocking off logged %d egress lines, want 1: %q", len(got), got)
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true, OnMiss: models.MissPassthrough}); err != nil {
		t.Fatal(err)
	}
	if err := p.Mock(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	got := egressLines(logs)
	if len(got) != 3 || !strings.Contains(got[1], "onMiss passthrough") || !strings.Contains(got[2], "test.mocking: false") {
		t.Fatalf("a changed session was not named: %q", got)
	}
}

// --global-passthrough is named at a replay agent's start; a recording agent's
// global passthrough is how it records, not real egress in a replay.
func TestGlobalPassthroughIsNamedOnlyByAReplayAgent(t *testing.T) {
	p, logs := replayProxy(t, true)
	p.logAgentEgressKnobs()
	if got := egressLines(logs); len(got) != 1 || !strings.HasPrefix(got[0], agent.RealEgressPrefix+"--global-passthrough: ") {
		t.Fatalf("a replay agent with global passthrough logged %q, want one line naming it", got)
	}
	if got := agentEgressKnobs(models.MODE_RECORD, true); len(got) != 0 {
		t.Fatalf("a recording agent named %v", got)
	}
}
