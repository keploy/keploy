package mock

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// errStopReplay short-circuits Replay right after the proxy has been switched
// into mock-serving mode. Everything the flow does afterwards (StoreMocks, the
// wrapped runner, the consumed-mock summary) is irrelevant to this wiring pin
// and would need a far larger fake.
var errStopReplay = errors.New("stop after MockOutgoing")

// capturingInstrumentation records the OutgoingOptions literal that
// mockService.Replay hands to the proxy. That literal is the ONLY channel
// through which `keploy mock replay` can turn on strict request-body matching:
// the proxy builds its schemanoise.Engine from opts.SchemaNoiseStrict
// (pkg/agent/proxy/integrations/http/decode.go, match.go's
// filterStrictNoiseMatches), so a field left off the literal is a feature that
// cannot be reached from this command at all.
type capturingInstrumentation struct {
	Instrumentation // embedded nil: any unexpected call panics loudly

	got models.OutgoingOptions
}

func (c *capturingInstrumentation) Setup(context.Context, string, models.SetupOptions) error {
	return nil
}

func (c *capturingInstrumentation) MockOutgoing(_ context.Context, opts models.OutgoingOptions) error {
	c.got = opts
	return errStopReplay
}

func (c *capturingInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

// TestReplay_ForwardsSchemaNoiseStrictToTheProxy is the regression guard for
// `keploy mock replay` silently ignoring strict request-body matching.
//
// Without strict, the outgoing HTTP match cascade ends at PerformBodyMatch ->
// bodyMatch, whose whole comparison is "does the request body contain every
// top-level key the mock's body had". It never reads a VALUE, so a request that
// keeps its shape and changes a value is served the STALE recorded response
// with missed: 0 and exit 0.
//
// The enforcement (filterStrictNoiseMatches -> schemanoise.Engine.StrictReject)
// already exists and is reached only when opts.SchemaNoiseStrict is true. This
// call site built OutgoingOptions without the field, so setting
// test.schemaNoiseStrict: true in keploy.yml was a silent no-op for this
// command.
func TestReplay_ForwardsSchemaNoiseStrictToTheProxy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
	}{
		{"strict off stays off", false},
		{"strict on reaches the proxy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &capturingInstrumentation{}
			cfg := &config.Config{Command: "echo hi"}
			cfg.Mock.Name = "my-set"
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Test.SchemaNoiseStrict = tc.strict

			svc := New(zap.NewNop(), inst, nil, nil, nil, nil, cfg)
			if err := svc.Replay(context.Background()); !errors.Is(err, errStopReplay) {
				t.Fatalf("Replay returned %v, want the post-MockOutgoing sentinel", err)
			}

			if inst.got.SchemaNoiseStrict != tc.strict {
				t.Fatalf("OutgoingOptions.SchemaNoiseStrict = %v, want %v — "+
					"`mock replay` is not forwarding it, so the strict request-body "+
					"matcher can never engage and a drifted request value is served "+
					"the stale recorded response",
					inst.got.SchemaNoiseStrict, tc.strict)
			}
		})
	}
}

// TestReplay_ForwardsRequestBodyNoiseOnlyWithStrict pins the escape hatch and
// the compatibility guarantee in one place.
//
// Escape hatch: with strict ON, test.globalNoise.requestbody must reach the
// proxy, because StrictReject subtracts the known-noise paths before rejecting
// — it is the only way to keep a legitimately-varying field (nonce, timestamp,
// request id) from failing every match.
//
// Compatibility: with strict OFF, NoiseConfig must stay nil. It also feeds
// header noise (decode.go's headerNoise), so forwarding it unconditionally
// would loosen header matching for every existing user who has a globalNoise
// block — a behaviour change on the default path.
func TestReplay_ForwardsRequestBodyNoiseOnlyWithStrict(t *testing.T) {
	newCfg := func(strict bool) *config.Config {
		cfg := &config.Config{Command: "echo hi"}
		cfg.Mock.Name = "my-set"
		cfg.Mock.OnMiss = string(models.MissFail)
		cfg.Test.SchemaNoiseStrict = strict
		cfg.Test.GlobalNoise.Global = config.GlobalNoise{
			"requestbody": {"nonce": {}},
			"header":      {"authorization": {}},
		}
		return cfg
	}

	t.Run("strict on forwards the noise buckets", func(t *testing.T) {
		inst := &capturingInstrumentation{}
		if err := New(zap.NewNop(), inst, nil, nil, nil, nil, newCfg(true)).
			Replay(context.Background()); !errors.Is(err, errStopReplay) {
			t.Fatalf("Replay returned %v, want the post-MockOutgoing sentinel", err)
		}
		if _, ok := inst.got.NoiseConfig["requestbody"]["nonce"]; !ok {
			t.Fatalf("requestbody noise did not reach the proxy: %#v — without it "+
				"strict mode rejects every mock whose nonce field drifted",
				inst.got.NoiseConfig)
		}
	})

	t.Run("strict off leaves NoiseConfig nil", func(t *testing.T) {
		inst := &capturingInstrumentation{}
		if err := New(zap.NewNop(), inst, nil, nil, nil, nil, newCfg(false)).
			Replay(context.Background()); !errors.Is(err, errStopReplay) {
			t.Fatalf("Replay returned %v, want the post-MockOutgoing sentinel", err)
		}
		if inst.got.NoiseConfig != nil {
			t.Fatalf("NoiseConfig = %#v with strict off, want nil — forwarding it "+
				"unconditionally also loosens header matching on the default path",
				inst.got.NoiseConfig)
		}
	})
}
