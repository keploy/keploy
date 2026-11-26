package models

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

func TestMockVariableSourceAndStripDeclaredVariables(t *testing.T) {
	if VariableSourceDeclared != "declared" {
		t.Fatalf("VariableSourceDeclared = %q, want %q", VariableSourceDeclared, "declared")
	}

	learned := MockVariable{
		Recorded: "o-rec-1",
		Bind:     "body.orderId",
		Use:      []string{"url.path[2]"},
		Answer:   []string{"body.id"},
	}
	declared := MockVariable{
		Recorded: "Bearer rec.jwt.sig",
		Use:      []string{"header.Authorization"},
		Source:   VariableSourceDeclared,
	}

	m := &Mock{
		Version: GetVersion(),
		Kind:    HTTP,
		Name:    "mock-1",
		Spec: MockSpec{
			Variables: MockVariables{
				"orderId": learned,
				"auth":    declared,
			},
		},
	}

	// Preserved in memory (Clone and DeepCopy) and on the JSON wire.
	if !m.Spec.Variables.HasDeclared() {
		t.Fatal("HasDeclared() = false, want true")
	}
	cloned := m.Spec.Variables.Clone()
	if cloned["auth"].Source != VariableSourceDeclared {
		t.Fatalf("Clone() dropped Source: got %q, want %q", cloned["auth"].Source, VariableSourceDeclared)
	}
	dc := m.DeepCopy()
	if dc.Spec.Variables["auth"].Source != VariableSourceDeclared {
		t.Fatalf("DeepCopy() dropped Source: got %q, want %q", dc.Spec.Variables["auth"].Source, VariableSourceDeclared)
	}
	wireBytes, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal(Mock): %v", err)
	}
	var wireBack Mock
	if err := json.Unmarshal(wireBytes, &wireBack); err != nil {
		t.Fatalf("json.Unmarshal(Mock): %v", err)
	}
	if !reflect.DeepEqual(wireBack.Spec.Variables, m.Spec.Variables) {
		t.Fatalf("JSON wire round-trip mismatch:\ngot:  %+v\nwant: %+v", wireBack.Spec.Variables, m.Spec.Variables)
	}
	if wireBack.Spec.Variables["auth"].Source != VariableSourceDeclared {
		t.Fatalf("wire round-trip lost Source: %q", wireBack.Spec.Variables["auth"].Source)
	}

	// Persisted() returns a deep copy with declared stripped and learned kept,
	// without mutating the original map or sharing slices.
	persisted := m.Spec.Variables.Persisted()
	if len(persisted) != 1 {
		t.Fatalf("Persisted() len = %d, want 1: %+v", len(persisted), persisted)
	}
	if !reflect.DeepEqual(persisted["orderId"], learned) {
		t.Fatalf("Persisted() learned entry = %+v, want %+v", persisted["orderId"], learned)
	}
	if _, ok := persisted["auth"]; ok {
		t.Fatalf("Persisted() leaked declared variable: %+v", persisted)
	}
	persisted["orderId"].Use[0] = "mutated"
	if m.Spec.Variables["orderId"].Use[0] != "url.path[2]" {
		t.Fatalf("Persisted() shared Use slice with original: %+v", m.Spec.Variables["orderId"].Use)
	}
	if len(m.Spec.Variables) != 2 {
		t.Fatalf("Persisted() mutated original map: %+v", m.Spec.Variables)
	}

	// Persisted() short-circuit when no declared entries exist returns an
	// independent deep copy.
	learnedOnly := MockVariables{"orderId": learned}
	persistedLearned := learnedOnly.Persisted()
	if !reflect.DeepEqual(persistedLearned, learnedOnly) {
		t.Fatalf("Persisted(learnedOnly) = %+v, want %+v", persistedLearned, learnedOnly)
	}
	persistedLearned["orderId"].Use[0] = "mutated-fastpath"
	if learnedOnly["orderId"].Use[0] != "url.path[2]" {
		t.Fatalf("Persisted(learnedOnly) shared Use slice: %+v", learnedOnly["orderId"].Use)
	}

	// StripDeclaredVariables strips in place while keeping learned variables.
	StripDeclaredVariables(m)
	if m.Spec.Variables.HasDeclared() {
		t.Fatalf("HasDeclared() still true after StripDeclaredVariables: %+v", m.Spec.Variables)
	}
	if !reflect.DeepEqual(m.Spec.Variables, MockVariables{"orderId": learned}) {
		t.Fatalf("StripDeclaredVariables left %+v, want only learned", m.Spec.Variables)
	}

	// When only declared variables exist, stripping clears Variables to nil.
	onlyDeclared := &Mock{
		Spec: MockSpec{
			Variables: MockVariables{"auth": declared},
		},
	}
	StripDeclaredVariables(onlyDeclared)
	if onlyDeclared.Spec.Variables != nil {
		t.Fatalf("StripDeclaredVariables on declared-only mock = %+v, want nil", onlyDeclared.Spec.Variables)
	}

	// Nil-safe.
	StripDeclaredVariables(nil)
}

