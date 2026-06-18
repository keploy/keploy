// Package schemanoise hosts the kind-agnostic schema-noise learn/enforce engine
// shared by every protocol integration.
//
// Schema-noise ("req_body_noise") is the field-path noise keploy auto-learns
// during schema-based auto-replay (config.Test.SchemaNoiseDetection): when a
// recorded request body and the live one differ on a field, the drifting field
// path is recorded as noise on the mock so a later strict replay tolerates it
// instead of failing. Historically this whole flow lived inside the HTTP
// integration; a second protocol (Pulsar) had to copy the flag handling,
// known-noise merge, JSON diffing, strict rejection and learn-merge wholesale.
//
// This package owns that algorithm exactly once. A protocol becomes a client by
// implementing the small Adapter interface — supplying only the protocol-
// specific bits (how to read the recorded request body and where its learned
// noise is stored). Everything else is provided by Engine. A brand-new parser
// (Kafka, Redis, …) gains detection + strict enforcement by implementing
// Adapter and nothing more, which is the "plug and play for the rest of the
// parsers" goal.
//
// Vocabulary: stored/learned noise uses the shared "body."-prefixed field-path
// form ("body.user.id" -> regex list, empty list == "ignore the whole field"),
// identical to HTTPReq.ReqBodyNoise, MockSpec.ReqBodyNoise and
// MockFieldDiff.Path. The JSON matcher works root-relative, so the engine
// strips the prefix before diffing and re-adds it when emitting learned noise.
package schemanoise

