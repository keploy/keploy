package proxy

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
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
			name: "V2 parser", parser: fakeParser{v2: true},
			wantV2Path: true,
			why:        "a migrated parser must reach the supervisor, or its V2 recorder never runs",
		},
		{
			// The KEPLOY_NEW_RELAY rollback knob was REMOVED: every parser the
			// dispatcher can route to record is V2, and the switch only served
			// to force them onto a legacy path whose half-close handling hangs
			// EOF-delimited peers. Setting it must now be inert. Kept as a case
			// rather than deleted so re-introducing the knob fails here.
			name:   "the removed rollback knob no longer forces legacy",
			parser: fakeParser{v2: true}, relayOff: "off",
			wantV2Path: true,
			why:        "KEPLOY_NEW_RELAY is gone; a stale value in the environment must not divert a V2 parser",
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

// TestWarnIfRemovedRelayKnobSet pins the startup notice for the removed
// KEPLOY_NEW_RELAY variable.
//
// Silence is the dangerous outcome: the variable was a documented incident
// rollback, so an operator can reasonably set it, restart, see no error, and
// believe they have rolled back — while every parser still records through the
// supervisor. The warning has to fire whenever the variable is PRESENT, not
// only when it holds a formerly-truthy value: "off" and "on" are now equally
// ignored, and someone who left `KEPLOY_NEW_RELAY=on` set is just as misled.
func TestWarnIfRemovedRelayKnobSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      bool
		value    string
		wantWarn bool
	}{
		{name: "unset", set: false, wantWarn: false},
		{name: "off (the documented rollback)", set: true, value: "off", wantWarn: true},
		{name: "0", set: true, value: "0", wantWarn: true},
		{name: "on", set: true, value: "on", wantWarn: true},
		{name: "empty but present", set: true, value: "", wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("KEPLOY_NEW_RELAY", tc.value)
			} else {
				os.Unsetenv("KEPLOY_NEW_RELAY")
			}
			core, logs := observer.New(zap.WarnLevel)
			warnIfRemovedRelayKnobSet(zap.New(core))

			got := logs.FilterMessageSnippet("KEPLOY_NEW_RELAY").Len() > 0
			if got != tc.wantWarn {
				t.Fatalf("warned=%v, want %v — an operator who set a removed rollback knob "+
					"must be told it is ignored, or they believe a rollback is in effect", got, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			// The message must name the lever that still works, or it tells the
			// operator their escape hatch is gone without offering another.
			if logs.FilterMessageSnippet("KEPLOY_NEW_RELAY").Len() > 0 {
				fields := logs.All()[0].ContextMap()
				next, _ := fields["next_step"].(string)
				if !strings.Contains(next, "KEPLOY_DISABLE_PARSING") {
					t.Errorf("next_step %q does not point at KEPLOY_DISABLE_PARSING, the "+
						"remaining way to disable parsing during an incident", next)
				}
			}
		})
	}
}

// TestNilLoggerDoesNotPanicOnRemovedKnob covers the guard: New is called with a
// logger in production, but the helper must not become a nil-panic source.
func TestNilLoggerDoesNotPanicOnRemovedKnob(t *testing.T) {
	t.Setenv("KEPLOY_NEW_RELAY", "off")
	warnIfRemovedRelayKnobSet(nil)
}

// TestNewWarnsAboutTheRemovedRelayKnob pins the WIRING, not just the helper.
//
// TestWarnIfRemovedRelayKnobSet calls warnIfRemovedRelayKnobSet directly, so it
// stays green if the call is deleted from New — the operator then gets silence,
// which is the exact failure the warning exists to prevent. This drives the
// real constructor.
func TestNewWarnsAboutTheRemovedRelayKnob(t *testing.T) {
	t.Setenv("KEPLOY_NEW_RELAY", "off")
	core, logs := observer.New(zap.WarnLevel)
	_ = New(zap.New(core), nil, &config.Config{})
	if logs.FilterMessageSnippet("KEPLOY_NEW_RELAY").Len() == 0 {
		t.Fatal("New() did not warn about the removed KEPLOY_NEW_RELAY knob — the helper " +
			"exists but nothing calls it, so an operator relying on the old rollback " +
			"gets no signal that it is ignored")
	}
}

