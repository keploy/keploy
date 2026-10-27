//go:build linux

package linux

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// Where the hold cannot start (here the main object is not loaded; on a
// kernel without sk_lookup, the ingress object does not load), keploy says
// once, at WARN, the window it leaves: a call between the app's listen and
// keploy's forwarder is refused and not recorded. It then holds nothing, and
// the ingress manager falls back to the forwarder alone.
func TestAHoldThatCannotStartSaysTheWindowItLeaves(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	h := NewHooks(zap.New(core), &config.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startIngressHold(ctx, t.TempDir())
	if h.IngressHeld() || h.IngressParkListener() != nil || h.IngressLookupCounts() != nil {
		t.Fatal("a hold that could not start reports one")
	}
	if _, ok := h.MovedPort(8080); ok {
		t.Fatal("MovedPort answered with nothing held")
	}
	h.RegisterIngressForwarder(8080, nil) // no-ops with nothing held
	h.UnregisterIngressForwarder(8080)
	warns := logs.FilterMessageSnippet("keploy cannot hold the application's port from its bind").All()
	if len(warns) != 1 {
		t.Fatalf("%d warnings, want one: %v", len(warns), logs.All())
	}
	w := warns[0]
	if !strings.Contains(w.Message, "refused") || !strings.Contains(w.Message, "not recorded") || !strings.Contains(w.Message, "Started ingress forwarding") {
		t.Fatalf("the warning does not say the window: %q", w.Message)
	}
	fields := w.ContextMap()
	if fields["next_step"] == nil || fields["kernel"] == nil || fields["error"] == nil {
		t.Fatalf("the warning lacks its kernel, error or next_step: %v", fields)
	}
}

// The bind hooks note what the app binds (structs.FlagIngressHold) only for
// a hold that is on: set when it is, cleared when it is not, even if a
// downstream build's customizer set it, and no other bit is touched.
func TestTheHoldFlagIsSetOnlyForAHoldThatIsOn(t *testing.T) {
	others := structs.FlagKernelPortAlloc | structs.FlagClassifies
	if got := ingressHoldFlag(others, true); got != others|structs.FlagIngressHold {
		t.Fatalf("held: flags %#x, want %#x", got, others|structs.FlagIngressHold)
	}
	if got := ingressHoldFlag(others|structs.FlagIngressHold, false); got != others {
		t.Fatalf("not held: flags %#x, want %#x", got, others)
	}
	if structs.FlagIngressHold != 1<<5 {
		t.Fatalf("FlagIngressHold is %#x; KEPLOY_FLAG_INGRESS_HOLD is bit 5", structs.FlagIngressHold)
	}
}