func TestMergeVariablesIgnoresDeclaredVariables(t *testing.T) {
	learned := MockVariable{
		Recorded: "o-1",
		Bind:     "body.id",
		Use:      []string{"url.path[1]"},
	}
	declaredSameName := MockVariable{
		Recorded: "o-1",
		Use:      []string{"header.Authorization"},
		Answer:   []string{"body.token"},
		Source:   VariableSourceDeclared,
	}
	declaredOther := MockVariable{
		Recorded: "Bearer tok",
		Use:      []string{"header.Authorization"},
		Source:   VariableSourceDeclared,
	}

	// Declared entry in b with same name must never merge into learned a.
	got := MergeVariables(
		MockVariables{"v": learned},
		MockVariables{"v": declaredSameName, "declOnly": declaredOther},
	)
	want := MockVariables{"v": learned}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeVariables(learned, declared) = %+v, want %+v", got, want)
	}

	// Declared entry in a with same name must never leak places into learned b.
	got = MergeVariables(
		MockVariables{"v": declaredSameName, "declOnly": declaredOther},
		MockVariables{"v": learned},
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeVariables(declared, learned) = %+v, want %+v", got, want)
	}

	// Declared-only inputs yield nil.
	got = MergeVariables(
		MockVariables{"declA": declaredOther},
		MockVariables{"declB": declaredSameName},
	)
	if got != nil {
		t.Fatalf("MergeVariables(declared, declared) = %+v, want nil", got)
	}

	// MergeBinds inherits the same protection.
	binds := MergeBinds(
		map[string]MockVariables{"mock-0": {"v": declaredSameName}},
		map[string]MockVariables{"mock-0": {"v": learned, "declOnly": declaredOther}},
	)
	if !reflect.DeepEqual(binds["mock-0"], want) {
		t.Fatalf("MergeBinds leaked declared variable: got %+v, want %+v", binds["mock-0"], want)
	}
}

func TestVariableBindingSensitiveRoundTrip(t *testing.T) {
	b := VariableBinding{
		Name:      "declared:fresh-jwt",
		Value:     "Bearer live.jwt",
		Recorded:  "Bearer rec.jwt",
		Mock:      "test case test-1",
		Place:     "header.Authorization",
		Given:     true,
		Sensitive: true,
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var jBack VariableBinding
	if err := json.Unmarshal(jb, &jBack); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(jBack, b) {
		t.Fatalf("JSON round-trip = %+v, want %+v", jBack, b)
	}

	// Non-sensitive omits "sensitive".
	plain := VariableBinding{Name: "id", Value: "u1", Recorded: "u0"}
	pj, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("json.Marshal(plain): %v", err)
	}
	if strings.Contains(string(pj), "sensitive") {
		t.Fatalf("non-sensitive binding serialized sensitive: %s", pj)
	}
}

func TestTestCaseRefusalErrorAndUnwrap(t *testing.T) {
	cause := errors.New("REFUSE_FROZEN_TIME_SHIFT: frozen test case")
	ref := &TestCaseRefusal{TestCase: "test-3", Err: cause}
	if !errors.Is(ref, cause) {
		t.Fatalf("errors.Is(ref, cause) = false, want true")
	}
	if !strings.Contains(ref.Error(), "test-3") || !strings.Contains(ref.Error(), cause.Error()) {
		t.Fatalf("TestCaseRefusal.Error() = %q, want test name and cause", ref.Error())
	}
}