// TestNewIsQuietWithoutTheRemovedKnob is the negative control: the warning must
// not fire on a normal start, or it becomes noise everyone learns to ignore.
func TestNewIsQuietWithoutTheRemovedKnob(t *testing.T) {
	os.Unsetenv("KEPLOY_NEW_RELAY")
	core, logs := observer.New(zap.WarnLevel)
	_ = New(zap.New(core), nil, &config.Config{})
	if n := logs.FilterMessageSnippet("KEPLOY_NEW_RELAY").Len(); n != 0 {
		t.Fatalf("New() warned about KEPLOY_NEW_RELAY %d time(s) with the variable unset", n)
	}
}

// TestRecordGenericOutgoing_UnregisteredParserDeclinesInsteadOfPanicking covers
// the guard that had no coverage at all: deleting it left the entire proxy
// suite green.
//
// Generic.IsV2() is unconditionally true, so with the KEPLOY_NEW_RELAY rollback
// removed the only way past the supervisor branch is an unregistered GENERIC —
// where the legacy call nil-derefs the interface. The connection must be
// relayed as a decline, not dropped and not panicked on.
func TestRecordGenericOutgoing_UnregisteredParserDeclinesInsteadOfPanicking(t *testing.T) {
	p := &Proxy{
		logger:                     zap.NewNop(),
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{}, // GENERIC absent
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     5 * time.Second,
		recordBufferHalfCloseGrace: 5 * time.Second,
	}

	srcRaw, destRaw, cleanup := tcpConnPair(t)
	defer cleanup()
	// Close both so relayDeclinedConn's copy loops finish rather than blocking.
	_ = srcRaw.Close()
	_ = destRaw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
	errGrp, gctx := errgroup.WithContext(ctx)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("recordGenericOutgoing panicked on an unregistered GENERIC parser: %v — "+
				"the legacy branch dereferences a nil integrations.Integrations", r)
		}
	}()

	err := p.recordGenericOutgoing(gctx, srcRaw, destRaw, make(chan *models.Mock, 8), errGrp,
		zap.NewNop(), 1, 2, models.OutgoingOptions{})
	if err != nil {
		t.Fatalf("recordGenericOutgoing returned %v, want nil — an unregistered parser is a "+
			"decline to be relayed, not a connection failure", err)
	}
}

// TestRecordGenericOutgoing_RoutesRegisteredParserToSupervisor pins the other
// half of the dispatch decision, so the nil guard cannot be "fixed" by making
// every generic connection take it.
func TestRecordGenericOutgoing_RoutesRegisteredParserToSupervisor(t *testing.T) {
	if !shouldRecordViaSupervisor(generic.New(zap.NewNop())) {
		t.Fatal("the real generic parser no longer routes to the supervisor — the widest " +
			"path in the proxy would record through the legacy surface")
	}
}

// TestMockGenericOutgoing_UnregisteredParserReportsInsteadOfPanicking is the
// replay-side twin of the record guard. The record half got a nil check while
// this one kept dereferencing the map lookup, so the same unregistered build
// that was fixed for recording still crashed on replay.
//
// Replay cannot relay — every destination dial in handleConnection is
// mode-gated, so there is no upstream conn — hence this reports the miss rather
// than passing the connection through.
func TestMockGenericOutgoing_UnregisteredParserReportsInsteadOfPanicking(t *testing.T) {
	p := &Proxy{
		logger:       zap.NewNop(),
		Integrations: map[integrations.IntegrationType]integrations.Integrations{}, // GENERIC absent
	}

	srcRaw, destRaw, cleanup := tcpConnPair(t)
	defer cleanup()
	_ = destRaw.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mockGenericOutgoing panicked on an unregistered GENERIC parser: %v", r)
		}
	}()

	err := p.mockGenericOutgoing(context.Background(), srcRaw,
		&models.ConditionalDstCfg{Addr: "127.0.0.1:1"}, nil, models.OutgoingOptions{}, zap.NewNop())
	if err == nil {
		t.Fatal("mockGenericOutgoing returned nil with no GENERIC parser registered — replay " +
			"cannot serve the connection, so this must surface as a mock miss rather than " +
			"reporting success")
	}
}
