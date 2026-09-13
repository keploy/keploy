package models

import "testing"

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
