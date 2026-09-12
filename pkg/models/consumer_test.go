package models

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// consumerCategories is the full published set: the eleven REFUSALS design §5
// names, FIVE further refusals this implementation added, and the four
// VERDICTS (§1 demo, §5 "What is asserted").
//
// The four additions each split a case design §5 folded into another name, and
// each split is justified by the same rule: two failures with opposite remedies
// must not share a name, because the name is what an agent keys its next move
// on. CategoryConsumerProjectorFailed (a crashed or unregistered projector is a
// keploy defect, not a wire-version limitation);
// CategoryConsumerTriggerNotDelivered (the app never asked, as against the app
// threw our bytes away); CategoryConsumerCompletionTimeout (we stopped waiting,
// as against we compared and found a difference); CategoryConsumerRunCancelled
// (someone stopped the run, which the gate used to report as an agent lacking
// consumer support); CategoryConsumerUnsupportedSpec (the file is corrupt or
// newer than this build, as against the worker having produced nothing).
//
// Keyed by constant name so adding one here is a deliberate act,
// and mirrored against the file below so a constant cannot be added without an
// expectation.
var consumerCategories = map[string]FailureCategory{
	"CategoryConsumerNoObservableEffect":       CategoryConsumerNoObservableEffect,
	"CategoryConsumerOpaqueEffectBody":         CategoryConsumerOpaqueEffectBody,
	"CategoryConsumerUnsupportedWireVersion":   CategoryConsumerUnsupportedWireVersion,
	"CategoryConsumerTriggerDiscarded":         CategoryConsumerTriggerDiscarded,
	"CategoryConsumerEffectStraddlesUnit":      CategoryConsumerEffectStraddlesUnit,
	"CategoryConsumerMultiConnectionRecording": CategoryConsumerMultiConnectionRecording,
	"CategoryConsumerEffectCacheOverflow":      CategoryConsumerEffectCacheOverflow,
	"CategoryConsumerUnitsLost":                CategoryConsumerUnitsLost,
	"CategoryConsumerMappingsRequired":         CategoryConsumerMappingsRequired,
	"CategoryConsumerRepeatPassUnsupported":    CategoryConsumerRepeatPassUnsupported,
	"CategoryConsumerUnsupportedAgent":         CategoryConsumerUnsupportedAgent,
	"CategoryConsumerProjectorFailed":          CategoryConsumerProjectorFailed,
	"CategoryConsumerTriggerNotDelivered":      CategoryConsumerTriggerNotDelivered,
	"CategoryConsumerCompletionTimeout":        CategoryConsumerCompletionTimeout,
	"CategoryConsumerRunCancelled":             CategoryConsumerRunCancelled,
	"CategoryConsumerUnsupportedSpec":          CategoryConsumerUnsupportedSpec,

	"CategoryEffectBodyChanged":    CategoryEffectBodyChanged,
	"CategoryEffectMissing":        CategoryEffectMissing,
	"CategoryEffectUnexpected":     CategoryEffectUnexpected,
	"CategoryEffectTargetChanged":  CategoryEffectTargetChanged,
	"CategoryEffectHeadersChanged": CategoryEffectHeadersChanged,
}

// The eleven refusals design §5 names, by WIRE VALUE. These strings are
// persisted in report YAML, in JUnit and in the NDJSON `failure_categories`
// array, and agents key remedies on them — they are a one-way door, so the
// test pins the exact spelling rather than just "some constant exists".
func TestConsumerRefusalCategoriesMatchTheDesign(t *testing.T) {
	want := []FailureCategory{
		"CONSUMER_NO_OBSERVABLE_EFFECT",
		"CONSUMER_OPAQUE_EFFECT_BODY",
		"CONSUMER_UNSUPPORTED_WIRE_VERSION",
		"CONSUMER_TRIGGER_DISCARDED",
		"CONSUMER_EFFECT_STRADDLES_UNIT",
		"CONSUMER_MULTI_CONNECTION_RECORDING",
		"CONSUMER_EFFECT_CACHE_OVERFLOW",
		"CONSUMER_UNITS_LOST",
		"CONSUMER_MAPPINGS_REQUIRED",
		"CONSUMER_REPEAT_PASS_UNSUPPORTED",
		"CONSUMER_UNSUPPORTED_AGENT",
	}
	have := map[FailureCategory]bool{}
	for _, v := range consumerCategories {
		have[v] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("design §5 names refusal %q but no constant carries that value", w)
		}
	}
	// And the five verdicts. EFFECT_HEADERS_CHANGED is not in design §5's
	// list: the design does not say what happens to a recorded header at all,
	// and shipping EffectView.Headers persisted, rendered and UNASSERTED is
	// the silent pass rule 7 forbids. Asserting them needs a name of its own
	// (see the constant), so this is a documented addition to §5 rather than
	// an implementation of it.
	for _, w := range []FailureCategory{
		"EFFECT_BODY_CHANGED", "EFFECT_MISSING", "EFFECT_UNEXPECTED", "EFFECT_TARGET_CHANGED",
		"EFFECT_HEADERS_CHANGED",
	} {
		if !have[w] {
			t.Errorf("verdict category %q is missing", w)
		}
	}
}