import (
	"encoding/json"
	"maps"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// Adapter is the per-protocol contract for plugging into the shared schema-noise
// engine. Implementations are tiny: they expose where the recorded request body
// lives and where this protocol keeps its learned noise. The engine owns flag
// handling, the known-noise merge, JSON field diffing, strict rejection and the
// monotonic learn-merge — none of which a new protocol should reimplement.
type Adapter interface {
	// RecordedBody returns the comparable recorded request-body bytes for the
	// mock — the thing live traffic is diffed against — and whether the mock
	// carries a usable body at all (ok=false short-circuits detection/strict).
	// For HTTP this is HTTPReq.Body; for Pulsar it is the decoded SEND payload.
	RecordedBody(m *models.Mock) (body []byte, ok bool)

	// StoredNoise returns the noise already recorded on the mock in the shared
	// "body."-prefixed vocabulary: HTTPReq.ReqBodyNoise for HTTP, the kind-
	// agnostic MockSpec.ReqBodyNoise for everything else.
	StoredNoise(m *models.Mock) map[string][]string

	// SetLearnedNoise writes the merged learned noise back onto the mock in the
	// protocol's storage location (the inverse of StoredNoise). Called by
	// Engine.Learn after merging newly-detected drift.
	SetLearnedNoise(m *models.Mock, merged map[string][]string)

	// RecordedValueIsNoise returns a predicate consulted with the recorded
	// (expected) scalar value of each changed field; returning true drops that
	// field so values already covered by the mock's value-regex noise (e.g.
	// enterprise-obfuscated secrets) are not re-flagged as schema noise.
	// Implementations with no such concept return nil.
	RecordedValueIsNoise(m *models.Mock) func(string) bool

	// Diff is the parser-owned schema comparison — the one piece that is
	// inherently protocol-specific, just like each parser owns its
	// buildXMismatchReport. It returns the "body."-prefixed field paths that
	// drifted between the recorded and live request bodies and are NOT already
	// in known (root-relative: global/user ∪ already-learned, prefix-stripped),
	// plus whether the payload had any diffable schema at all.
	//
	// comparable=false means the payload is byte-opaque (no field structure):
	// the engine then treats any byte difference as a real, non-learnable
	// mismatch instead of "nothing drifted". A JSON-payload parser embeds
	// JSONDiffer to get this for free; a binary/structured parser (Avro,
	// Protobuf, RESP, BSON) supplies its own. m is provided for parsers that
	// need headers/metadata to pick a decoder (e.g. HTTP JSON vs form); JSON-
	// only parsers ignore it.
	Diff(m *models.Mock, recorded, live []byte, known map[string][]string, valIsNoise func(string) bool) (drift map[string][]string, comparable bool)
}

// JSONDiffer is the ready-made JSON implementation of Adapter.Diff. A parser
// whose request body is JSON embeds it and inherits Diff with no extra code:
//
//	type myAdapter struct{ schemanoise.JSONDiffer }
//
// so the only parser-specific methods left to implement are RecordedBody /
// StoredNoise / SetLearnedNoise / RecordedValueIsNoise.
type JSONDiffer struct{}

// Diff implements Adapter.Diff for JSON bodies via DetectJSONDrift. The mock is
// unused — JSON needs no header/metadata to decode.
func (JSONDiffer) Diff(_ *models.Mock, recorded, live []byte, known map[string][]string, valIsNoise func(string) bool) (map[string][]string, bool) {
	return DetectJSONDrift(recorded, live, known, valIsNoise)
}

// Engine runs the schema-noise learn/enforce flow for one protocol through its
// Adapter. Construct one per replay with the resolved flags via New.
type Engine struct {
	adapter   Adapter
	detection bool // config.Test.SchemaNoiseDetection
	strict    bool // config.Test.SchemaNoiseStrict
}

// New builds an Engine for an adapter with the resolved schema-noise flags
// (config.Test.SchemaNoiseDetection / SchemaNoiseStrict).
func New(a Adapter, detection, strict bool) *Engine {
	return &Engine{adapter: a, detection: detection, strict: strict}
}

// DetectionEnabled reports whether auto-learning is on. nil-safe.
func (e *Engine) DetectionEnabled() bool { return e != nil && e.detection }

// StrictEnabled reports whether strict enforcement is on. nil-safe.
func (e *Engine) StrictEnabled() bool { return e != nil && e.strict }

// KnownNoise is the root-relative known-noise set for a mock: the global/user
// body noise (already root-relative) unioned with the noise already learned on
// the mock (prefix-stripped). Diffing against this set means only NEW drift —
// beyond global noise and anything a prior auto-replay round already learned —
// is ever surfaced. It is also the noise set a protocol's matcher should consult
// so a field once learned as noise never re-triggers a mismatch on a later
// (including strict) replay.
func (e *Engine) KnownNoise(m *models.Mock, userNoise map[string][]string) map[string][]string {
	if e == nil {
		return userNoise
	}
	return MergeKnown(userNoise, StripBodyPrefix(e.adapter.StoredNoise(m)))
}

// Detect returns the new "body."-prefixed schema-noise to record on the mock for
// a live request body, plus whether the bodies were comparable (both JSON).
//
// It is a no-op (nil, false) when detection is disabled or the mock has no
// recorded body. When comparable is false the bodies have no field structure to
// diff, so the caller MUST treat any raw difference as a real, non-learnable
// mismatch — never as "nothing drifted, pass". That contract is what stops
// lenient detection from silently accepting binary/plain-text/Avro/protobuf
// payload drift. The caller records the returned drift via its consume ->
// MockState -> mockdb persistence path (or directly via Learn).
func (e *Engine) Detect(m *models.Mock, liveBody []byte, userNoise map[string][]string) (drift map[string][]string, comparable bool) {
	if !e.DetectionEnabled() {
		return nil, false
	}
	recorded, ok := e.adapter.RecordedBody(m)
	if !ok {
		return nil, false
	}
	return e.adapter.Diff(m, recorded, liveBody, e.KnownNoise(m, userNoise), e.adapter.RecordedValueIsNoise(m))
}

// Learn merges newly-detected drift into the mock's stored noise (monotonic —
// existing entries win) and writes it back through the adapter. Returns the
// number of newly-added field paths; 0 (and no write) when nothing is new.
func (e *Engine) Learn(m *models.Mock, drift map[string][]string) int {
	if e == nil || len(drift) == 0 {
		return 0
	}
	existing := e.adapter.StoredNoise(m)
	merged := MergeLearned(existing, drift)
	added := len(merged) - len(existing)
	if added <= 0 {
		return 0
	}
	e.adapter.SetLearnedNoise(m, merged)
	return added
}

// StrictAllows reports whether mock m may still match the live body under strict
// enforcement. A mock with no learned noise is always allowed — strict only
// tightens what was explicitly learned, never mocks the auto-replay never
// touched. A mock WITH learned noise is rejected when a field OUTSIDE that
// learned set drifted (an unmarked change), or when the bodies aren't JSON-
// comparable yet differ byte-for-byte (a non-learnable mismatch).
func (e *Engine) StrictAllows(m *models.Mock, liveBody []byte, userNoise map[string][]string) bool {
	if e == nil {
		return true
	}
	stored := e.adapter.StoredNoise(m)
	if len(stored) == 0 {
		return true
	}
	recorded, ok := e.adapter.RecordedBody(m)
	if !ok {
		return true
	}
	drift, comparable := e.adapter.Diff(m, recorded, liveBody, e.KnownNoise(m, userNoise), e.adapter.RecordedValueIsNoise(m))
	if !comparable {
		// No field structure to diff — fall back to byte equality so unequal
		// opaque bodies are a real mismatch rather than a silent pass.
		return string(recorded) == string(liveBody)
	}
	return len(drift) == 0
}

// DetectJSONDrift compares a recorded request body against the live one and
// returns the "body."-prefixed field paths that drifted and are NOT already
// covered by known noise (root-relative: global/user ∪ already-learned, prefix
// stripped). isRecordedNoise, when non-nil, drops fields whose recorded value is
// an obfuscated secret.
//
// comparable reports whether both bodies were valid JSON. When false there is no
// field structure to diff and the caller must treat any byte difference as a
// real, non-learnable mismatch. This is the single JSON-diff kernel behind both
// the HTTP and Pulsar schema-noise paths.
func DetectJSONDrift(recordedBody, liveBody []byte, known map[string][]string, isRecordedNoise func(string) bool) (drift map[string][]string, comparable bool) {
	if !json.Valid(recordedBody) || !json.Valid(liveBody) {
		return nil, false
	}
	paths := matcher.ChangedJSONFieldPaths(string(recordedBody), string(liveBody), known, isRecordedNoise)
	if len(paths) == 0 {
		return nil, true
	}
	out := make(map[string][]string, len(paths))
	for _, p := range paths {
		// Empty regex list == "ignore this whole field", the same semantics as
		// HTTPReq.ReqBodyNoise and TestCase.Noise.
		out["body."+p] = []string{}
	}
	return out, true
}

// MergeLearned returns a fresh map combining already-recorded noise with newly
// detected noise. Existing entries win on key collision (learned noise is
// monotonic — once a field is noise it stays noise), and every value slice is
// deep-copied so the result shares no backing storage with its inputs. This is
// the single merge helper behind HTTP's HTTPReq.ReqBodyNoise, the kind-agnostic
// MockSpec.ReqBodyNoise, and the on-disk persistence in mockdb.
func MergeLearned(existing, detected map[string][]string) map[string][]string {
	out := make(map[string][]string, len(existing)+len(detected))
	for k, v := range existing {
		vc := make([]string, len(v))
		copy(vc, v)
		out[k] = vc
	}
	for k, v := range detected {
		if _, ok := out[k]; ok {
			continue
		}
		vc := make([]string, len(v))
		copy(vc, v)
		out[k] = vc
	}
	return out
}

// MergeKnown unions two read-only noise sets into a fresh map; entries in a win
// on key collision. Either input may be empty; nil is returned when both are.
// Used to build the "known noise" set (global/user ∪ already-learned) consulted
// during diffing — the result never aliases an input, so callers may hold it.
func MergeKnown(a, b map[string][]string) map[string][]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string][]string, len(a)+len(b))
	// b first, then a, so a wins on key collision.
	maps.Copy(out, b)
	maps.Copy(out, a)
	return out
}

// StripBodyPrefix returns a copy of in with a leading "body." trimmed from each
// key, converting stored ("body."-prefixed) noise to the root-relative form the
// JSON matcher consumes. Returns nil for an empty input.
func StripBodyPrefix(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[strings.TrimPrefix(k, "body.")] = v
	}
	return out
}

// AddBodyPrefix returns a copy of in with "body." prepended to each key that
// doesn't already carry it. Returns nil for an empty input.
func AddBodyPrefix(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		if !strings.HasPrefix(k, "body.") {
			k = "body." + k
		}
		out[k] = v
	}
	return out
}
