package models

// The accessors and normaliser below exist because the schema-noise ->
// mock-noise rename crosses five repositories (keploy, integrations,
// enterprise, k8s-proxy, enterprise-ui) that merge and deploy independently.
//
// A package can be renamed with a type alias. A struct FIELD cannot: Go has no
// field aliases. So OutgoingOptions carries both spellings, and the failure
// mode of getting that wrong is silent — encoding/json and yaml drop unknown
// keys without error, so a producer on one spelling talking to a consumer on
// the other produces a toggle that does nothing, with no diagnostic anywhere.
// That is precisely the bug this pattern is here to prevent.
//
// Rule for callers: SET whichever spelling you like (prefer the canonical one),
// then let NormalizeMockNoise reconcile at the boundary; READ only through
// NoiseDetection() / NoiseStrict(), never off a field directly.

// NoiseDetection reports whether request-body drift detection is on, under
// either spelling. Nil-safe.
func (o *OutgoingOptions) NoiseDetection() bool {
	if o == nil {
		return false
	}
	return o.MockNoiseDetection || o.SchemaNoiseDetection
}

// NoiseStrict reports whether strict enforcement of learned request-body noise
// is on, under either spelling. Nil-safe.
func (o *OutgoingOptions) NoiseStrict() bool {
	if o == nil {
		return false
	}
	return o.MockNoiseStrict || o.SchemaNoiseStrict
}

// NormalizeMockNoise makes the two spellings agree, so everything downstream
// can read either one and get the same answer.
//
// OR rather than "canonical wins": the deprecated field is the ONLY one an
// unmigrated producer sets, so treating an unset canonical field as an
// instruction to turn the toggle off would discard that producer's intent.
// Enabling is the only direction either spelling can express — neither carries
// "explicitly off" distinct from "unset" — so OR loses nothing.
//
// Idempotent, so calling it more than once along a request path is free.
func (o *OutgoingOptions) NormalizeMockNoise() {
	if o == nil {
		return
	}
	detection, strict := o.NoiseDetection(), o.NoiseStrict()
	o.MockNoiseDetection, o.SchemaNoiseDetection = detection, detection
	o.MockNoiseStrict, o.SchemaNoiseStrict = strict, strict
}