// No consumer category may collide with an existing one, and none may collide
// with another. A duplicate string would silently merge two different failure
// meanings into one label in every report consumer.
func TestConsumerCategoriesAreUniqueAcrossTheWholeEnum(t *testing.T) {
	preexisting := []FailureCategory{
		SchemaUnchanged, SchemaAdded, SchemaBroken, StatusCodeChanged,
		HeaderChanged, InternalFailure, AppConnectionError, DependencyMissing,
	}
	seen := map[FailureCategory]string{}
	for _, c := range preexisting {
		seen[c] = "pre-existing"
	}
	for name, c := range consumerCategories {
		if c == "" {
			t.Errorf("%s is the empty string; an empty category is indistinguishable from 'no category'", name)
			continue
		}
		if prev, dup := seen[c]; dup {
			t.Errorf("%s = %q collides with %s", name, c, prev)
			continue
		}
		seen[c] = name
	}
}

// Every FailureCategory constant declared in consumer.go must have an entry in
// consumerCategories, so a new one cannot be added without an expectation.
func TestEveryConsumerCategoryConstantIsCovered(t *testing.T) {
	declared := typedConstantsInFile(t, "consumer.go", "FailureCategory")
	if len(declared) == 0 {
		t.Fatal("found no FailureCategory constants in consumer.go — the parser is broken, not the code")
	}
	for name, value := range declared {
		got, ok := consumerCategories[name]
		if !ok {
			t.Errorf("FailureCategory constant %s (%q) is declared in consumer.go but not covered by consumerCategories", name, value)
			continue
		}
		if string(got) != value {
			t.Errorf("%s: table says %q, source says %q", name, got, value)
		}
	}
	for name := range consumerCategories {
		if _, ok := declared[name]; !ok {
			t.Errorf("consumerCategories names %s, which no longer exists in consumer.go", name)
		}
	}
}

// typedConstantsInFile parses one file in this package and returns every
// top-level constant declared with the given explicit type. Same technique as
// kindConstantsInMockGo in depresult_test.go.
func typedConstantsInFile(t *testing.T, file, typeName string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				out[name.Name] = strings.Trim(lit.Value, `"`)
			}
		}
	}
	return out
}

// CONSUMER is a TEST-CASE kind, never a MOCK kind, and it must stay out of
// mock.go's Kind block.
//
// That block is the input to DepTypeForKind, which maps a MOCK's Kind onto the
// DepResult `type` protocol family and is pinned by
// TestDepTypeForKind_CoversEveryKind (which parses mock.go). Declaring
// CONSUMER there would assert that "Consumer" is a dependency protocol family,
// which is false — a consumer test's own dependencies are Kafka/Postgres/…
// mocks. This test pins the placement so a later move to mock.go forces that
// table to be updated deliberately rather than silently.
func TestConsumerKindIsNotAMockKind(t *testing.T) {
	for name, value := range kindConstantsInMockGo(t) {
		if name == "CONSUMER" || Kind(value) == CONSUMER {
			t.Errorf("Kind constant %s (%q) is declared in mock.go, but CONSUMER is a test-case kind. "+
				"If this move is deliberate, add it to TestDepTypeForKind's tables too and decide "+
				"what DepResult `type` a Consumer dependency should have.", name, value)
		}
	}
	if CONSUMER != "Consumer" {
		t.Errorf("CONSUMER = %q, want %q — the on-disk `kind:` value is a one-way door", CONSUMER, "Consumer")
	}
	// If it ever does leak into a mock pool, the fallback must still be a
	// sane lowercase family rather than an empty string.
	if got := DepTypeForKind(CONSUMER); got != "consumer" {
		t.Errorf("DepTypeForKind(CONSUMER) = %q, want %q", got, "consumer")
	}
}

