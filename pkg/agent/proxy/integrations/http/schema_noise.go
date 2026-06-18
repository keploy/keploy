package http

import (
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// httpNoiseAdapter is the HTTP implementation of schemanoise.Adapter, making
// HTTP a first-class client of the shared schema-noise engine (the same engine
// Pulsar and any future parser use). HTTP keeps its noise on HTTPReq.ReqBodyNoise
// (not the kind-agnostic MockSpec.ReqBodyNoise), and supplies a Diff that handles
// BOTH JSON and form-urlencoded bodies — the form path is HTTP's own concern, so
// it lives here in the adapter rather than leaking into the shared engine.
//
// HTTP does NOT use Engine.Learn: its learn carry-out (updateMock) clones the
// matched mock and routes the noise through DeleteFilteredMock/UpdateUnFilteredMock
// so the shared pooled mock is never mutated. SetLearnedNoise is implemented for
// interface completeness but is unused on the HTTP path.
type httpNoiseAdapter struct{}

// RecordedBody returns the recorded HTTP request body.
func (httpNoiseAdapter) RecordedBody(m *models.Mock) ([]byte, bool) {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil, false
	}
	return []byte(m.Spec.HTTPReq.Body), true
}

// StoredNoise returns the noise learned on this HTTP mock (HTTPReq.ReqBodyNoise).
func (httpNoiseAdapter) StoredNoise(m *models.Mock) map[string][]string {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil
	}
	return m.Spec.HTTPReq.ReqBodyNoise
}

// SetLearnedNoise writes merged noise back onto HTTPReq.ReqBodyNoise. Unused on
// the HTTP match path (see updateMock's copy-on-learn); present for completeness.
func (httpNoiseAdapter) SetLearnedNoise(m *models.Mock, merged map[string][]string) {
	if m == nil || m.Spec.HTTPReq == nil {
		return
	}
	m.Spec.HTTPReq.ReqBodyNoise = merged
}

// RecordedValueIsNoise excludes recorded values the enterprise obfuscator already
// redacted (recorded value matches a Mock.Noise regex) so secret fields are not
// re-flagged as schema noise.
func (httpNoiseAdapter) RecordedValueIsNoise(m *models.Mock) func(string) bool {
	if m == nil {
		return nil
	}
	nc := util.NewNoiseChecker(m.Noise)
	if nc == nil {
		return nil
	}
	return func(v string) bool { return nc.IsNoisy(v) }
}

// Diff compares the recorded vs live HTTP request body. JSON bodies use the
// shared DetectJSONDrift kernel; form-urlencoded bodies use HTTP's key-by-key
// form differ. Any other body (binary, plain text) has no field structure, so
// comparable=false and the engine falls back to byte equality. known arrives
// root-relative from the engine; the form differ wants "body."-prefixed keys, so
// it is re-prefixed for that branch only.
func (httpNoiseAdapter) Diff(m *models.Mock, recorded, live []byte, known map[string][]string, valIsNoise func(string) bool) (map[string][]string, bool) {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil, false
	}
	switch {
	case pkg.IsJSON(recorded) && pkg.IsJSON(live):
		return schemanoise.DetectJSONDrift(recorded, live, known, valIsNoise)
	case isFormURLEncoded(m.Spec.HTTPReq.Header):
		// Form bodies are comparable; formReqBodyNoise emits "body."-prefixed
		// drift and expects a "body."-prefixed known set.
		return formReqBodyNoise(string(recorded), string(live), schemanoise.AddBodyPrefix(known), valIsNoise), true
	default:
		return nil, false
	}
}
