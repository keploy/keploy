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
