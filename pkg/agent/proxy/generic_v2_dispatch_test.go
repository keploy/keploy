package proxy

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// fakeParser implements Integrations, and IntegrationsV2 only when v2Declared.
type fakeParser struct{ v2 bool }

func (fakeParser) MatchType(context.Context, []byte) bool { return false }
func (fakeParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (fakeParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}
func (f fakeParser) IsV2() bool { return f.v2 }

// legacyOnlyParser does NOT implement IntegrationsV2 at all.
type legacyOnlyParser struct{}

func (legacyOnlyParser) MatchType(context.Context, []byte) bool { return false }
func (legacyOnlyParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (legacyOnlyParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}

// TestShouldRecordViaSupervisor pins the dispatch decision behaviourally.
//
// This replaces two source-grep assertions that could not fail for any
// behavioural reason. They asserted that the substrings "recordViaSupervisor("
// and "IsV2()" appeared in a text window — so INVERTING the gate
// (ok && !v2.IsV2()) kept every asserted substring and left the entire proxy
// test tree green, which is the exact bug plus its inverse. One of them also
// bounded its window on a string that does not exist in proxy.go, so the
// "window" was the remaining 890 lines of the file and any match anywhere
// satisfied it.
func TestShouldRecordViaSupervisor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parser     integrations.Integrations
		relayOff   string
		wantV2Path bool
		why        string
	}{
		{
			name: "V2 parser, relay enabled", parser: fakeParser{v2: true},
			wantV2Path: true,
			why:        "a migrated parser must reach the supervisor, or its V2 recorder never runs",
		},
		{
			name: "V2 parser, relay disabled", parser: fakeParser{v2: true}, relayOff: "off",
			wantV2Path: false,
			why:        "KEPLOY_NEW_RELAY=off is the documented escape hatch and must still force legacy",
		},
		{
			name: "parser declares IsV2() false", parser: fakeParser{v2: false},
			wantV2Path: false,
			why:        "a parser that opts out must not be handed FakeConns it cannot use",
		},
		{
			name: "parser does not implement IntegrationsV2", parser: legacyOnlyParser{},
			wantV2Path: false,
			why:        "an unmigrated parser has no V2 entrypoint at all",
		},
		{
			name: "the real generic parser", parser: generic.New(zap.NewNop()),
			wantV2Path: true,
			why: "generic is the catch-all for every unmatched protocol; if it does not reach " +
				"the supervisor, the widest path in the proxy records legacy on the default config",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.relayOff != "" {
				t.Setenv("KEPLOY_NEW_RELAY", tc.relayOff)
			} else {
				t.Setenv("KEPLOY_NEW_RELAY", "")
			}
			if got := shouldRecordViaSupervisor(tc.parser); got != tc.wantV2Path {
				t.Errorf("shouldRecordViaSupervisor = %v, want %v — %s", got, tc.wantV2Path, tc.why)
			}
		})
	}
}
