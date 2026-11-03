// Package mocknoise hosts the kind-agnostic mock-noise learn/enforce engine
// shared by every protocol integration.
//
// Named "mock-noise" because that is what it is: noise learned ON A MOCK. The
// package was called "schemanoise" first, which read as though it were tied to
// schema matching or to the smart-set schema pipeline — it is neither. The old
// import path still resolves: integrations/schemanoise is a deprecated shim of
// type ALIASES over this package, so migrated and unmigrated callers can still
// exchange values. See that file for why aliases and not wrapper types.
//
// Mock-noise ("req_body_noise") is the field-path noise keploy auto-learns
// during auto-replay (config.Test.MockNoiseDetection, formerly
// SchemaNoiseDetection): when a
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
package mocknoise

import (
	"encoding/json"
	"maps"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Adapter is the per-protocol contract for plugging into the shared mock-noise
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
	// typeStrict names paths whose VALUE is ignored but whose TYPE must still
	// match (learned-dynamic enforcement); valueChangesOnly restricts the result
	// to value drift (the learn pass, so a type change is never learned). Both
	// empty/false => prior behaviour.
	// known and typeStrict are read only: known may be the caller's own
	// noise, given to every recording it is compared with
	// (StrictRejectRecordedJSONBody).
	Diff(m *models.Mock, recorded, live []byte, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (drift map[string][]string, comparable bool)
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
func (JSONDiffer) Diff(_ *models.Mock, recorded, live []byte, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (map[string][]string, bool) {
	return DetectJSONDrift(recorded, live, known, typeStrict, valueChangesOnly, valIsNoise)
}

// Engine runs the schema-noise learn/enforce flow for one protocol through its
// Adapter. Construct one per replay with the resolved flags via New.
type Engine struct {
	adapter   Adapter
	detection bool // config.Test.MockNoiseDetection (formerly SchemaNoiseDetection)
	strict    bool // config.Test.MockNoiseStrict (formerly SchemaNoiseStrict)
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
	return MergeKnown(userNoise, userDecided(userNoise, StripBodyPrefix(e.adapter.StoredNoise(m))))
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
	return e.DetectRecorded(m, recorded, ok, liveBody, userNoise)
}

// DetectRecorded is Detect with m's recorded body given (ok false: it has
// none), for a parser that compares a recording as the live request reads it
// (StrictRejectRecorded): what it learns is what strict then compares.
func (e *Engine) DetectRecorded(m *models.Mock, recorded []byte, ok bool, liveBody []byte, userNoise map[string][]string) (drift map[string][]string, comparable bool) {
	if !e.DetectionEnabled() || !ok {
		return nil, false
	}
	// Learn pass (JSON bodies): narrow learning to VALUE drift only
	// (valueChangesOnly=true). Auto-noising tolerates fields whose VALUE varies
	// between record and replay (ids, tokens, timestamps) while StrictReject
	// still enforces their TYPE -- it marks each learned path typeStrict, except
	// one the user also configured as noise, which stays full-ignore. A field
	// whose TYPE changed (incl. null<->value) or was removed is not "a value
	// that varies"; learning it would mask the type/shape drift type-enforcement
	// exists to catch, so it is left strict. typeStrict is nil here (this is the
	// learn pass, not the enforce pass). Non-JSON form bodies ignore this flag
	// (value-only/type-strict is a JSON-body feature for now).
	return e.adapter.Diff(m, recorded, liveBody, e.KnownNoise(m, userNoise), nil, true, e.adapter.RecordedValueIsNoise(m))
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
// enforcement. Every candidate is value-compared against its recorded request
// body: it is rejected when a field OUTSIDE the known-noise set (global/user
// body noise ∪ anything the mock already learned) drifted — an unmarked value or
// type change — or when the bodies aren't JSON-comparable yet differ
// byte-for-byte (a non-learnable mismatch). A mock with NO learned noise is no
// longer waved through: in strict mode only configured/learned noise is
// tolerated, so a value change on an unmarked field fails the match.
//
// This value check only runs when the engine was built with strict enforcement.
// Without it, StrictReject still tightens only what was explicitly learned and
// leaves un-learned mocks alone, preserving the lenient schema/key behaviour.
func (e *Engine) StrictAllows(m *models.Mock, liveBody []byte, userNoise map[string][]string) bool {
	allowed, _ := e.StrictReject(m, liveBody, userNoise)
	return allowed
}

// StrictReject is the diagnostic-bearing form of StrictAllows: alongside the
// allow/reject verdict it returns the "body."-prefixed non-noise field paths
// that caused a rejection, so callers can log WHY a candidate was dropped
// (e.g. "body.content"). allowed == true always comes with drift == nil. For an
// opaque (non-comparable) body that differs byte-for-byte, drift carries the
// single sentinel key "body" since there is no field structure to name.
func (e *Engine) StrictReject(m *models.Mock, liveBody []byte, userNoise map[string][]string) (allowed bool, drift map[string][]string) {
	if e == nil {
		return true, nil
	}
	recorded, ok := e.adapter.RecordedBody(m)
	return e.StrictRejectRecorded(m, recorded, ok, liveBody, userNoise)
}

// StrictRejectRecorded is StrictReject with m's recorded body given (ok false:
// it has none), for a parser that compares a recording as the live request
// reads it.
func (e *Engine) StrictRejectRecorded(m *models.Mock, recorded []byte, ok bool, liveBody []byte, userNoise map[string][]string) (allowed bool, drift map[string][]string) {
	return e.StrictRejectRecordedBody(m, recorded, ok, NewJSONBody(liveBody), userNoise)
}

// StrictRejectRecordedBody is StrictRejectRecorded with the live body as a
// JSONBody: compared with many recordings, it is validated and decoded once
// when the adapter compares JSON bodies as they are (JSONBodyDiffer).
func (e *Engine) StrictRejectRecordedBody(m *models.Mock, recorded []byte, ok bool, live *JSONBody, userNoise map[string][]string) (allowed bool, drift map[string][]string) {
	return e.StrictRejectRecordedJSONBody(m, NewJSONBody(recorded), ok, live, userNoise)
}

// StrictRejectRecordedJSONBody is StrictRejectRecordedBody with the recorded
// body as a JSONBody too: a parser that has a recording's body already
// decoded (NewDecodedJSONBody) gives it as it is, so the engine neither
// validates nor decodes it, and reads its bytes only where it compares
// bytes (an adapter that is not a JSONBodyDiffer, or bodies that are not
// both JSON). A recording with no learned noise is compared under userNoise
// itself (knownWithLearned), so a parser that compares a call with many
// recordings builds no noise set per recording.
func (e *Engine) StrictRejectRecordedJSONBody(m *models.Mock, recorded *JSONBody, ok bool, live *JSONBody, userNoise map[string][]string) (allowed bool, drift map[string][]string) {
	if e == nil {
		return true, nil
	}
	stored := e.adapter.StoredNoise(m)
	// Under strict enforcement every candidate is value-compared against its
	// recorded body, even one with no learned req_body_noise — so a changed value
	// (or type) on a field not covered by configured/learned noise rejects the
	// mock. Without strict enforcement we only tighten what was explicitly
	// learned, so a mock the auto-replay never touched (no stored noise) is left
	// alone and stays a match.
	if !e.strict && len(stored) == 0 {
		return true, nil
	}
	if !ok {
		return true, nil
	}
	// Enforcement: LEARNED paths (what the auto-noising detected) become
	// type-strict for JSON parsers — value ignored, type still enforced. known
	// keeps carrying them (as before) so NON-JSON parsers, which don't consult
	// typeStrict, still fully exclude learned noise; typeStrict additionally marks
	// them so a JSON parser type-enforces rather than fully ignores (collectJSON
	// consults typeStrict first). User-configured noise stays full-ignore/regex.
	learned := userDecided(userNoise, StripBodyPrefix(stored))
	known := knownWithLearned(userNoise, learned)
	typeStrict := learnedTypeStrict(userNoise, learned)
	var d map[string][]string
	var comparable bool
	if jd, isJSON := e.adapter.(JSONBodyDiffer); isJSON {
		d, comparable = jd.DiffJSONBodies(m, recorded, live, known, typeStrict, false, e.adapter.RecordedValueIsNoise(m))
	} else {
		d, comparable = e.adapter.Diff(m, recorded.Raw(), live.Raw(), known, typeStrict, false, e.adapter.RecordedValueIsNoise(m))
	}
	if !comparable {
		// No field structure to diff — fall back to byte equality so unequal
		// opaque bodies are a real mismatch rather than a silent pass.
		if string(recorded.Raw()) == string(live.Raw()) {
			return true, nil
		}
		return false, map[string][]string{"body": nil}
	}
	if len(d) == 0 {
		return true, nil
	}
	return false, d
}

// knownWithLearned is KnownNoise's set (userNoise ∪ learned, userNoise winning
// on a key both hold) for the strict compare, which only reads it: userNoise
// itself when nothing is learned, a merged copy otherwise.
func knownWithLearned(userNoise, learned map[string][]string) map[string][]string {
	if len(learned) == 0 {
		return userNoise
	}
	return MergeKnown(userNoise, learned)
}

// userDecided is learned without every path keploy.yml's entry with patterns
// names (matcher.NoiseLookup: by whole fields, without case): the user's
// patterns decide such a field, both its values. A learned entry there, a
// case twin ("userId" beside the user's "userid") or a more specific path
// ("user.id" under the user's "id"), would win the matcher's most specific
// first, or case, tie-break and excuse a value the patterns refuse. learned
// itself when it drops nothing.
func userDecided(userNoise, learned map[string][]string) map[string][]string {
	if len(learned) == 0 || !hasPatterns(userNoise) {
		return learned
	}
	user := matcher.NewNoiseLookup(userNoise)
	var out map[string][]string
	for k := range learned {
		if pats, ok := user.Patterns(k); ok && len(pats) > 0 {
			if out == nil {
				out = maps.Clone(learned)
			}
			delete(out, k)
		}
	}
	if out == nil {
		return learned
	}
	return out
}

// hasPatterns reports whether an entry of noise has patterns.
func hasPatterns(noise map[string][]string) bool {
	for _, pats := range noise {
		if len(pats) > 0 {
			return true
		}
	}
	return false
}

// learnedTypeStrict is the learned paths a JSON parser type-enforces: every
// learned path but one the user ALSO configured as noise, which keeps the
// user's intent (full-ignore / value-regex). known still carries it, so a
// non-JSON parser keeps excluding it. nil when nothing is learned.
func learnedTypeStrict(userNoise, learned map[string][]string) map[string]struct{} {
	if len(learned) == 0 {
		return nil
	}
	// User body noise is lowercased at ingest (decode.go), while learned noise
	// keeps the recorded JSON case (e.g. "userId"), so the exclusion must compare
	// case-insensitively — otherwise a user-ignored camelCase field would slip into
	// typeStrict and get type-enforced.
	userKeys := make(map[string]struct{}, len(userNoise))
	for k := range userNoise {
		userKeys[strings.ToLower(k)] = struct{}{}
	}
	typeStrict := make(map[string]struct{}, len(learned))
	for k := range learned {
		if _, userConfigured := userKeys[strings.ToLower(k)]; userConfigured {
			continue
		}
		typeStrict[k] = struct{}{}
	}
	return typeStrict
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
func DetectJSONDrift(recordedBody, liveBody []byte, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, isRecordedNoise func(string) bool) (drift map[string][]string, comparable bool) {
	return DetectJSONBodyDrift(NewJSONBody(recordedBody), NewJSONBody(liveBody), known, typeStrict, valueChangesOnly, isRecordedNoise)
}

// JSONBody is a request body DetectJSONBodyDrift compares, read once: whether
// it is JSON, and its value, are worked out by the first comparison that needs
// them and kept. A body compared with many others (a live command's, with
// every recording the matcher meets) is then validated and decoded once, not
// once a comparison. The kept value is only read, never changed. Not safe for
// concurrent use.
type JSONBody struct {
	raw []byte
	// rawFrom, when set, makes raw on its first read (Raw): a body made
	// decoded (NewDecodedJSONBody) has its bytes made only when compared
	// as bytes.
	rawFrom func() []byte
	// checked, valid: json.Valid's answer, once asked. read, decoded,
	// value: json.Unmarshal's, once asked (decoded false: a body that is
	// JSON but does not decode, a number past float64's range).
	checked, valid, read, decoded bool
	value                         any
	// reads counts the times the body was validated or decoded, so a test
	// can check that each is done once without timing it.
	reads int
}

// NewJSONBody is raw as a JSONBody, not yet read.
func NewJSONBody(raw []byte) *JSONBody {
	return &JSONBody{raw: raw}
}

// NewDecodedJSONBody is a JSON body given as the value json.Unmarshal
// decodes its bytes into (an interface{}: map[string]any, []any, float64,
// string, bool, nil) and a function that makes those bytes, called on the
// first read of them (Raw), if any. The caller vouches that value is what
// json.Unmarshal makes of raw's bytes, which are JSON: the body is neither
// validated nor decoded (Reads stays 0).
func NewDecodedJSONBody(value any, raw func() []byte) *JSONBody {
	return &JSONBody{rawFrom: raw, checked: true, valid: true, read: true, decoded: true, value: value}
}

// Raw is the body's bytes.
func (b *JSONBody) Raw() []byte {
	if b.rawFrom != nil {
		b.raw, b.rawFrom = b.rawFrom(), nil
	}
	return b.raw
}

// Reads is how many times the body was validated or decoded so far: each
// once at most, however many comparisons read it.
func (b *JSONBody) Reads() int {
	return b.reads
}

// isJSON reports whether the body is JSON (json.Valid), asked once.
func (b *JSONBody) isJSON() bool {
	if !b.checked {
		b.reads++
		b.checked, b.valid = true, json.Valid(b.raw)
	}
	return b.valid
}

// decodedValue is the body as json.Unmarshal decodes it into an interface{}
// (ok false: it does not decode), decoded once.
func (b *JSONBody) decodedValue() (value any, ok bool) {
	if !b.read {
		b.read = true
		b.reads++
		b.decoded = json.Unmarshal(b.raw, &b.value) == nil
	}
	return b.value, b.decoded
}

// JSONBodyDiffer is an Adapter whose Diff is DetectJSONDrift over the request
// bodies as they are (JSONDiffer's), saying so by comparing them as JSONBody
// (DetectJSONBodyDrift): the engine then reads a live body compared with many
// recordings once. JSONDiffer does not implement it, so an adapter that embeds
// JSONDiffer and has a Diff of its own (HTTP's, for form bodies) keeps its Diff.
// Like Diff, it reads known and typeStrict only.
type JSONBodyDiffer interface {
	DiffJSONBodies(m *models.Mock, recorded, live *JSONBody, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, valIsNoise func(string) bool) (drift map[string][]string, comparable bool)
}

// DetectJSONBodyDrift is DetectJSONDrift over two JSONBody, each validated and
// decoded once however many comparisons read it.
func DetectJSONBodyDrift(recorded, live *JSONBody, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, isRecordedNoise func(string) bool) (drift map[string][]string, comparable bool) {
	if !recorded.isJSON() || !live.isJSON() {
		return nil, false
	}
	// Both are JSON: a body that does not decode diffs to no path, as
	// matcher.ChangedJSONFieldPaths finds none for it.
	exp, ok := recorded.decodedValue()
	if !ok {
		return nil, true
	}
	act, ok := live.decodedValue()
	if !ok {
		return nil, true
	}
	// typeStrict paths ignore value but enforce type (learned-dynamic enforcement);
	// valueChangesOnly restricts learning to value drift. Both default to the empty
	// value, which keeps the prior behaviour.
	paths := matcher.ChangedDecodedJSONFieldPaths(exp, act, known, typeStrict, valueChangesOnly, isRecordedNoise)
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
	proven := provenswitch.On()
	out := make(map[string][]string, len(in))
	for k, v := range in {
		// Under the proven switch the learn step also learns noise in a
		// request's other parts (header.<name>, url.query.<name>,
		// url.path.<i>; package matcher/learn), in the same store. None
		// of them is a body path.
		if proven && learnedOutsideBody(k) {
			continue
		}
		out[strings.TrimPrefix(k, "body.")] = v
	}
	return out
}

// learnedOutsideBody reports whether a stored noise path names a part of the
// request other than its body, as the learn step writes them.
func learnedOutsideBody(path string) bool {
	return strings.HasPrefix(path, "header.") || strings.HasPrefix(path, "url.")
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
