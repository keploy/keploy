package report

import (
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestBuildEffectRows(t *testing.T) {
	tests := []struct {
		name string
		test models.TestResult
		want []effectRow
	}{
		{
			name: "no dependency rows yields a non-nil empty slice",
			test: models.TestResult{},
			want: []effectRow{},
		},
		{
			name: "one row per meta, flattened with the parent name and type",
			test: models.TestResult{Result: models.Result{DepResult: []models.DepResult{
				missingDepRow(), consumedDepRow(),
			}}},
			want: []effectRow{
				{
					Name: "deps[0] postgres PostgreSQL INSERT (presence)", Type: "postgres",
					Key: "presence", Expected: "consumed", Actual: "not consumed", Matched: false,
				},
				{
					Name: "deps[1] http GET api.internal:80/orders (presence)", Type: "http",
					Key: "presence", Expected: "consumed", Actual: "consumed", Matched: true,
				},
			},
		},
		{
			name: "an empty meta key falls back to the presence key",
			test: models.TestResult{Result: models.Result{DepResult: []models.DepResult{{
				Name: "deps[0] kafka orders (presence)", Type: "kafka",
				Meta: []models.DepMetaResult{{Normal: false, Expected: "1", Actual: "0"}},
			}}}},
			want: []effectRow{{
				Name: "deps[0] kafka orders (presence)", Type: "kafka",
				Key: "presence", Expected: "1", Actual: "0", Matched: false,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildEffectRows(tt.test)
			if got == nil {
				t.Fatal("buildEffectRows must never return nil so effects marshals as []")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d rows, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuildTestVerdict_Schema(t *testing.T) {
	v := buildTestVerdict("test-run-3", "test-set-0", models.TestResult{
		Kind:       models.HTTP,
		TestCaseID: "test-1",
		Status:     models.TestStatusFailed,
		FailureInfo: models.FailureInfo{
			Category: []models.FailureCategory{models.DependencyMissing},
		},
		Result: models.Result{
			DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true, DepsConsumed: 7,
		},
	})

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"schema_version":        EffectsSchemaVersion,
		"test_run_id":           "test-run-3",
		"test_set_id":           "test-set-0",
		"test_case_id":          "test-1",
		"kind":                  string(models.HTTP),
		"status":                string(models.TestStatusFailed),
		"failure_categories":    []any{"DEPENDENCY_MISSING"},
		"dependencies_checked":  true,
		"dependencies_consumed": float64(7),
	}
	for k, expected := range want {
		got, ok := decoded[k]
		if !ok {
			t.Fatalf("schema field %q missing from %s", k, data)
		}
		if gotSlice, isSlice := got.([]any); isSlice {
			expectedSlice := expected.([]any)
			if len(gotSlice) != len(expectedSlice) || gotSlice[0] != expectedSlice[0] {
				t.Errorf("field %q = %v, want %v", k, got, expected)
			}
			continue
		}
		if got != expected {
			t.Errorf("field %q = %v, want %v", k, got, expected)
		}
	}

	effects, ok := decoded["effects"].([]any)
	if !ok {
		t.Fatalf("effects missing or not an array in %s", data)
	}
	if len(effects) != 1 {
		t.Fatalf("expected 1 effect, got %d", len(effects))
	}
	effect := effects[0].(map[string]any)
	for _, key := range []string{"name", "type", "key", "expected", "actual", "matched"} {
		if _, ok := effect[key]; !ok {
			t.Errorf("effect missing field %q: %v", key, effect)
		}
	}
	if effect["matched"] != false {
		t.Errorf("matched = %v, want false", effect["matched"])
	}
}

func TestBuildTestVerdict_EmptyCategoriesAreOmittedAndEffectsAlwaysPresent(t *testing.T) {
	data, err := json.Marshal(buildTestVerdict("run", "set", models.TestResult{
		Kind: models.HTTP, TestCaseID: "t", Status: models.TestStatusPassed,
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	if strings.Contains(out, "failure_categories") {
		t.Errorf("empty failure_categories must be omitted: %s", out)
	}
	if !strings.Contains(out, `"effects":[]`) {
		t.Errorf(`effects must always be present as []: %s`, out)
	}
}

func TestBuildTestVerdicts_DeterministicOrderingAndTestSetFallback(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-2": {TestSet: "test-set-2", Tests: []models.TestResult{
			{TestCaseID: "c", Status: models.TestStatusPassed},
		}},
		"test-set-0": {TestSet: "test-set-0", Tests: []models.TestResult{
			{TestCaseID: "a", Status: models.TestStatusPassed},
			{TestCaseID: "b", Status: models.TestStatusFailed},
		}},
		// TestSet unset: the map key is the fallback.
		"test-set-1": {Tests: []models.TestResult{{TestCaseID: "d", Status: models.TestStatusPassed}}},
		"test-set-3": nil,
	}

	for range 5 {
		got := buildTestVerdicts("run-9", reports)
		var ids, sets []string
		for _, v := range got {
			ids = append(ids, v.TestCaseID)
			sets = append(sets, v.TestSetID)
		}
		if strings.Join(ids, ",") != "a,b,d,c" {
			t.Fatalf("non-deterministic or wrong ordering: %v", ids)
		}
		if strings.Join(sets, ",") != "test-set-0,test-set-0,test-set-1,test-set-2" {
			t.Fatalf("test_set_id wrong (fallback broken?): %v", sets)
		}
	}
}

func TestWriteNDJSON_OneObjectPerLine(t *testing.T) {
	var sb strings.Builder
	verdicts := buildTestVerdicts("run-1", map[string]*models.TestReport{
		"test-set-0": {TestSet: "test-set-0", Tests: []models.TestResult{
			{Kind: models.HTTP, TestCaseID: "t1", Status: models.TestStatusFailed,
				Result: models.Result{DepResult: []models.DepResult{missingDepRow()}}},
			{Kind: models.HTTP, TestCaseID: "t2", Status: models.TestStatusPassed},
		}},
	})
	if err := writeNDJSON(&sb, verdicts); err != nil {
		t.Fatalf("writeNDJSON: %v", err)
	}

	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 NDJSON lines, got %d: %q", len(lines), sb.String())
	}
	for i, line := range lines {
		var v testVerdict
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("line %d is not standalone JSON: %v (%q)", i, err, line)
		}
		if v.SchemaVersion != EffectsSchemaVersion {
			t.Errorf("line %d schema_version = %q, want %q", i, v.SchemaVersion, EffectsSchemaVersion)
		}
		if v.TestRunID != "run-1" {
			t.Errorf("line %d test_run_id = %q", i, v.TestRunID)
		}
	}
}

func TestWriteNDJSON_NoVerdictsWritesNothing(t *testing.T) {
	var sb strings.Builder
	if err := writeNDJSON(&sb, nil); err != nil {
		t.Fatalf("writeNDJSON: %v", err)
	}
	if sb.Len() != 0 {
		t.Errorf("expected no output, got %q", sb.String())
	}
}

// --- schema contract pin ---
//
// The other assertions in this file compare the emitted schema_version to
// EffectsSchemaVersion, i.e. the constant to itself: changing "1" to "2" — the
// exact act ndjson.go says must be deliberate and breaking — leaves them all
// green. This one pins the literal BYTES of one line: the version, every field
// name, and the field ORDER a consumer's line-oriented parser sees.
//
// If this fails you are changing the agent contract. That is allowed, but it
// is a decision: update the literal here AND bump EffectsSchemaVersion if the
// change removes, renames, or redefines a field.
const goldenNDJSONLine = `{"schema_version":"1","test_run_id":"test-run-3","test_set_id":"test-set-0","test_case_id":"test-1","kind":"Http","status":"FAILED","failure_categories":["DEPENDENCY_MISSING"],"dependencies_checked":true,"dependencies_consumed":3,"effects":[{"name":"deps[0] postgres PostgreSQL INSERT (presence)","type":"postgres","key":"presence","expected":"consumed","actual":"not consumed","matched":false}]}`

// The other half of the contract, and the one an agent gets wrong: a test the
// assertion never ran for. It must be distinguishable from a clean one by a
// field, not by an empty array.
const goldenNDJSONUncheckedLine = `{"schema_version":"1","test_run_id":"test-run-3","test_set_id":"test-set-0","test_case_id":"test-2","kind":"Http","status":"PASSED","dependencies_checked":false,"dependencies_consumed":0,"effects":[]}`

func TestNDJSONLineIsByteStable(t *testing.T) {
	var sb strings.Builder
	verdicts := buildTestVerdicts("test-run-3", map[string]*models.TestReport{
		"test-set-0": {TestSet: "test-set-0", Tests: []models.TestResult{{
			Kind:       models.HTTP,
			TestCaseID: "test-1",
			Status:     models.TestStatusFailed,
			FailureInfo: models.FailureInfo{
				Category: []models.FailureCategory{models.DependencyMissing},
			},
			Result: models.Result{
				DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true, DepsConsumed: 3,
			},
		}, {
			Kind:       models.HTTP,
			TestCaseID: "test-2",
			Status:     models.TestStatusPassed,
		}}},
	})
	if err := writeNDJSON(&sb, verdicts); err != nil {
		t.Fatalf("writeNDJSON: %v", err)
	}

	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), sb.String())
	}
	for i, want := range []string{goldenNDJSONLine, goldenNDJSONUncheckedLine} {
		if lines[i] != want {
			t.Fatalf("the NDJSON contract changed.\n got: %s\nwant: %s\n\n"+
				"If this is deliberate, update the golden line — and bump EffectsSchemaVersion "+
				"if a field was removed, renamed, or given a different meaning.", lines[i], want)
		}
	}

	// And the version inside the golden line must be the constant, so the two
	// cannot be updated independently.
	if !strings.Contains(goldenNDJSONLine, `"schema_version":"`+EffectsSchemaVersion+`"`) {
		t.Fatalf("EffectsSchemaVersion is %q but the golden line pins a different version: %s",
			EffectsSchemaVersion, goldenNDJSONLine)
	}
}

// The false-green the agent contract itself could re-create: a test whose
// dependency assertion never ran emits `effects: []`, byte-identical to a fully
// checked test that lost nothing. `dependencies_checked` is the only thing
// separating them, so it has to be present and it has to differ.
func TestNDJSON_UncheckedIsDistinguishableFromCleanlyChecked(t *testing.T) {
	// Everything consumed: the assertion RAN and found nothing missing. The
	// replayer sets the DepsChecked bit to say so — and writes NO rows, which
	// is exactly why the bit has to exist.
	//
	// DepsConsumed is non-zero because a WRITER cannot produce checked-with-
	// zero-consumed-and-no-rows: the bit is set only when at least one
	// dependency was eligible, and an eligible dependency that was not consumed
	// is a missing row. Using the impossible shape here would enshrine the
	// false green as the example of a healthy test.
	checked := buildTestVerdict("run", "set", models.TestResult{
		Kind: models.HTTP, TestCaseID: "checked", Status: models.TestStatusPassed,
		Result: models.Result{DepsChecked: true, DepsConsumed: 2},
	})
	// A --base-path run, --disable-mapping, an unmapped test set, a failed
	// consumed-mock fetch, or the deferred streaming path.
	unchecked := buildTestVerdict("run", "set", models.TestResult{
		Kind: models.HTTP, TestCaseID: "unchecked", Status: models.TestStatusPassed,
	})

	if !checked.DependenciesChecked {
		t.Error("a test whose assertion ran and found nothing must report dependencies_checked=true")
	}
	if unchecked.DependenciesChecked {
		t.Error("a test whose assertion never ran must report dependencies_checked=false")
	}

	// Neither has a failed effect, so `any(matched == false)` cannot tell them
	// apart — which is exactly why the boolean exists.
	for _, v := range []testVerdict{checked, unchecked} {
		for _, e := range v.Effects {
			if !e.Matched {
				t.Fatalf("%s unexpectedly has a failed effect: %+v", v.TestCaseID, e)
			}
		}
	}

	a, err := json.Marshal(checked)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(unchecked)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(a), `"dependencies_checked":true`) {
		t.Errorf("checked line lost the field: %s", a)
	}
	if !strings.Contains(string(b), `"dependencies_checked":false`) {
		t.Errorf("unchecked line lost the field: %s", b)
	}
}