func TestConsumerEndReasonsAreDistinct(t *testing.T) {
	reasons := map[string]ConsumerEndReason{
		"CountReached":        ConsumerEndReasonCountReached,
		"Timeout":             ConsumerEndReasonTimeout,
		"TriggerNotDelivered": ConsumerEndReasonTriggerNotDelivered,
		"TriggerDiscarded":    ConsumerEndReasonTriggerDiscarded,
		"InternalError":       ConsumerEndReasonInternalError,
	}
	seen := map[ConsumerEndReason]string{}
	for name, r := range reasons {
		if r == "" {
			t.Errorf("%s is empty", name)
		}
		if prev, dup := seen[r]; dup {
			t.Errorf("%s collides with %s (%q)", name, prev, r)
		}
		seen[r] = name
	}
	// Only one end reason may accompany a passing test.
	if ConsumerEndReasonCountReached != "count_reached" {
		t.Errorf("count_reached spelling changed: %q", ConsumerEndReasonCountReached)
	}
}

// The row name is a stable, human-first identifier that agents also parse, so
// its shape is a one-way door. Design §2 shows the exact string.
func TestEffectRowName(t *testing.T) {
	tests := []struct {
		name  string
		index int
		p     string
		op    string
		tgt   string
		key   string
		want  string
	}{
		{
			name: "the design's own example", index: 0,
			p: "kafka", op: "produce", tgt: "order-events", key: "o-4c1",
			want: "effects[0] kafka produce order-events key=o-4c1",
		},
		{
			name: "no key", index: 3,
			p: "pulsar", op: "send", tgt: "orders",
			want: "effects[3] pulsar send orders",
		},
		{
			name: "a projector that carries only a protocol stays readable", index: 7,
			p:    "sqs",
			want: "effects[7] sqs",
		},
		{
			name: "nothing at all still names its index", index: 12,
			want: "effects[12]",
		},
		{
			name: "whitespace-only parts are dropped, not rendered as gaps", index: 1,
			p: "kafka", op: "   ", tgt: "orders", key: " ",
			want: "effects[1] kafka orders",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectRowName(tt.index, tt.p, tt.op, tt.tgt, tt.key); got != tt.want {
				t.Errorf("EffectRowName = %q, want %q", got, tt.want)
			}
		})
	}

	v := EffectView{Protocol: "kafka", Op: "produce", Target: "order-events", Key: "o-4c1"}
	if got, want := EffectRowNameFor(0, v), "effects[0] kafka produce order-events key=o-4c1"; got != want {
		t.Errorf("EffectRowNameFor = %q, want %q", got, want)
	}
}

// The `effects[i]` / `deps[i]` prefix split is the documented discriminator
// between the consumer projector and the sync-path presence writer. Renderers
// dispatch on it instead of on a test's Kind, so it must be exactly disjoint.
func TestEffectRowsAndDepRowsAreTellableApartByPrefix(t *testing.T) {
	effect := DepResult{Name: EffectRowName(0, "kafka", "produce", "order-events", "o-4c1")}
	dep := DepResult{Name: DepRowName(0, DepTypePostgres, "db:5432 INSERT")}
	overflow := DepMissingOverflowRow(7)

	if !IsEffectRow(effect) {
		t.Errorf("IsEffectRow(%q) = false, want true", effect.Name)
	}
	if IsEffectRow(dep) {
		t.Errorf("IsEffectRow(%q) = true — a sync-path presence row must never be read as an effect row", dep.Name)
	}
	if IsEffectRow(overflow) {
		t.Errorf("IsEffectRow(%q) = true — the missing-count overflow row belongs to the sync path", overflow.Name)
	}
	if strings.HasPrefix(dep.Name, EffectRowPrefix) || strings.HasPrefix(effect.Name, "deps[") {
		t.Error("the two row-name prefixes overlap")
	}
	// `writes[i]` is retired and must be used by neither producer.
	for _, n := range []string{effect.Name, dep.Name, overflow.Name} {
		if strings.HasPrefix(n, "writes[") {
			t.Errorf("row %q uses the retired writes[] prefix", n)
		}
	}
}