func TestTestResultAppliedTransformsAndClockFreezeRoundTrip(t *testing.T) {
	instant := time.Date(2026, 11, 26, 10, 30, 0, 0, time.UTC)
	orig := TestResult{
		Kind:       HTTP,
		Name:       "test-1",
		Status:     TestStatusPassed,
		TestCaseID: "test-1",
		AppliedTransforms: []AppliedTransform{
			{
				Name:        "fresh-jwt",
				At:          "test.request",
				Place:       "header.Authorization",
				Function:    "jwt.resign",
				SigningMode: "secret",
				SecretRef:   "JWT_SECRET",
				Summary:     "re-signed HS256 JWT (exp +1h)",
			},
			{
				Name:     "shift-created",
				At:       "mock.answer",
				Place:    "body.createdAt",
				Function: "time.shift",
			},
		},
		ClockFreeze: &ClockFreezeInfo{
			Active:  true,
			Instant: &instant,
		},
	}

	// JSON round-trip.
	jBytes, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal(TestResult): %v", err)
	}
	var jBack TestResult
	if err := json.Unmarshal(jBytes, &jBack); err != nil {
		t.Fatalf("json.Unmarshal(TestResult): %v", err)
	}
	if !reflect.DeepEqual(jBack.AppliedTransforms, orig.AppliedTransforms) {
		t.Fatalf("JSON AppliedTransforms mismatch:\ngot:  %+v\nwant: %+v", jBack.AppliedTransforms, orig.AppliedTransforms)
	}
	if jBack.ClockFreeze == nil || jBack.ClockFreeze.Active != orig.ClockFreeze.Active || jBack.ClockFreeze.Instant == nil || !jBack.ClockFreeze.Instant.Equal(*orig.ClockFreeze.Instant) {
		t.Fatalf("JSON ClockFreeze mismatch:\ngot:  %+v\nwant: %+v", jBack.ClockFreeze, orig.ClockFreeze)
	}

	// YAML round-trip.
	yBytes, err := yamlLib.Marshal(orig)
	if err != nil {
		t.Fatalf("yaml.Marshal(TestResult): %v", err)
	}
	var yBack TestResult
	if err := yamlLib.Unmarshal(yBytes, &yBack); err != nil {
		t.Fatalf("yaml.Unmarshal(TestResult): %v", err)
	}
	if !reflect.DeepEqual(yBack.AppliedTransforms, orig.AppliedTransforms) {
		t.Fatalf("YAML AppliedTransforms mismatch:\ngot:  %+v\nwant: %+v", yBack.AppliedTransforms, orig.AppliedTransforms)
	}
	if yBack.ClockFreeze == nil || yBack.ClockFreeze.Active != orig.ClockFreeze.Active || yBack.ClockFreeze.Instant == nil || !yBack.ClockFreeze.Instant.Equal(*orig.ClockFreeze.Instant) {
		t.Fatalf("YAML ClockFreeze mismatch:\ngot:  %+v\nwant: %+v", yBack.ClockFreeze, orig.ClockFreeze)
	}

	// Inactive ClockFreeze with nil Instant omits "instant" in BOTH JSON and YAML.
	inactive := ClockFreezeInfo{Active: false}
	ij, err := json.Marshal(inactive)
	if err != nil {
		t.Fatalf("json.Marshal(inactive): %v", err)
	}
	var iRaw map[string]any
	if err := json.Unmarshal(ij, &iRaw); err != nil {
		t.Fatalf("json.Unmarshal(inactive): %v", err)
	}
	if _, ok := iRaw["instant"]; ok {
		t.Fatalf("inactive ClockFreezeInfo serialized instant in JSON: %s", ij)
	}
	iy, err := yamlLib.Marshal(inactive)
	if err != nil {
		t.Fatalf("yaml.Marshal(inactive): %v", err)
	}
	var iyRaw map[string]any
	if err := yamlLib.Unmarshal(iy, &iyRaw); err != nil {
		t.Fatalf("yaml.Unmarshal(inactive): %v", err)
	}
	if _, ok := iyRaw["instant"]; ok {
		t.Fatalf("inactive ClockFreezeInfo serialized instant in YAML: %s", iy)
	}

	// omitempty when absent on TestResult.
	empty := TestResult{Kind: HTTP, Name: "test-2", Status: TestStatusPassed}
	ej, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal(empty): %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(ej, &raw); err != nil {
		t.Fatalf("json.Unmarshal(raw): %v", err)
	}
	if _, ok := raw["applied_transforms"]; ok {
		t.Fatalf("empty TestResult serialized applied_transforms: %s", ej)
	}
	if _, ok := raw["clock_freeze"]; ok {
		t.Fatalf("empty TestResult serialized clock_freeze: %s", ej)
	}
}
