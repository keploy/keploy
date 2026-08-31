package models

import "testing"

// The mock-noise flags are carried on two field pairs because Go has no field
// aliases (see the NAMING note on OutgoingOptions). These tests pin the two
// directions that keep the rename source-compatible across module boundaries.

func TestSetMockNoiseWritesBothPairs(t *testing.T) {
	// A producer on the new API must remain visible to a consumer still
	// reading the deprecated names — that is the whole point of the pair.
	var o OutgoingOptions
	o.SetMockNoise(true, true)

	if !o.MockNoiseDetection || !o.MockNoiseStrict {
		t.Fatalf("canonical pair not set: detection=%v strict=%v",
			o.MockNoiseDetection, o.MockNoiseStrict)
	}
	if !o.SchemaNoiseDetection || !o.SchemaNoiseStrict {
		t.Fatalf("deprecated mirror not set: detection=%v strict=%v",
			o.SchemaNoiseDetection, o.SchemaNoiseStrict)
	}

	// Clearing must clear both too, or a disabled flag would stay latched on
	// for whichever consumer reads the other pair.
	o.SetMockNoise(false, false)
	if o.MockNoiseDetection || o.MockNoiseStrict || o.SchemaNoiseDetection || o.SchemaNoiseStrict {
		t.Fatalf("clearing left a flag set: %+v", o)
	}
}

func TestNormalizeMockNoiseFoldsEitherDirection(t *testing.T) {
	cases := []struct {
		name string
		in   OutgoingOptions
	}{{
		// An out-of-tree producer that predates the rename: it only knows
		// the deprecated names, but the parsers read the canonical ones.
		name: "deprecated only",
		in:   OutgoingOptions{SchemaNoiseDetection: true, SchemaNoiseStrict: true},
	}, {
		// The reverse: a new producer talking to a consumer still on the
		// old names.
		name: "canonical only",
		in:   OutgoingOptions{MockNoiseDetection: true, MockNoiseStrict: true},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.in
			o.NormalizeMockNoise()
			if !o.MockNoiseDetection || !o.MockNoiseStrict {
				t.Fatalf("canonical pair not folded: %+v", o)
			}
			if !o.SchemaNoiseDetection || !o.SchemaNoiseStrict {
				t.Fatalf("deprecated pair not folded: %+v", o)
			}

			// Idempotent: normalizing again must not flip anything back.
			// OutgoingOptions holds slices and so is not comparable; the
			// four flags are the only thing this call touches.
			before := [4]bool{o.MockNoiseDetection, o.MockNoiseStrict, o.SchemaNoiseDetection, o.SchemaNoiseStrict}
			o.NormalizeMockNoise()
			after := [4]bool{o.MockNoiseDetection, o.MockNoiseStrict, o.SchemaNoiseDetection, o.SchemaNoiseStrict}
			if before != after {
				t.Fatalf("not idempotent:\n first: %v\nsecond: %v", before, after)
			}
		})
	}
}

func TestNormalizeMockNoiseLeavesUnsetFlagsOff(t *testing.T) {
	// The fold is an OR, so it must not manufacture a flag nobody asked for —
	// otherwise every replay would silently run with detection enabled.
	var o OutgoingOptions
	o.NormalizeMockNoise()
	if o.MockNoiseDetection || o.MockNoiseStrict || o.SchemaNoiseDetection || o.SchemaNoiseStrict {
		t.Fatalf("normalize turned a flag on from zero value: %+v", o)
	}

	// A half-set pair must fold only the flag that was set.
	partial := OutgoingOptions{SchemaNoiseStrict: true}
	partial.NormalizeMockNoise()
	if partial.MockNoiseDetection || partial.SchemaNoiseDetection {
		t.Fatalf("detection leaked on from a strict-only producer: %+v", partial)
	}
	if !partial.MockNoiseStrict || !partial.SchemaNoiseStrict {
		t.Fatalf("strict not folded: %+v", partial)
	}
}
