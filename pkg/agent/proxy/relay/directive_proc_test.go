package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/models"
)

// A client-side TLS upgrade whose handshake failed is handed, with the
// upgrader's own error, to OnClientHandshakeFailed -- the proxy charges it
// as any client-side handshake -- and is no STARTTLS loss; a destination-side
// failure, an upgrade keploy could not make, still is. Without the hook, a
// client-side failure is one too, as before.
func TestAFailedClientSideUpgradeGoesToTheClientHandshakeHook(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failSide     string
		hook         bool
		wantHook     bool
		wantStartTLS int
	}{
		{"the client's side, with the hook", "client", true, true, 0},
		{"the destination's side, with the hook", "dest", true, false, 1},
		{"the client's side, without the hook", "client", false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			fn, _ := recordingUpgrader(&mu, &order, tc.failSide)
			var starttls int
			var got error
			cfg := Config{TLSUpgradeFn: fn, OnCaptureLoss: func(k models.CaptureLossKind, _ string) {
				mu.Lock()
				defer mu.Unlock()
				if k == models.CaptureLossSTARTTLS {
					starttls++
				}
			}}
			if tc.hook {
				cfg.OnClientHandshakeFailed = func(_ context.Context, err error) {
					mu.Lock()
					defer mu.Unlock()
					got = err
				}
			}
			h := newHarness(t, cfg)
			h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
			if ack := awaitAck(t, h); ack.OK {
				t.Fatal("the upgrade succeeded")
			}
			mu.Lock()
			defer mu.Unlock()
			if (got != nil) != tc.wantHook || (got != nil && got.Error() != "client upgrade failed (simulated)") {
				t.Fatalf("hook got %v, want called %v with the upgrader's error", got, tc.wantHook)
			}
			if starttls != tc.wantStartTLS {
				t.Fatalf("%d starttls_failed, want %d", starttls, tc.wantStartTLS)
			}
		})
	}
}

// The failed ack still says the client's side failed, and wraps its error.
func TestAClientHandshakeErrorReadsAsBefore(t *testing.T) {
	inner := errors.New("tls: bad certificate")
	err := error(&clientHandshakeError{err: inner})
	if err.Error() != "client TLS upgrade: tls: bad certificate" || !errors.Is(err, inner) {
		t.Fatalf("%v: want the old text, wrapping the handshake's error", err)
	}
}
