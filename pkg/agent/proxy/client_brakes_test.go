package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.uber.org/zap"
)

// These tests exist because the client write hold shipped switched off.
// The relay implemented it, MySQL declared WantsClientWriteHold, the
// session carried ClientWritesHeld — and nothing connected them, because
// every other test built relay.Config by hand. A feature whose only
// wiring is one line in a 400-line dispatcher needs a test that fails
// when that line goes away.

// brakeParser is a stand-in parser that answers whichever capability
// probes the test wants it to. Embedding the interface keeps it
// compiling as an integrations.Integrations without implementing a
// parser; none of these tests call the parsing methods.
type brakeParser struct {
	integrations.Integrations
	hold        bool
	preDispatch bool
	sayHold     bool
	sayPre      bool
}

func (b *brakeParser) WantsClientWriteHold() bool  { return b.hold }
func (b *brakeParser) WantsPreDispatchPause() bool { return b.preDispatch }

// bareParser answers no capability probe at all — the third-party
// parser that never heard of either brake.
type bareParser struct{ integrations.Integrations }

func TestApplyClientBrakes_SetsRelayAndSessionTogether(t *testing.T) {
	cfg := relay.Config{}
	sess := &supervisor.Session{}

	applyClientBrakes(&cfg, sess, &brakeParser{hold: true}, zap.NewNop(), integrations.MYSQL)

	if !cfg.HoldClientWrites {
		t.Error("relay.Config.HoldClientWrites is false: the relay will forward the client's " +
			"ClientHello upstream in cleartext, which is the bug the hold exists to prevent")
	}
	if !sess.ClientWritesHeld {
		t.Error("supervisor.Session.ClientWritesHeld is false: the parser will not release the " +
			"hold on the plaintext path, and the connection wedges waiting for a reply to a " +
			"request the server never received")
	}
}

func TestApplyClientBrakes_NoHoldWhenParserDoesNotAsk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parser integrations.Integrations
	}{
		{"declines explicitly", &brakeParser{hold: false}},
		{"does not implement the probe", &bareParser{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := relay.Config{}
			sess := &supervisor.Session{}
			applyClientBrakes(&cfg, sess, tc.parser, zap.NewNop(), integrations.HTTP)
			if cfg.HoldClientWrites || sess.ClientWritesHeld {
				t.Errorf("a parser that did not ask got a hold: cfg=%v sess=%v",
					cfg.HoldClientWrites, sess.ClientWritesHeld)
			}
		})
	}
}

// The two brakes are alternatives, not layers. Pre-dispatch routes the
// client's first chunk through the pause stash, which bypasses the
// hold's byte accounting, and its resume handler acks OK while leaving
// the hold armed.
func TestApplyClientBrakes_HoldWinsOverPreDispatch(t *testing.T) {
	cfg := relay.Config{}
	sess := &supervisor.Session{}

	applyClientBrakes(&cfg, sess, &brakeParser{hold: true, preDispatch: true}, zap.NewNop(), integrations.MYSQL)

	if !cfg.HoldClientWrites {
		t.Error("HoldClientWrites was dropped; the hold is the stronger brake and must win")
	}
	if cfg.PreDispatchPause {
		t.Error("PreDispatchPause survived alongside a hold: the combination silently disables " +
			"ClientHoldCap and lets resume-pre-dispatch ack OK on a still-held connection")
	}
}

func TestApplyClientBrakes_PreDispatchStillWorksAlone(t *testing.T) {
	cfg := relay.Config{}
	sess := &supervisor.Session{}

	applyClientBrakes(&cfg, sess, &brakeParser{preDispatch: true}, zap.NewNop(), integrations.POSTGRES_V2)

	if !cfg.PreDispatchPause {
		t.Error("PreDispatchPause was lost; postgres depends on it and the hold must not disturb it")
	}
	if cfg.HoldClientWrites || sess.ClientWritesHeld {
		t.Error("a pre-dispatch parser was given a client write hold it never asked for")
	}
}

// MySQL is the parser the hold was built for. If this ever returns
// false the e2e MySQL-over-TLS lanes go back to recording a
// desynchronised upstream, so pin it here rather than relying on the
// dispatcher tests above to notice.
func TestMySQLAsksForTheClientWriteHold(t *testing.T) {
	var parser integrations.Integrations = mysql.New(zap.NewNop())

	hp, ok := parser.(interface{ WantsClientWriteHold() bool })
	if !ok {
		t.Fatal("MySQL no longer implements WantsClientWriteHold; the dispatcher probe is " +
			"duck-typed, so dropping the method silently disables the hold rather than " +
			"failing to compile")
	}
	if !hp.WantsClientWriteHold() {
		t.Error("MySQL declines the client write hold; its CLIENT_SSL upgrade leaks the " +
			"ClientHello upstream without it")
	}
}
