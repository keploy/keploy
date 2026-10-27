//go:build linux

package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// TestEveryRegisteredParserDeclaresMockNoise enforces the
// integrations.NoiseDeclarer contract: every registered dependency parser must
// consciously DECLARE whether it does field-level request-body mock-noise
// learning (DeclaresMockNoise() -> true) or deliberately opts out because it
// handles drift another way (-> false). A new parser that forgets to implement
// the method fails here, so the decision can never be silently skipped.
//
// This runs over the parsers blank-imported in parsers.go (http, mysql,
// generic). The same assertion runs in the enterprise / k8s-proxy builds, which
// register the full parser set, so the contract is enforced everywhere the
// registry is populated.
func TestEveryRegisteredParserDeclaresMockNoise(t *testing.T) {
	if len(integrations.Registered) == 0 {
		t.Fatal("no parsers registered; expected the blank imports in parsers.go to populate integrations.Registered")
	}

	for name, p := range integrations.Registered {
		if p == nil || p.Initializer == nil {
			t.Errorf("registered parser %q has no initializer", name)
			continue
		}
		parser := p.Initializer(zap.NewNop())
		decl, ok := parser.(integrations.NoiseDeclarer)
		if !ok {
			t.Errorf("parser %q does not implement integrations.NoiseDeclarer: "+
				"every parser must DECLARE its mock-noise behaviour (return true for "+
				"field-level request-body learning, false to opt out). Add a "+
				"DeclaresMockNoise() bool method on the parser type.", name)
			continue
		}
		t.Logf("parser %q declares mock-noise = %v", name, decl.DeclaresMockNoise())
	}
}

// TestEveryRegisteredParserReportsMockMiss enforces the integrations.MissReporter
// contract: every registered parser must confirm it reports a no-mock-matched
// miss (mock diff) so the miss reaches the test report instead of being
// silently swallowed. Unlike mock noise there is no opt-out — every parser must
// return true — so a new parser that forgets to report misses fails here.
func TestEveryRegisteredParserReportsMockMiss(t *testing.T) {
	if len(integrations.Registered) == 0 {
		t.Fatal("no parsers registered; expected the blank imports in parsers.go to populate integrations.Registered")
	}
	for name, p := range integrations.Registered {
		if p == nil || p.Initializer == nil {
			t.Errorf("registered parser %q has no initializer", name)
			continue
		}
		rep, ok := p.Initializer(zap.NewNop()).(integrations.MissReporter)
		if !ok {
			t.Errorf("parser %q does not implement integrations.MissReporter: "+
				"every parser must report a no-mock-matched miss. Add a "+
				"ReportsMockMiss() bool method (return true) on the parser type.", name)
			continue
		}
		if !rep.ReportsMockMiss() {
			t.Errorf("parser %q returns ReportsMockMiss()=false: a parser must "+
				"surface its misses to the test report, not swallow them.", name)
		}
	}
}
