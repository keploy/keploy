package agent

import (
	"context"
	"errors"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// pausingProxy is a proxy that can pause recording.
type pausingProxy struct {
	coreAgent.Proxy // nil: any other call panics loudly
	paused          bool
	reason          string
}

func (p *pausingProxy) PauseRecording(_ context.Context, reason string) error {
	p.paused, p.reason = true, reason
	return nil
}

func (p *pausingProxy) ResumeRecording(context.Context) error {
	p.paused, p.reason = false, ""
	return nil
}

func (p *pausingProxy) RecordingPaused() (bool, string) { return p.paused, p.reason }

// The agent hands the control plane's pause and resume to its proxy, and says
// what the proxy says of the pause.
func TestTheAgentPausesRecordingThroughItsProxy(t *testing.T) {
	px := &pausingProxy{}
	a := &Agent{Proxy: px}
	ctx := context.Background()
	if err := a.PauseRecording(ctx, "rollout"); err != nil {
		t.Fatal(err)
	}
	if paused, reason, supported := a.RecordingPaused(); !paused || reason != "rollout" || !supported {
		t.Fatalf("RecordingPaused() = %v, %q, %v after the pause", paused, reason, supported)
	}
	if err := a.ResumeRecording(ctx); err != nil {
		t.Fatal(err)
	}
	if paused, _, supported := a.RecordingPaused(); paused || !supported {
		t.Fatalf("RecordingPaused() = %v, supported %v after the resume", paused, supported)
	}
}

// With a proxy that cannot pause (another's, a double's, none), the agent
// says so in the error the routes answer 501 for, and is never paused.
func TestTheAgentCannotPauseWithoutAProxyThatCan(t *testing.T) {
	for name, a := range map[string]*Agent{
		"no proxy":              {},
		"a proxy with no pause": {Proxy: struct{ coreAgent.Proxy }{}},
	} {
		for what, err := range map[string]error{
			"pause":  a.PauseRecording(context.Background(), "rollout"),
			"resume": a.ResumeRecording(context.Background()),
		} {
			if !errors.Is(err, ErrRecordPauseUnsupported) {
				t.Fatalf("%s, %s: %v, want ErrRecordPauseUnsupported", name, what, err)
			}
			var u interface{ RecordPauseUnsupported() bool }
			if !errors.As(err, &u) || !u.RecordPauseUnsupported() {
				t.Fatalf("%s, %s: the error does not say the agent cannot pause at all", name, what)
			}
		}
		if paused, _, supported := a.RecordingPaused(); paused || supported {
			t.Fatalf("%s: RecordingPaused() = %v, supported %v", name, paused, supported)
		}
	}
}