// A reported diff key must paste straight back into spec.assertions.noise, so
// the key namespace and the noise vocabulary are the same strings.
func TestEffectKeyVocabulary(t *testing.T) {
	if got, want := EffectBodyKeyPrefix(0), "effects.0.body."; got != want {
		t.Errorf("EffectBodyKeyPrefix(0) = %q, want %q", got, want)
	}
	if got, want := EffectBodyKeyPrefix(12)+"status", "effects.12.body.status"; got != want {
		t.Errorf("body key = %q, want %q", got, want)
	}
	if got, want := EffectKey(0, EffectKeyPresence), "effects.0.presence"; got != want {
		t.Errorf("EffectKey = %q, want %q", got, want)
	}
	if got, want := EffectKey(2, EffectKeyUnexpected), "effects.2.unexpected"; got != want {
		t.Errorf("EffectKey = %q, want %q", got, want)
	}
	// The design's own reported path (§2 report example).
	if got, want := EffectBodyKeyPrefix(0)+"status", "effects.0.body.status"; got != want {
		t.Errorf("design example key = %q, want %q", got, want)
	}
}

// Completion must never degrade into "no grace" or "no timeout": a zero grace
// silently disables over-production detection (the N+1 regression becomes
// uncatchable) and a zero timeout turns a worker that never produces into a
// hung run instead of a failed test.
func TestConsumerCompletionNeverDegradesToZero(t *testing.T) {
	tests := []struct {
		name        string
		c           ConsumerCompletion
		wantGrace   time.Duration
		wantTimeout time.Duration
	}{
		{
			name:      "the design's own example",
			c:         ConsumerCompletion{ExpectEffects: 1, GraceMs: 250, TimeoutMs: 5000},
			wantGrace: 250 * time.Millisecond, wantTimeout: 5 * time.Second,
		},
		{
			name:        "unset falls back to the floor, not to zero",
			c:           ConsumerCompletion{},
			wantGrace:   time.Duration(ConsumerGraceMinMs) * time.Millisecond,
			wantTimeout: time.Duration(ConsumerDefaultTimeoutMs) * time.Millisecond,
		},
		{
			name:        "negative is treated as unset",
			c:           ConsumerCompletion{GraceMs: -1, TimeoutMs: -1},
			wantGrace:   time.Duration(ConsumerGraceMinMs) * time.Millisecond,
			wantTimeout: time.Duration(ConsumerDefaultTimeoutMs) * time.Millisecond,
		},
		{
			name:        "an absurd grace is clamped to the ceiling so one test cannot stall a suite",
			c:           ConsumerCompletion{GraceMs: 600000, TimeoutMs: 1000},
			wantGrace:   time.Duration(ConsumerGraceMaxMs) * time.Millisecond,
			wantTimeout: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.Grace(); got != tt.wantGrace {
				t.Errorf("Grace() = %v, want %v", got, tt.wantGrace)
			}
			if got := tt.c.Timeout(); got != tt.wantTimeout {
				t.Errorf("Timeout() = %v, want %v", got, tt.wantTimeout)
			}
		})
	}
}

func TestEffectViewDefaults(t *testing.T) {
	var zero EffectView
	if got := zero.AssertMode(); got != AssertFull {
		t.Errorf("empty Assert must mean AssertFull, got %q", got)
	}
	if zero.IsOpaque() {
		t.Error("an unstamped view must not read as opaque — only an explicit DecodedOpaque refuses")
	}
	if got := zero.RecordCount(); got != 1 {
		t.Errorf("RecordCount() = %d, want 1 — a real effect must never contribute zero to the completion count", got)
	}
	opaque := EffectView{Decoded: DecodedOpaque}
	if !opaque.IsOpaque() {
		t.Error("DecodedOpaque must read as opaque")
	}
	confident := EffectView{Decoded: DecodedConfident, Records: 4}
	if confident.IsOpaque() {
		t.Error("DecodedConfident must not read as opaque")
	}
	if got := confident.RecordCount(); got != 4 {
		t.Errorf("RecordCount() = %d, want 4", got)
	}
}

