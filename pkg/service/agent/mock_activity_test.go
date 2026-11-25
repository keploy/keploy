package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

type ctxMarkerKey struct{}

type activityStubProxy struct {
	coreAgent.Proxy
	consumed        int
	missed          int
	inFlight        int
	inFlightCounted bool
	calls           []string
	seenCtxMarkers  []string
}

func (p *activityStubProxy) MockActivity() (int, int, int, bool) {
	return p.consumed, p.missed, p.inFlight, p.inFlightCounted
}

func (p *activityStubProxy) record(name string, ctx context.Context) {
	p.calls = append(p.calls, name)
	marker, _ := ctx.Value(ctxMarkerKey{}).(string)
	p.seenCtxMarkers = append(p.seenCtxMarkers, marker)
}

func (p *activityStubProxy) BeginTestErrorCaptureCtx(ctx context.Context) {
	p.record("begin-ctx", ctx)
}

func (p *activityStubProxy) ContinueTestErrorCaptureCtx(ctx context.Context) {
	p.record("continue-ctx", ctx)
}

func (p *activityStubProxy) BeginTestCaseCaptureCtx(ctx context.Context) {
	p.record("begin-case-ctx", ctx)
}

func (p *activityStubProxy) ContinueTestCaseCaptureCtx(ctx context.Context) {
	p.record("continue-case-ctx", ctx)
}

type legacyCaptureStubProxy struct {
	coreAgent.Proxy
	calls []string
}

func (p *legacyCaptureStubProxy) BeginTestErrorCapture() {
	p.calls = append(p.calls, "begin-legacy")
}

func (p *legacyCaptureStubProxy) ContinueTestErrorCapture() {
	p.calls = append(p.calls, "continue-legacy")
}

func TestAgentMockStatsPopulatesActivity(t *testing.T) {
	t.Run("inFlightCounted=true", func(t *testing.T) {
		p := &activityStubProxy{consumed: 7, missed: 2, inFlight: 1, inFlightCounted: true}
		a := &Agent{logger: zap.NewNop(), Proxy: p, loadedMocks: 21}

		stats, err := a.MockStats(context.Background())
		if err != nil {
			t.Fatalf("MockStats: %v", err)
		}
		if !stats.ActivityCounted || !stats.InFlightCounted {
			t.Fatalf("flags = (activity=%v, inFlight=%v), want (true, true)", stats.ActivityCounted, stats.InFlightCounted)
		}
		if stats.Loaded != 21 || stats.Consumed != 7 || stats.Missed != 2 || stats.InFlight != 1 {
			t.Fatalf("stats = %+v, want loaded=21 consumed=7 missed=2 inFlight=1", stats)
		}
		raw, err := json.Marshal(stats)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(raw)
		if !strings.Contains(s, `"activityCounted":true`) || !strings.Contains(s, `"inFlightCounted":true`) || !strings.Contains(s, `"inFlight":1`) {
			t.Fatalf("json = %s, want activityCounted, inFlightCounted, and inFlight", s)
		}
	})

	t.Run("inFlightCounted=false", func(t *testing.T) {
		p := &activityStubProxy{consumed: 4, missed: 1, inFlight: 0, inFlightCounted: false}
		a := &Agent{logger: zap.NewNop(), Proxy: p, loadedMocks: 10}

		stats, err := a.MockStats(context.Background())
		if err != nil {
			t.Fatalf("MockStats: %v", err)
		}
		if !stats.ActivityCounted || stats.InFlightCounted {
			t.Fatalf("flags = (activity=%v, inFlight=%v), want (true, false)", stats.ActivityCounted, stats.InFlightCounted)
		}
		if stats.Loaded != 10 || stats.Consumed != 4 || stats.Missed != 1 || stats.InFlight != 0 {
			t.Fatalf("stats = %+v, want loaded=10 consumed=4 missed=1 inFlight=0", stats)
		}
		raw, err := json.Marshal(stats)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(raw)
		if !strings.Contains(s, `"activityCounted":true`) {
			t.Fatalf("json = %s, want activityCounted:true", s)
		}
		if strings.Contains(s, `"inFlightCounted"`) || strings.Contains(s, `"inFlight"`) {
			t.Fatalf("json = %s, must omit inFlightCounted and inFlight when inFlightCounted=false", s)
		}
	})

	t.Run("proxy without MockActivityReader", func(t *testing.T) {
		a := &Agent{logger: zap.NewNop(), Proxy: &legacyCaptureStubProxy{}, loadedMocks: 5}
		stats, err := a.MockStats(context.Background())
		if err != nil {
			t.Fatalf("MockStats: %v", err)
		}
		if stats.ActivityCounted || stats.InFlightCounted || stats.Loaded != 5 {
			t.Fatalf("stats = %+v, want Loaded=5 and both counted flags false", stats)
		}
	})
}

func TestAgentBeginAndContinueCapturePassContext(t *testing.T) {
	for _, tc := range []struct {
		proven string
		want   []string
	}{
		{"", []string{"begin-ctx", "continue-ctx"}},
		{"1", []string{"begin-case-ctx", "continue-case-ctx"}},
	} {
		t.Run("KEPLOY_PROVEN="+tc.proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			p := &activityStubProxy{}
			a := &Agent{logger: zap.NewNop(), Proxy: p}
			beginCtx := context.WithValue(context.Background(), ctxMarkerKey{}, "begin-token")
			continueCtx := context.WithValue(context.Background(), ctxMarkerKey{}, "continue-token")
			if err := a.BeginTestErrorCapture(beginCtx); err != nil {
				t.Fatalf("BeginTestErrorCapture: %v", err)
			}
			if err := a.ContinueTestErrorCapture(continueCtx); err != nil {
				t.Fatalf("ContinueTestErrorCapture: %v", err)
			}
			if len(p.calls) != len(tc.want) || p.calls[0] != tc.want[0] || p.calls[1] != tc.want[1] {
				t.Fatalf("calls = %v, want %v", p.calls, tc.want)
			}
			if len(p.seenCtxMarkers) != 2 || p.seenCtxMarkers[0] != "begin-token" || p.seenCtxMarkers[1] != "continue-token" {
				t.Fatalf("forwarded ctx markers = %v, want [begin-token continue-token]", p.seenCtxMarkers)
			}
		})
	}

	t.Run("legacy proxy fallback without Ctx methods", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		p := &legacyCaptureStubProxy{}
		a := &Agent{logger: zap.NewNop(), Proxy: p}
		if err := a.BeginTestErrorCapture(context.Background()); err != nil {
			t.Fatalf("BeginTestErrorCapture: %v", err)
		}
		if err := a.ContinueTestErrorCapture(context.Background()); err != nil {
			t.Fatalf("ContinueTestErrorCapture: %v", err)
		}
		if len(p.calls) != 2 || p.calls[0] != "begin-legacy" || p.calls[1] != "continue-legacy" {
			t.Fatalf("calls = %v, want [begin-legacy continue-legacy]", p.calls)
		}
	})
}
