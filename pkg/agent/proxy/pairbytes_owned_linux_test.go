//go:build linux

package proxy

import (
	"net/netip"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent"
)

// The proxy's warning for a loss the gap between test cases owned says the
// bytes were written between test cases, never "in" a test case: a test
// case already verified is not named, and the gap reads as no test case's.
// A test's own says "in <test>".
func TestAGapsLossWarningSaysBetweenTestCases(t *testing.T) {
	for _, tc := range []struct {
		owner, says string
	}{
		{agent.BetweenTestCases, "the bytes were written between test cases, not in the test open now"},
		{"test-1", "the bytes were written in test-1, not in the test open now"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			p := &Proxy{logger: zap.New(core)}
			pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:50912")}
			pc.cutShort(tc.owner, 10)
			p.pairLost(pc, 3, 8, unreadLost)
			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("%d WARNs, want 1: %v", len(entries), entries)
			}
			next, _ := entries[0].ContextMap()["next_step"].(string)
			if !strings.HasPrefix(next, tc.says) {
				t.Fatalf("next_step %q, want it to begin %q", next, tc.says)
			}
			if tc.owner == agent.BetweenTestCases && strings.Contains(next, "in "+agent.BetweenTestCases) {
				t.Fatalf("next_step %q names the gap as a test", next)
			}
			if got := p.TakeOwnedLosses(); len(got) != 1 || got[0].Test != tc.owner {
				t.Fatalf("owned %+v, want one of %q", got, tc.owner)
			}
		})
	}
}