// Backdate anchors generated TLS certificates. Reading it straight off
// HTTPReq.Timestamp yields the ZERO TIME for a consumer test, and a zero
// backdate is NOT a 1970 certificate — tls.CertForClient substitutes
// time.Now() before subtracting a year, so zero has always been safe. What it
// loses is the RELATIONSHIP TO THE RECORDING: the certificate is anchored on
// wall-clock now rather than on the exchange it stands in for. See
// EarliestTimestamp's own comment, which states this in full.
func TestRecordWindowIsKindAware(t *testing.T) {
	req := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	resp := time.Date(2026, 8, 22, 10, 0, 1, 0, time.UTC)

	tests := []struct {
		name     string
		tc       *TestCase
		wantReq  time.Time
		wantResp time.Time
		wantEarl time.Time
	}{
		{
			name:    "nil test case",
			tc:      nil,
			wantReq: time.Time{}, wantResp: time.Time{}, wantEarl: time.Time{},
		},
		{
			name: "http",
			tc: &TestCase{Kind: HTTP,
				HTTPReq:  HTTPReq{Timestamp: req},
				HTTPResp: HTTPResp{Timestamp: resp}},
			wantReq: req, wantResp: resp, wantEarl: req,
		},
		{
			name: "grpc",
			tc: &TestCase{Kind: GRPC_EXPORT,
				GrpcReq:  GrpcReq{Timestamp: req},
				GrpcResp: GrpcResp{Timestamp: resp}},
			wantReq: req, wantResp: resp, wantEarl: req,
		},
		{
			name: "consumer reads its trigger window off the spec",
			tc: &TestCase{Kind: CONSUMER, ConsumerSpec: &ConsumerSpec{
				ReqTimestampMock: req, ResTimestampMock: resp}},
			wantReq: req, wantResp: resp, wantEarl: req,
		},
		{
			name:    "consumer with no spec degrades to zero, not to a panic",
			tc:      &TestCase{Kind: CONSUMER},
			wantReq: time.Time{}, wantResp: time.Time{}, wantEarl: time.Time{},
		},
		{
			name: "consumer with only a response time still anchors",
			tc: &TestCase{Kind: CONSUMER, ConsumerSpec: &ConsumerSpec{
				ResTimestampMock: resp}},
			wantReq: time.Time{}, wantResp: resp, wantEarl: resp,
		},
		{
			name: "empty kind (older recording) falls back to whichever payload is populated",
			tc: &TestCase{
				GrpcReq:  GrpcReq{Timestamp: req},
				GrpcResp: GrpcResp{Timestamp: resp}},
			wantReq: req, wantResp: resp, wantEarl: req,
		},
		{
			name:    "nothing but Created",
			tc:      &TestCase{Kind: CONSUMER, Created: 1755861852},
			wantReq: time.Time{}, wantResp: time.Time{}, wantEarl: time.Unix(1755861852, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReq, gotResp := tt.tc.RecordWindow()
			if !gotReq.Equal(tt.wantReq) || !gotResp.Equal(tt.wantResp) {
				t.Errorf("RecordWindow() = (%v, %v), want (%v, %v)", gotReq, gotResp, tt.wantReq, tt.wantResp)
			}
			if got := tt.tc.EarliestTimestamp(); !got.Equal(tt.wantEarl) {
				t.Errorf("EarliestTimestamp() = %v, want %v", got, tt.wantEarl)
			}
		})
	}
}

// EarliestTimestamp reports "no usable timestamp" rather than inventing one.
//
// The zero return is deliberate and is NOT a 1970 certificate: ca.go
// substitutes time.Now() for a zero backdate, which it has always done. The
// property being pinned is that this function does not fabricate an anchor of
// its own — a test case with nothing usable must say so, so the caller keeps
// ca.go's long-standing fallback instead of getting a made-up time that looks
// like it came from the recording.
func TestEarliestTimestampReportsUnusableRatherThanFabricatingAnAnchor(t *testing.T) {
	tc := &TestCase{Kind: CONSUMER, ConsumerSpec: &ConsumerSpec{}}
	if got := tc.EarliestTimestamp(); !got.IsZero() {
		t.Errorf("EarliestTimestamp() = %v, want the zero time so the caller can refuse", got)
	}
}

