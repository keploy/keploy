package replayer

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// mysqlNoiseAdapter makes the MySQL COM_QUERY matcher a first-class client of
// the shared schema-noise engine (the same engine HTTP and Pulsar use). Like
// every parser, MySQL stores its learned noise on the kind-agnostic
// MockSpec.ReqBodyNoise, so it inherits #4286's transport (MockState.ReqBodyNoise),
// persistence (mockdb UpdateMocks/PersistMockNoise) and YAML/gob round-trip for
// free — no MySQL-specific QueryNoise field, MockState channel, or merge helper.
//
// The adapter owns only the genuinely protocol-specific piece — the SQL literal
// comparison (Diff), which delegates to querynoise.go's clause-aware extractor.
//
// IMPORTANT — strict is NOT routed through the engine. Engine.StrictAllows
// allows any mock with no learned noise ("strict only tightens what was
// learned", correct for HTTP's post-body-match filter shape). MySQL reaches the
// structure-equal branch precisely WHEN literals drift, and its eligibility is
// asymmetric (SET/VALUES learnable; WHERE/predicate never), which a single
// symmetric Diff cannot feed to both detection and strict. So Diff carries
// DETECTION semantics only, and the matcher enforces strict via the bespoke
// queryMatchesWithinNoise (see match.go).
type mysqlNoiseAdapter struct{}

// Compile-time guarantee that MySQL satisfies the shared schema-noise contract.
var _ schemanoise.Adapter = mysqlNoiseAdapter{}

// RecordedBody returns the recorded COM_QUERY SQL text for the mock's first
// MySQL request. ok=false for a mock whose first request is not a plaintext
// query packet (e.g. a prepared statement or a non-COM_QUERY mock), which
// short-circuits detection.
func (mysqlNoiseAdapter) RecordedBody(m *models.Mock) ([]byte, bool) {
	if m == nil || len(m.Spec.MySQLRequests) == 0 {
		return nil, false
	}
	qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
	if !ok || qp == nil {
		return nil, false
	}
	return []byte(qp.Query), true
}

// StoredNoise returns the noise learned on this mock (kind-agnostic
// MockSpec.ReqBodyNoise), "body."-prefixed.
func (mysqlNoiseAdapter) StoredNoise(m *models.Mock) map[string][]string {
	if m == nil {
		return nil
	}
	return m.Spec.ReqBodyNoise
}

// SetLearnedNoise writes merged noise back onto MockSpec.ReqBodyNoise. The
// MySQL match path attaches detected noise via updateMock (copy-on-learn so the
// shared pooled mock is never mutated), so this is unused on that path; it is
// present for interface completeness and any direct Engine.Learn caller.
func (mysqlNoiseAdapter) SetLearnedNoise(m *models.Mock, merged map[string][]string) {
	if m == nil {
		return
	}
	m.Spec.ReqBodyNoise = merged
}

// RecordedValueIsNoise has no MySQL analogue (no value-regex obfuscation on SQL
// literals), so it returns nil.
func (mysqlNoiseAdapter) RecordedValueIsNoise(*models.Mock) func(string) bool { return nil }

// Diff implements the engine's DETECTION comparison for COM_QUERY: the eligible
// (SET/VALUES) literal positions that drifted between the recorded and live SQL
// and are NOT already learned, emitted in the shared "body."-prefixed
// vocabulary. comparable reflects querynoise.go's authoritative redacted-skeleton
// gate — false when the two statements differ in any non-literal way (table,
// column, operator, clause shape) or fail to parse, in which case the engine
// treats any byte difference as a real, non-learnable mismatch.
//
// known arrives root-relative from the engine (the mock's stored noise with the
// "body." prefix stripped, unioned with user noise), matching the raw clause-
// role keys detectQueryNoise emits, so subtraction is a direct key lookup.
func (mysqlNoiseAdapter) Diff(_ *models.Mock, recorded, live []byte, known map[string][]string, _ func(string) bool) (map[string][]string, bool) {
	raw, comparable := detectQueryNoise(string(recorded), string(live))
	if !comparable {
		return nil, false
	}
	if len(raw) == 0 {
		return nil, true
	}
	out := make(map[string][]string, len(raw))
	for k, v := range raw {
		if _, seen := known[k]; seen {
			continue // already learned (or user-configured) — not new drift
		}
		out["body."+k] = v
	}
	return out, true
}
