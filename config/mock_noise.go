package config

// Mock-noise accessors for the user-facing config, mirroring the pair on
// models.OutgoingOptions.
//
// Two spellings exist here for the same reason they exist on the wire type: the
// rename crosses repositories and, more importantly here, it crosses TIME. A
// keploy.yml written before the rename says `schemaNoiseDetection`, and viper
// drops keys it does not recognise without complaining — so a user upgrading
// keploy would find the toggle silently stopped working, with a config file
// that still looks correct.
//
// Read via NoiseDetection() / NoiseStrict(); never off a field directly.

// NoiseDetection reports whether request-body drift detection is enabled under
// either spelling. Nil-safe.
func (t *Test) NoiseDetection() bool {
	if t == nil {
		return false
	}
	return t.MockNoiseDetection || t.SchemaNoiseDetection
}

// NoiseStrict reports whether strict enforcement of learned request-body noise
// is enabled under either spelling. Nil-safe.
func (t *Test) NoiseStrict() bool {
	if t == nil {
		return false
	}
	return t.MockNoiseStrict || t.SchemaNoiseStrict
}

// NormalizeMockNoise makes the two spellings agree so any reader gets the same
// answer. OR, not "canonical wins": the deprecated key is the only one an older
// keploy.yml sets, and neither spelling can express "explicitly off" as
// distinct from "unset", so OR discards nothing. Idempotent.
func (t *Test) NormalizeMockNoise() {
	if t == nil {
		return
	}
	detection, strict := t.NoiseDetection(), t.NoiseStrict()
	t.MockNoiseDetection, t.SchemaNoiseDetection = detection, detection
	t.MockNoiseStrict, t.SchemaNoiseStrict = strict, strict
}
