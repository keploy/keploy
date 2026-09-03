package config

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	yaml3 "gopkg.in/yaml.v3"
)

// TestDefaultConfigParses pins that the embedded default config string is
// valid YAML that unmarshals cleanly into Config.
//
// New() panics on a parse error, and it runs on every keploy invocation — so a
// typo in defaultConfig is not a quiet mistake, it is a hard crash on startup
// for every user. The duration fields are the sharp edge: they are written as
// strings ("2s", "60s") and decoded by yaml.v3 through time.ParseDuration, so
// an English-looking value such as "2 seconds" is accepted by a YAML linter
// and rejected only at decode time.
func TestDefaultConfigParses(t *testing.T) {
	t.Parallel()

	var cfg *Config
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("New() panicked on the default config: %v", r)
			}
		}()
		cfg = New()
	}()

	// Every duration in the default config, with the value the string encodes.
	// A mis-typed unit decodes to zero rather than failing loudly for some
	// shapes, so assert the values, not just that parsing succeeded.
	durations := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"test.healthPollTimeout", cfg.Test.HealthPollTimeout, DefaultHealthPollTimeout},
		{"record.recordTimer", cfg.Record.RecordTimer, 0},
		// Asserted against the relay's own constant rather than a literal: the
		// yaml default and relay.DefaultConsumerStallGrace are two sources of
		// truth for the same number, and nothing else makes them agree. Tying
		// them here means changing one without the other fails a test instead
		// of silently shipping a keploy.yml that overrides the package default.
		{"record.recordBuffer.consumerStallGrace", cfg.Record.RecordBuffer.ConsumerStallGrace, relay.DefaultConsumerStallGrace},
	}
	for _, d := range durations {
		if d.got != d.want {
			t.Errorf("%s = %v, want %v", d.name, d.got, d.want)
		}
	}

	// The two sibling record-buffer knobs, so this test fails if the block is
	// dropped or renamed rather than silently defaulting to zero everywhere.
	if got, want := cfg.Record.RecordBuffer.MaxMemoryPerConnection, uint64(64*1024*1024); got != want {
		t.Errorf("record.recordBuffer.maxMemoryPerConnection = %d, want %d", got, want)
	}
	if got, want := cfg.Record.RecordBuffer.QueueSize, 1024; got != want {
		t.Errorf("record.recordBuffer.queueSize = %d, want %d", got, want)
	}

	// record.upstreamTls is a nested block inside another nested block, the
	// shape a stray indent in the YAML string literal silently reparents. If
	// the block ends up under recordBuffer (or at the top level) these fields
	// stay zero-valued and nothing else in the codebase notices — the feature
	// just never turns on. Asserting the defaults pins the block's position.
	//
	// verify MUST default to false: keploy is never stricter than the app it
	// records. Flipping this default is a behaviour change for every existing
	// user (apps on sslmode=require / tls=skip-verify start failing, and the
	// failure is a silently dropped mock), so it should not be possible to do
	// it by accident.
	if cfg.Record.UpstreamTLS.Verify {
		t.Error("record.upstreamTls.verify defaults to true; it must default to false so keploy is never stricter than the application it records")
	}
	if got := cfg.Record.UpstreamTLS.CACert; got != "" {
		t.Errorf("record.upstreamTls.caCert = %q, want empty", got)
	}

	// Present in the generated config so the escape hatch is
	// discoverable, and false so nothing changes for anyone who does not
	// need it. A readiness probe is destructive only in specific
	// environments (an app behind a kubectl port-forward); everywhere
	// else it is the thing preventing status_code=0 flakes.
	if cfg.Test.DisableAppReadyProbe {
		t.Error("test.disableAppReadyProbe defaults to true; it must default to false or every " +
			"app silently loses the readiness gate that stops the first test firing early")
	}
}

// TestDefaultConfigUpstreamTLSBlockPosition pins WHERE the upstreamTls block
// sits in the YAML template, which the zero-value assertions in
// TestDefaultConfigParses cannot: an over-indented block reparents under
// recordBuffer and an under-indented one lands at the document root, and in
// both cases record.upstreamTls just stays zero — the same values a correctly
// placed block produces today. Walking the raw document is the only way to tell
// "the key is where users will write it" from "the key is nowhere".
func TestDefaultConfigUpstreamTLSBlockPosition(t *testing.T) {
	t.Parallel()

	var doc map[string]any
	if err := yaml3.Unmarshal([]byte(GetDefaultConfig()), &doc); err != nil {
		t.Fatalf("default config is not valid YAML: %v", err)
	}

	record, ok := doc["record"].(map[string]any)
	if !ok {
		t.Fatalf("default config has no `record:` mapping (got %T)", doc["record"])
	}
	upstream, ok := record["upstreamTls"].(map[string]any)
	if !ok {
		t.Fatalf("record.upstreamTls is not a mapping (got %T) — check the indentation of the block in defaultConfig", record["upstreamTls"])
	}
	if verify, ok := upstream["verify"].(bool); !ok || verify {
		t.Errorf("record.upstreamTls.verify = %v (%T), want false", upstream["verify"], upstream["verify"])
	}
	if caCert, ok := upstream["caCert"].(string); !ok || caCert != "" {
		t.Errorf("record.upstreamTls.caCert = %v (%T), want an empty string", upstream["caCert"], upstream["caCert"])
	}
}
