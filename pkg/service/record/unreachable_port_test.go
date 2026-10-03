package record

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A keploy.yml that sends replay somewhere other than test.host and each test's
// recorded port makes "can the host reach the recorded port?" the wrong
// question: replay will not dial it, so a warning about it is noise, or worse,
// advice to publish a port nobody uses. Each redirect setting on its own keeps
// the recording quiet and asks the instrumentation nothing; with none of them,
// it asks and warns.
func TestUnreachablePortWarnerAsksNothingWhenReplayIsRedirected(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Test)
	}{
		{"test.port", func(t *config.Test) { t.Port = 8080 }},
		{"test.grpcPort", func(t *config.Test) { t.GRPCPort = 50051 }},
		{"test.ssePort", func(t *config.Test) { t.SSEPort = 8047 }},
		{"a protocol port", func(t *config.Test) { t.Protocol = config.ProtocolConfig{"http": {Port: 8080}} }},
		{"replaceWith.global.url", func(t *config.Test) {
			t.ReplaceWith.Global.URL = map[string]string{"localhost:8096": "app.internal:8096"}
		}},
		{"replaceWith.global.port", func(t *config.Test) { t.ReplaceWith.Global.Port = map[uint32]uint32{8096: 18096} }},
		{"replaceWith.test-sets", func(t *config.Test) {
			t.ReplaceWith.TestSets = map[string]config.ReplaceWithMap{"test-set-0": {Port: map[uint32]uint32{8096: 18096}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			tc.set(&cfg.Test)
			instr := &unpublishedInstr{unpublished: map[uint16]string{8096: "it is not published on the host"}}
			core, logs := observer.New(zapcore.DebugLevel)

			w := newUnreachablePortWarner(zap.New(core), instr, cfg)
			w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})

			if len(instr.asked) != 0 {
				t.Errorf("asked %v; replay does not dial the recorded port", instr.asked)
			}
			if logs.Len() != 0 {
				t.Errorf("logged %v", logs.All())
			}
		})
	}

	// The control: nothing redirected, so it asks, and warns.
	instr := &unpublishedInstr{unpublished: map[uint16]string{8096: "it is not published on the host"}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := newUnreachablePortWarner(zap.New(core), instr, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	if len(instr.asked) != 1 || logs.FilterMessageSnippet("port 8096 cannot be replayed").Len() != 1 {
		t.Errorf("not redirected: asked %v, logged %v; want one question and one warning", instr.asked, logs.All())
	}
}
