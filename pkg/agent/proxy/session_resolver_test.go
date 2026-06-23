package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
)

// TestGetSessionForResolver verifies the O3 seam: GetSessionFor routes by
// TGID when a resolver is installed, falls back to the single session
// when none is set or the resolver returns nil (unmapped/late conn), and
// reverts cleanly when the resolver is cleared. OSS never installs a
// resolver, so this preserves single-session behaviour by default.
func TestGetSessionForResolver(t *testing.T) {
	single := &agent.Session{ID: 1}
	p := &Proxy{session: single}

	// No resolver → single session.
	if got := p.GetSessionFor(123); got != single {
		t.Fatal("nil resolver should return the single session")
	}

	appA := &agent.Session{ID: 2}
	p.SetSessionResolver(func(tgid uint32) *agent.Session {
		if tgid == 1 {
			return appA
		}
		return nil // unmapped
	})

	if got := p.GetSessionFor(1); got != appA {
		t.Fatal("resolver should route tgid 1 to appA")
	}
	if got := p.GetSessionFor(999); got != single {
		t.Fatal("unmapped tgid should fall back to the single session, not drop")
	}

	// Clearing the resolver reverts to single-session mode.
	p.SetSessionResolver(nil)
	if got := p.GetSessionFor(1); got != single {
		t.Fatal("cleared resolver should return the single session")
	}
}