func TestConsumerResultInfoProjection(t *testing.T) {
	var nilResult *ConsumerResult
	if nilResult.Info() != nil {
		t.Fatal("a nil ConsumerResult must project to a nil Info so a non-consumer test serializes unchanged")
	}
	if nilResult.Refused() {
		t.Fatal("a nil result must not read as refused")
	}
	r := &ConsumerResult{
		TestID: "test-7", TriggerAccepted: true,
		ExpectEffects: 2, ObservedEffects: 2,
		EndReason: ConsumerEndReasonCountReached,
		Effects:   []EffectView{{Protocol: "kafka"}},
	}
	got := r.Info()
	want := &ConsumerResultInfo{
		TriggerAccepted: true, ExpectedEffects: 2, ObservedEffects: 2,
		EndReason: ConsumerEndReasonCountReached,
	}
	if *got != *want {
		t.Errorf("Info() = %+v, want %+v", *got, *want)
	}
	if r.Refused() {
		t.Error("a result with no Refusal must not read as refused")
	}
	r.Refusal = CategoryConsumerTriggerDiscarded
	if !r.Refused() {
		t.Error("a result carrying a Refusal must read as refused")
	}
}

// The persisted report shape is design §2's, exactly. These keys are what
// k8s-proxy and any agent read.
func TestConsumerResultInfoSerializesTheDesignsKeys(t *testing.T) {
	info := &ConsumerResultInfo{
		TriggerAccepted: true, ExpectedEffects: 2, ObservedEffects: 2,
		EndReason: ConsumerEndReasonCountReached,
	}
	out, err := yaml.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := "trigger_accepted: true\nexpected_effects: 2\nobserved_effects: 2\nend_reason: count_reached\n"
	if string(out) != want {
		t.Errorf("yaml =\n%s\nwant\n%s", out, want)
	}
	j, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	wantJSON := `{"trigger_accepted":true,"expected_effects":2,"observed_effects":2,"end_reason":"count_reached"}`
	if string(j) != wantJSON {
		t.Errorf("json = %s, want %s", j, wantJSON)
	}
}

// TestResult.Consumer and TestCase.ConsumerSpec are nullable pointers with
// omitempty precisely so an HTTP or gRPC artifact is byte-identical to a
// pre-consumer build. This is the field-level pin; the report and testdb
// golden tests are the document-level ones.
func TestNilConsumerFieldsAddNoBytes(t *testing.T) {
	res := TestResult{Kind: HTTP, Name: "test-set-0", TestCaseID: "test-1"}
	out, err := yaml.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "consumer") {
		t.Errorf("a non-consumer TestResult serialized a consumer key:\n%s", out)
	}
	j, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if strings.Contains(string(j), "consumer") {
		t.Errorf("a non-consumer TestResult serialized a consumer key in JSON:\n%s", j)
	}

	tc := TestCase{Kind: HTTP, Name: "test-1"}
	jt, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal testcase: %v", err)
	}
	if strings.Contains(string(jt), "consumer_spec") {
		t.Errorf("a non-consumer TestCase serialized consumer_spec:\n%s", jt)
	}
	// yaml:"-" — the persisted YAML form of a test case is the doc's spec
	// node, not this struct, so ConsumerSpec must never appear here even
	// when it IS set.
	tc.Kind = CONSUMER
	tc.ConsumerSpec = &ConsumerSpec{Protocol: "kafka"}
	yt, err := yaml.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal consumer testcase: %v", err)
	}
	if strings.Contains(string(yt), "consumerspec") || strings.Contains(string(yt), "consumer_spec") {
		t.Errorf("TestCase.ConsumerSpec leaked into the TestCase yaml projection:\n%s", yt)
	}
}

// The metadata keys live in OSS so a parser in another repository cannot drift
// the spelling: a typo would not fail to compile, it would silently produce a
// consumer test with no trigger and no effects.
func TestConsumerMetadataKeys(t *testing.T) {
	pairs := map[string]string{
		MetaKeyRole:     "role",
		MetaKeyOp:       "op",
		MetaKeyTarget:   "target",
		MetaKeyUnit:     "unit",
		MetaKeyRecords:  "records",
		MetaKeyUnitTest: "unitTest",
		RoleTrigger:     "trigger",
		RoleEffect:      "effect",
	}
	for got, want := range pairs {
		if got != want {
			t.Errorf("metadata constant = %q, want %q", got, want)
		}
	}
	if RoleTrigger == RoleEffect {
		t.Error("trigger and effect roles must be distinguishable")
	}
	// "" is the third, default role and means "ordinary mock, behaviour
	// unchanged" — that is what makes this contract additive.
	if RoleTrigger == "" || RoleEffect == "" {
		t.Error("a role value must not be empty: empty means 'ordinary mock'")
	}
}

