//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// bgClockProxy is a wireProxy that takes the background check's clock
// (coreAgent.BackgroundClockSetter).
type bgClockProxy struct {
	*wireProxy
	set   int
	clock func(time.Time) (time.Time, bool)
}

func (p *bgClockProxy) SetBackgroundClock(f func(time.Time) (time.Time, bool)) {
	p.set++
	p.clock = f
}

// bgClockHook is the no-op hooks that tell the background check the app's
// clock, offset past the real one (BackgroundClockReader).
type bgClockHook struct {
	AgentHook
	offset time.Duration
}

func (h *bgClockHook) BackgroundAppClock(real time.Time) (time.Time, bool) {
	return real.Add(h.offset), true
}

// Once the mocks are stored with the proven switch off, the background check
// alone is handed the app's clock: the hooks' where they tell it, the real
// one from this build's own hooks, which move none, and none from hooks that
// tell neither, replacing the earlier. The learn step is handed none, as
// before. Under the switch the background check is handed nothing: the
// learn tracker tells a call's span there.
func TestStoredMocksHandTheBackgroundCheckTheAppsClockWithTheSwitchOff(t *testing.T) {
	prev := ActiveHooks
	t.Cleanup(func() { ActiveHooks = prev })
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	ActiveHooks = &AgentHook{}
	a, wp := wireAgent(t, models.MODE_TEST)
	t.Setenv(provenswitch.Env, "")
	p := &bgClockProxy{wireProxy: wp}
	a.Proxy = p
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 1 || p.clock == nil {
		t.Fatalf("this build's own hooks: handed %d times, clock %v; want the real clock once", p.set, p.clock != nil)
	}
	if got, ok := p.clock(at); !ok || !got.Equal(at) {
		t.Fatalf("this build's own hooks: the clock read %v %v; want the real instant", got, ok)
	}
	if wp.appClock != nil {
		t.Fatal("the switch off: the learn step is handed no clock, as before")
	}

	ActiveHooks = &bgClockHook{offset: 7 * time.Second}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 2 || p.clock == nil {
		t.Fatalf("hooks that tell it: handed %d times, clock %v; want theirs", p.set, p.clock != nil)
	}
	if got, ok := p.clock(at); !ok || !got.Equal(at.Add(7*time.Second)) {
		t.Fatalf("hooks that tell it: the clock read %v %v; want theirs", got, ok)
	}

	ActiveHooks = &otherHooks{}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 3 || p.clock != nil {
		t.Fatalf("hooks that tell neither: handed %d times, clock %v; want none, replacing the earlier", p.set, p.clock != nil)
	}

	t.Setenv(provenswitch.Env, "1")
	ActiveHooks = &AgentHook{}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 3 {
		t.Fatalf("under the switch the background check was handed a clock (%d times): the learn tracker tells the span there", p.set)
	}
	if wp.appClock == nil {
		t.Fatal("under the switch the learn step reads the real clock, as before")
	}
}