// A test that PASSED while losing a dependency must be readable as such from
// one NDJSON line alone — that is the agent-loop contract.
func TestNDJSON_PassedWithMissingDependencyIsMachineDetectable(t *testing.T) {
	v := buildTestVerdict("run", "set", models.TestResult{
		Kind: models.HTTP, TestCaseID: "t", Status: models.TestStatusPassed,
		Result: models.Result{
			DepResult: []models.DepResult{missingDepRow()}, DepsChecked: true, DepsConsumed: 4,
		},
	})
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Status               string `json:"status"`
		DependenciesChecked  bool   `json:"dependencies_checked"`
		DependenciesConsumed int    `json:"dependencies_consumed"`
		Effects              []struct {
			Key      string `json:"key"`
			Matched  bool   `json:"matched"`
			Expected string `json:"expected"`
		} `json:"effects"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Status != string(models.TestStatusPassed) {
		t.Errorf("status = %q, want PASSED (the knob is what flips it, not the report)", decoded.Status)
	}
	if !decoded.DependenciesChecked {
		t.Error("dependencies_checked must be true, or the documented rule never fires")
	}
	if decoded.DependenciesConsumed != 4 {
		t.Errorf("dependencies_consumed = %d, want 4", decoded.DependenciesConsumed)
	}
	var lost int
	for _, e := range decoded.Effects {
		if !e.Matched {
			lost++
		}
	}
	if lost != 1 {
		t.Errorf("expected exactly one failed effect, got %d", lost)
	}
	// The dependencies the test DID exercise must not be effect entries: at
	// ~190 bytes of persisted YAML each they are the report-bloat axis this
	// slice explicitly rejected.
	if len(decoded.Effects) != 1 {
		t.Errorf("expected exactly one effect entry (the failed one), got %d: %s", len(decoded.Effects), data)
	}
}

// The capped shape on the wire. An agent following the documented rule
// (`dependencies_checked && any(e.matched == false)`) must still see the
// failure when the per-test cap collapsed most of the missing dependencies
// into the overflow entry, and must be able to tell an overflow entry from a
// named one WITHOUT parsing the row name.
func TestBuildEffectRows_MissingOverflowIsOnTheWire(t *testing.T) {
	rows := buildEffectRows(models.TestResult{Result: models.Result{
		DepResult:   []models.DepResult{missingDepRow(), models.DepMissingOverflowRow(150)},
		DepsChecked: true,
	}})

	if len(rows) != 2 {
		t.Fatalf("got %d effect rows, want 2: %+v", len(rows), rows)
	}
	overflow := rows[1]
	want := effectRow{
		Name:     "deps[*] 150 more not consumed (presence)",
		Type:     "",
		Key:      models.DepKeyMissingCount,
		Expected: "0",
		Actual:   "150",
		Matched:  false,
	}
	if overflow != want {
		t.Fatalf("overflow entry = %+v, want %+v", overflow, want)
	}

	// The documented rule fires.
	lost := false
	named := 0
	for _, r := range rows {
		if !r.Matched {
			lost = true
		}
		if r.Key == models.DepKeyPresence && !r.Matched {
			named++
		}
	}
	if !lost {
		t.Fatal("a capped, mostly-overflowed test reads as having lost nothing")
	}
	if named != 1 {
		t.Fatalf("counted %d individually named missing dependencies, want 1", named)
	}
}
