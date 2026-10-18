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
	h.startIngressHold(ctx)
	if h.IngressHeld() || h.IngressParkListener() != nil || h.IngressParked() != 0 {
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
