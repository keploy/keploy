package models

import (
	"encoding/json"
	"testing"
)

// The rename crosses five repositories that merge and deploy independently, so
// every combination of "producer on one spelling, consumer on the other" has to
// work. These pin that matrix.
//
// The failure this guards against is SILENT: encoding/json and yaml drop
// unknown keys without error, so a producer setting only the spelling its
// consumer does not know about yields a toggle that does nothing, with no
// diagnostic anywhere. A test is the only place that shows up.
func TestOutgoingOptionsNoiseAccessorsReadEitherSpelling(t *testing.T) {
	cases := []struct {
		name       string
		opts       OutgoingOptions
		wantDetect bool
		wantStrict bool
	}{
		{"unset", OutgoingOptions{}, false, false},
		{"canonical only", OutgoingOptions{MockNoiseDetection: true, MockNoiseStrict: true}, true, true},
		// The pre-rename producer: an older enterprise or k8s-proxy build.
		{"deprecated only", OutgoingOptions{SchemaNoiseDetection: true, SchemaNoiseStrict: true}, true, true},
		{"both", OutgoingOptions{MockNoiseDetection: true, SchemaNoiseDetection: true}, true, false},
		{"detection without strict", OutgoingOptions{MockNoiseDetection: true}, true, false},
		{"strict via the deprecated field only", OutgoingOptions{SchemaNoiseStrict: true}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.NoiseDetection(); got != tc.wantDetect {
				t.Errorf("NoiseDetection() = %v, want %v", got, tc.wantDetect)
			}
			if got := tc.opts.NoiseStrict(); got != tc.wantStrict {
				t.Errorf("NoiseStrict() = %v, want %v", got, tc.wantStrict)
			}
		})
	}
}

// NormalizeMockNoise must make BOTH spellings agree, not just populate the
// canonical one: the value travels onward to consumers that may still read the
// deprecated field directly (another repo, an older build), so leaving that
// field false would drop the toggle on exactly the hop this shim exists for.
func TestNormalizeMockNoiseFillsBothDirections(t *testing.T) {
	t.Run("deprecated in, canonical out", func(t *testing.T) {
		o := OutgoingOptions{SchemaNoiseDetection: true, SchemaNoiseStrict: true}
		o.NormalizeMockNoise()
		if !o.MockNoiseDetection || !o.MockNoiseStrict {
			t.Fatalf("canonical fields not populated from the deprecated ones: %+v", o)
		}
	})

	t.Run("canonical in, deprecated out", func(t *testing.T) {
		o := OutgoingOptions{MockNoiseDetection: true, MockNoiseStrict: true}
		o.NormalizeMockNoise()
		if !o.SchemaNoiseDetection || !o.SchemaNoiseStrict {
			t.Fatalf("deprecated mirror not populated; an unmigrated consumer downstream would see the toggle off: %+v", o)
		}
	})

	t.Run("off stays off", func(t *testing.T) {
		o := OutgoingOptions{}
		o.NormalizeMockNoise()
		if o.MockNoiseDetection || o.SchemaNoiseDetection || o.MockNoiseStrict || o.SchemaNoiseStrict {
			t.Fatalf("normalize turned a toggle on from nothing: %+v", o)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		// Compared field-by-field: OutgoingOptions carries slices, so it is not
		// a comparable type.
		o := OutgoingOptions{SchemaNoiseDetection: true}
		o.NormalizeMockNoise()
		d1, s1 := o.MockNoiseDetection, o.MockNoiseStrict
		dd1, ds1 := o.SchemaNoiseDetection, o.SchemaNoiseStrict
		o.NormalizeMockNoise()
		if o.MockNoiseDetection != d1 || o.MockNoiseStrict != s1 ||
			o.SchemaNoiseDetection != dd1 || o.SchemaNoiseStrict != ds1 {
			t.Fatalf("second normalize changed the value: %+v", o)
		}
	})
}

// Nil receivers: the accessors are called on options reached through pointers
// in several paths, and a panic there would be a worse outcome than the toggle
// being off.
func TestOutgoingOptionsNoiseAccessorsAreNilSafe(t *testing.T) {
	var o *OutgoingOptions
	if o.NoiseDetection() || o.NoiseStrict() {
		t.Error("nil options reported noise enabled")
	}
	o.NormalizeMockNoise() // must not panic
}

// TestCanonicalOnlyOptionsStillReachAPreRenameAgent is the row the original
// compatibility matrix was missing: NEW producer -> OLD consumer.
//
// OutgoingOptions has no json tags, so it crosses the agent boundary under Go
// FIELD NAMES. An agent image built before the rename decodes SchemaNoise* and
// has never heard of MockNoise*. Every producer in this repo now sets only the
// canonical pair, so without a mirror before the marshal the deprecated fields
// go out as false and the toggle is silently dead on that agent — including for
// users who passed the still-supported --schema-noise-detection, because the
// accessor collapses both spellings into the canonical field.
//
// That skew is the normal state during a rollout rather than an edge case:
// agent images are pinned separately from the CLI (k8s-proxy sets
// proxy.keployAgentImage, enterprise carries its own keploy pin), so a new CLI
// meets an old agent for as long as the rollout takes.
//
// The tests either side of this one all exercise producer and consumer at the
// SAME version, which is why they stayed green while this direction was broken.
func TestCanonicalOnlyOptionsStillReachAPreRenameAgent(t *testing.T) {
	// preRenameAgentOptions is the shape an agent built before the rename
	// decodes into — the deprecated pair and nothing else.
	type preRenameAgentOptions struct {
		SchemaNoiseDetection bool
		SchemaNoiseStrict    bool
	}

	// What a producer builds today: canonical only.
	sent := OutgoingOptions{MockNoiseDetection: true, MockNoiseStrict: true}

	// The mirror the wire boundary applies (platform/http/agent.go).
	sent.NormalizeMockNoise()

	wire, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var old preRenameAgentOptions
	if err := json.Unmarshal(wire, &old); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !old.SchemaNoiseDetection {
		t.Error("a pre-rename agent decoded detection=false from a canonical-only producer; the toggle is dead across version skew")
	}
	if !old.SchemaNoiseStrict {
		t.Error("a pre-rename agent decoded strict=false from a canonical-only producer; the toggle is dead across version skew")
	}
}

// And the reverse direction, for completeness: an OLD producer's payload must
// still drive a NEW agent. This one was already covered by the accessors, but
// pinning it next to its twin keeps the matrix visibly complete.
func TestDeprecatedOnlyWireStillDrivesANewAgent(t *testing.T) {
	type preRenameAgentOptions struct {
		SchemaNoiseDetection bool
		SchemaNoiseStrict    bool
	}

	wire, err := json.Marshal(preRenameAgentOptions{SchemaNoiseDetection: true, SchemaNoiseStrict: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var received OutgoingOptions
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	received.NormalizeMockNoise() // what Proxy.Record / Proxy.Mock do

	if !received.NoiseDetection() || !received.NoiseStrict() {
		t.Errorf("a pre-rename producer's payload did not drive a new agent: %+v", received)
	}
}
