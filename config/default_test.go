package config

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
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
		{"test.healthPollTimeout", cfg.Test.HealthPollTimeout, 60 * time.Second},
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
}