func TestDecodeConfidenceValues(t *testing.T) {
	if DecodedConfident == DecodedOpaque {
		t.Fatal("confident and opaque must be distinguishable")
	}
	if DecodedConfident != "confident" || DecodedOpaque != "opaque" {
		t.Errorf("decoded spellings changed: %q / %q", DecodedConfident, DecodedOpaque)
	}
	// The zero value must NOT be opaque — an unstamped view is not a
	// refusal — but it must also not be silently treated as confident by
	// anything that checks explicitly.
	var zero DecodeConfidence
	if zero == DecodedOpaque {
		t.Error("the zero DecodeConfidence must not equal DecodedOpaque")
	}
}

// TestResult.Consumer had exactly one writer and NO reader, and nothing pinned
// its tags: a yaml/json tag typo would have silently written a report field no
// reader could find, and the persisted end_reason — the field an agent keys
// remedies on — would have vanished with no test noticing.
func TestConsumerResultInfoRoundTrips(t *testing.T) {
	in := TestResult{
		Kind:       CONSUMER,
		Name:       "test-set-0",
		TestCaseID: "test-7",
		Status:     TestStatusFailed,
		Consumer: &ConsumerResultInfo{
			TriggerAccepted: true,
			ExpectedEffects: 2,
			ObservedEffects: 3,
			EndReason:       ConsumerEndReasonCountReached,
		},
	}

	t.Run("yaml", func(t *testing.T) {
		raw, err := yaml.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// The persisted spellings are part of the report schema that
		// k8s-proxy and the agent loop read.
		for _, want := range []string{"trigger_accepted: true", "expected_effects: 2", "observed_effects: 3", "end_reason: count_reached"} {
			if !strings.Contains(string(raw), want) {
				t.Fatalf("report yaml is missing %q:\n%s", want, raw)
			}
		}
		var out TestResult
		if err := yaml.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Consumer == nil || *out.Consumer != *in.Consumer {
			t.Fatalf("round trip lost the window: %+v", out.Consumer)
		}
	})

	t.Run("json", func(t *testing.T) {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out TestResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.Consumer == nil || *out.Consumer != *in.Consumer {
			t.Fatalf("round trip lost the window: %+v", out.Consumer)
		}
	})

	t.Run("nil for every other kind leaves the document untouched", func(t *testing.T) {
		// The backward-compatibility half: an HTTP result must serialize with
		// no consumer key at all.
		raw, err := yaml.Marshal(TestResult{Kind: HTTP, Name: "test-set-0"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(raw), "consumer") {
			t.Fatalf("an HTTP result must not carry a consumer key:\n%s", raw)
		}
	})
}

// The one-line window summary is what puts end_reason in front of a human.
func TestFormatConsumerRun(t *testing.T) {
	tests := []struct {
		name string
		in   *ConsumerResultInfo
		want string
	}{
		{name: "nil renders nothing, so a non-consumer report is byte-identical", in: nil, want: ""},
		{
			name: "a healthy window",
			in:   &ConsumerResultInfo{TriggerAccepted: true, ExpectedEffects: 2, ObservedEffects: 2, EndReason: ConsumerEndReasonCountReached},
			want: "  window: 2 of 2 effects observed, trigger accepted, ended count_reached\n",
		},
		{
			// The two failures a bare "0 effects" cannot tell apart.
			name: "the app never took the message",
			in:   &ConsumerResultInfo{ExpectedEffects: 1, EndReason: ConsumerEndReasonTriggerNotDelivered},
			want: "  window: 0 of 1 effects observed, trigger NOT accepted, ended trigger_not_delivered\n",
		},
		{
			name: "no end reason at all",
			in:   &ConsumerResultInfo{TriggerAccepted: true, ExpectedEffects: 1},
			want: "  window: 0 of 1 effects observed, trigger accepted, ended no end reason reported\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.FormatConsumerRun(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
