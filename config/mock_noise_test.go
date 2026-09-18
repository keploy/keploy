package config

import "testing"

// An existing keploy.yml says `schemaNoiseDetection`. Viper drops keys it does
// not recognise silently, so if the rename removed that spelling the toggle
// would stop working on upgrade while the config file still looked correct —
// no error, no warning, just a feature that quietly went away.
func TestTestNoiseAccessorsReadEitherSpelling(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Test
		wantDetect bool
		wantStrict bool
	}{
		{"unset", Test{}, false, false},
		{"canonical", Test{MockNoiseDetection: true, MockNoiseStrict: true}, true, true},
		{"pre-rename keploy.yml", Test{SchemaNoiseDetection: true, SchemaNoiseStrict: true}, true, true},
		{"mixed", Test{MockNoiseDetection: true, SchemaNoiseStrict: true}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.NoiseDetection(); got != tc.wantDetect {
				t.Errorf("NoiseDetection() = %v, want %v", got, tc.wantDetect)
			}
			if got := tc.cfg.NoiseStrict(); got != tc.wantStrict {
				t.Errorf("NoiseStrict() = %v, want %v", got, tc.wantStrict)
			}
		})
	}
}

func TestTestNormalizeMockNoiseFillsBothDirections(t *testing.T) {
	c := Test{SchemaNoiseDetection: true}
	c.NormalizeMockNoise()
	if !c.MockNoiseDetection {
		t.Error("canonical field not populated from the deprecated one")
	}

	c = Test{MockNoiseStrict: true}
	c.NormalizeMockNoise()
	if !c.SchemaNoiseStrict {
		t.Error("deprecated mirror not populated; a consumer still reading it would see strict off")
	}

	c = Test{}
	c.NormalizeMockNoise()
	if c.NoiseDetection() || c.NoiseStrict() {
		t.Error("normalize enabled a toggle that nothing had set")
	}
}

func TestTestNoiseAccessorsAreNilSafe(t *testing.T) {
	var c *Test
	if c.NoiseDetection() || c.NoiseStrict() {
		t.Error("nil config reported noise enabled")
	}
	c.NormalizeMockNoise()
}

// TestYamlOnlyNoiseKeysAreNotClobberedByFlagDefaults documents the ordering
// hazard that ValidateFlags has to guard against, and pins the invariant the
// guard exists to preserve.
//
// The sequence is: AddFlags registers each flag with a default read from a ZERO
// config; PreProcessFlags then runs viper.Unmarshal, which fills the config
// from keploy.yml; only then does ValidateFlags read the flags back. An
// unguarded `cfg.Test.X = flags.GetBool("x")` therefore overwrites the value
// keploy.yml supplied with the flag's stale default — the toggle silently stops
// working while the config file still looks correct.
//
// ValidateFlags guards both spellings on Changed||!viper.IsSet. This test pins
// the second half of that contract: once a value HAS been resolved, normalizing
// must not disturb it in either direction.
func TestYamlOnlyNoiseKeysAreNotClobberedByFlagDefaults(t *testing.T) {
	t.Run("yaml-only canonical key survives normalize", func(t *testing.T) {
		// What the guard leaves behind: viper filled the canonical field, the
		// flag was not passed, so the deprecated one is still false.
		c := Test{MockNoiseDetection: true}
		c.NormalizeMockNoise()
		if !c.NoiseDetection() {
			t.Fatal("a keploy.yml-only mockNoiseDetection:true did not survive")
		}
	})

	t.Run("yaml-only deprecated key survives normalize", func(t *testing.T) {
		c := Test{SchemaNoiseDetection: true}
		c.NormalizeMockNoise()
		if !c.NoiseDetection() {
			t.Fatal("a keploy.yml-only schemaNoiseDetection:true did not survive")
		}
	})

	t.Run("an explicit false is not turned on by the other spelling's zero value", func(t *testing.T) {
		// Both false is what "user said false" looks like after resolution;
		// normalize must leave it alone rather than read the unset field as
		// permission to enable.
		c := Test{MockNoiseDetection: false, SchemaNoiseDetection: false}
		c.NormalizeMockNoise()
		if c.NoiseDetection() {
			t.Fatal("normalize enabled detection that nothing had asked for")
		}
	})
}
