package replay

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

func consumed(name string, kind models.Kind) models.MockState {
	return models.MockState{Name: name, Kind: kind}
}

func missingMeta() []models.DepMetaResult {
	return []models.DepMetaResult{{
		Normal:   false,
		Key:      models.DepKeyPresence,
		Expected: models.DepPresenceConsumed,
		Actual:   models.DepPresenceMissing,
	}}
}

func TestBuildDepResults(t *testing.T) {
	tests := []struct {
		name          string
		expected      []models.MockEntry
		consumedMocks []models.MockState
		valid         bool
		reusable      map[string]bool
		kindByName    map[string]models.Kind
		lookup        map[string]mockDisplayInfo
		// want is the MISSING rows only. The consumed side is two scalars on
		// the result, never rows — see buildDepResults' sizing note.
		want         []models.DepResult
		wantConsumed int
		wantChecked  bool
	}{
		{
			name:          "a consumed dependency is counted, never persisted as a row",
			expected:      []models.MockEntry{{Name: "mock-1", Kind: "Postgres"}},
			consumedMocks: []models.MockState{consumed("mock-1", models.Kind("Postgres"))},
			valid:         true,
			lookup:        map[string]mockDisplayInfo{"mock-1": {protocol: "Postgres", target: "db:5432"}},
			want:          nil,
			wantConsumed:  1,
			wantChecked:   true,
		},
		{
			name:          "unconsumed dependency yields a MISSING row unconditionally",
			expected:      []models.MockEntry{{Name: "mock-1", Kind: "Postgres"}},
			consumedMocks: nil,
			valid:         true,
			lookup:        map[string]mockDisplayInfo{"mock-1": {protocol: "Postgres", target: "db:5432"}},
			want: []models.DepResult{
				{
					Name: "deps[0] postgres db:5432 (presence)",
					Type: "postgres",
					Meta: missingMeta(),
				},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			name: "parser versions normalise onto one protocol family",
			// postgresv3 / http2 must never reach the `type` contract: an agent
			// keying on "postgres" would miss every v2/v3 recording.
			expected: []models.MockEntry{
				{Name: "pg", Kind: "PostgresV3"},
				{Name: "h2", Kind: "Http2"},
			},
			valid:  true,
			lookup: map[string]mockDisplayInfo{"pg": {target: "db:5432"}, "h2": {target: "GET api:443/v1"}},
			want: []models.DepResult{
				{Name: "deps[0] postgres db:5432 (presence)", Type: "postgres", Meta: missingMeta()},
				{Name: "deps[1] http GET api:443/v1 (presence)", Type: "http", Meta: missingMeta()},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			name: "the consumed count covers only per-test dependencies",
			expected: []models.MockEntry{
				{Name: "m1", Kind: "Http"},
				{Name: "m2", Kind: "Http"},
				{Name: "gone", Kind: "Http"},
				{Name: "session-1", Kind: "Http"},
				{Name: "dns-1", Kind: "DNS"},
			},
			consumedMocks: []models.MockState{
				consumed("m1", models.HTTP), consumed("m2", models.HTTP),
				{Name: "session-1", Kind: models.HTTP, Lifetime: models.LifetimeSession},
				consumed("dns-1", models.DNS),
			},
			valid:    true,
			reusable: map[string]bool{"session-1": true},
			lookup:   map[string]mockDisplayInfo{"gone": {target: "GET api:80/orders"}},
			want: []models.DepResult{
				// "gone" is the THIRD entry of the filtered expected list
				// (m1, m2, gone), so its name is deps[2] whether or not m1 and
				// m2 happened to be consumed in this run.
				{Name: "deps[2] http GET api:80/orders (presence)", Type: "http", Meta: missingMeta()},
			},
			wantConsumed: 2,
			wantChecked:  true,
		},
		{
			name: "reusable-tier expected mocks are excluded from the assertion",
			expected: []models.MockEntry{
				{Name: "session-1", Kind: "MySQL"},
				{Name: "mock-1", Kind: "MySQL"},
			},
			consumedMocks: []models.MockState{consumed("mock-1", models.Kind("MySQL"))},
			valid:         true,
			reusable:      map[string]bool{"session-1": true},
			lookup:        map[string]mockDisplayInfo{"mock-1": {target: "mysql:3306"}},
			want:          nil,
			wantConsumed:  1,
			wantChecked:   true,
		},
		{
			name: "reusable-tier CONSUMED mocks do not satisfy a per-test expectation",
			// Mirrors filteredMockNames at the call site: a mock consumed under
			// a session/connection lifetime is not attributable to this test.
			expected: []models.MockEntry{{Name: "mock-1", Kind: "MySQL"}},
			consumedMocks: []models.MockState{
				{Name: "mock-1", Kind: models.Kind("MySQL"), Lifetime: models.LifetimeSession},
			},
			valid: true,
			want: []models.DepResult{
				{
					Name: "deps[0] mysql (presence)",
					Type: "mysql",
					Meta: missingMeta(),
				},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			name: "DNS is excluded via the entry kind and via the lookup",
			expected: []models.MockEntry{
				{Name: "dns-1", Kind: "DNS"},
				{Name: "dns-2", Kind: ""},
				{Name: "mock-1", Kind: "Http"},
			},
			consumedMocks: []models.MockState{consumed("mock-1", models.HTTP)},
			valid:         true,
			kindByName:    map[string]models.Kind{"dns-2": models.DNS},
			lookup:        map[string]mockDisplayInfo{"mock-1": {target: "GET api.internal:80/x"}},
			want:          nil,
			wantConsumed:  1,
			wantChecked:   true,
		},
		{
			name:          "an invalid assertion context yields no rows at all",
			expected:      []models.MockEntry{{Name: "mock-1", Kind: "Postgres"}},
			consumedMocks: []models.MockState{consumed("stale-mock", models.Kind("Postgres"))},
			valid:         false,
			want:          nil,
		},
		{
			name:          "no expected mocks yields no rows",
			expected:      nil,
			consumedMocks: []models.MockState{consumed("mock-1", models.Kind("Postgres"))},
			valid:         true,
			want:          nil,
		},
		{
			// The whole point of the unconditional Checked bit: "checked and
			// clean" must be distinguishable on disk from "never checked",
			// without persisting a single row to say so.
			name:          "everything consumed still sets Checked, so the assertion is provably RUN",
			expected:      []models.MockEntry{{Name: "m1", Kind: "Http"}, {Name: "m2", Kind: "Http"}},
			consumedMocks: []models.MockState{consumed("m1", models.HTTP), consumed("m2", models.HTTP)},
			valid:         true,
			want:          nil,
			wantConsumed:  2,
			wantChecked:   true,
		},
		{
			// THE FALSE GREEN THIS GUARD EXISTS TO CLOSE, and the shape an
			// ordinary recording actually produces: every mapped entry is
			// session-tier (what models.Mock.DeriveLifetime makes of untagged
			// HTTP/Postgres/MySQL egress), so the filter removes all of them
			// and the assertion never ran over a single dependency.
			//
			// This case USED to assert Checked=true, which persisted
			// `deps_checked: true, deps_consumed: 0, dep_result: []` — the
			// documented spelling of "checked, nothing missing" — for a test
			// where nothing was ever eligible to be checked.
			name:          "every mapped dependency filtered out means NOT checked, never checked-and-clean",
			expected:      []models.MockEntry{{Name: "session-1", Kind: "Http"}},
			consumedMocks: nil,
			valid:         true,
			reusable:      map[string]bool{"session-1": true},
			want:          nil,
			wantConsumed:  0,
			wantChecked:   false,
		},
		{
			// The same guard through the OTHER exclusion, so the fix cannot be
			// narrowed to the reusable tier and leave DNS-only mappings lying.
			name:          "a DNS-only mapping is not an assertion either",
			expected:      []models.MockEntry{{Name: "dns-1", Kind: "DNS"}, {Name: "dns-2", Kind: ""}},
			consumedMocks: []models.MockState{consumed("dns-1", models.DNS)},
			valid:         true,
			kindByName:    map[string]models.Kind{"dns-2": models.DNS},
			want:          nil,
			wantConsumed:  0,
			wantChecked:   false,
		},
		{
			// The BOUNDARY: one eligible entry among reusable ones is still a
			// real assertion. A guard that keyed off "any entry was filtered"
			// instead of "no entry survived" would silently stop reporting
			// missing dependencies for every mixed mapping.
			name: "one eligible entry among reusable ones still counts as checked",
			expected: []models.MockEntry{
				{Name: "session-1", Kind: "Http"},
				{Name: "gone", Kind: "Http"},
				{Name: "session-2", Kind: "Http"},
			},
			consumedMocks: nil,
			valid:         true,
			reusable:      map[string]bool{"session-1": true, "session-2": true},
			lookup:        map[string]mockDisplayInfo{"gone": {target: "GET api:80/orders"}},
			want: []models.DepResult{
				{Name: "deps[0] http GET api:80/orders (presence)", Type: "http", Meta: missingMeta()},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			name:          "empty mock lookup still produces a readable row",
			expected:      []models.MockEntry{{Name: "mock-1", Kind: "Redis"}},
			consumedMocks: nil,
			valid:         true,
			want: []models.DepResult{
				{
					Name: "deps[0] redis (presence)",
					Type: "redis",
					Meta: missingMeta(),
				},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			name:          "kind resolved from the lookup when the mapping entry has none",
			expected:      []models.MockEntry{{Name: "mock-1", Kind: ""}},
			consumedMocks: nil,
			valid:         true,
			kindByName:    map[string]models.Kind{"mock-1": models.Kind("Mongo")},
			lookup:        map[string]mockDisplayInfo{"mock-1": {target: "MongoDB find"}},
			want: []models.DepResult{
				{
					Name: "deps[0] mongo MongoDB find (presence)",
					Type: "mongo",
					Meta: missingMeta(),
				},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			// depTypeFor's THIRD fallback: the mapping entry carries no Kind
			// and the loaded-mock registry has none either, so the only
			// protocol left is the one mockDisplayInfo captured. Deleting that
			// arm used to leave the whole package green.
			name:          "kind resolved from the display lookup when neither the entry nor the registry has one",
			expected:      []models.MockEntry{{Name: "mock-1", Kind: ""}},
			consumedMocks: nil,
			valid:         true,
			kindByName:    nil,
			lookup:        map[string]mockDisplayInfo{"mock-1": {protocol: "PostgresV2", target: "db:5432 SELECT"}},
			want: []models.DepResult{
				{
					// Normalised through models.DepTypeForKind on the way out:
					// the lookup carries the PARSER VERSION, the row must not.
					Name: "deps[0] postgres db:5432 SELECT (presence)",
					Type: "postgres",
					Meta: missingMeta(),
				},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
		{
			name: "row indices skip the FILTERED entries, not the consumed ones",
			expected: []models.MockEntry{
				{Name: "dns-1", Kind: "DNS"},
				{Name: "mock-1", Kind: "Http"},
				{Name: "session-1", Kind: "Http"},
				{Name: "mock-2", Kind: "Http"},
			},
			consumedMocks: nil,
			valid:         true,
			reusable:      map[string]bool{"session-1": true},
			want: []models.DepResult{
				{Name: "deps[0] http (presence)", Type: "http", Meta: missingMeta()},
				{Name: "deps[1] http (presence)", Type: "http", Meta: missingMeta()},
			},
			wantConsumed: 0,
			wantChecked:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDepResults(tt.expected, tt.consumedMocks, tt.valid, tt.reusable, tt.kindByName, tt.lookup)
			assert.Equal(t, tt.want, got.Rows, "MISSING rows")
			assert.Equal(t, tt.wantConsumed, got.Consumed,
				"Consumed count — this is the ONLY persisted trace of the dependencies the test did exercise")
			assert.Equal(t, tt.wantChecked, got.Checked,
				"Checked is the one bit that tells `dep_result: []` meaning 'nothing missing' apart from "+
					"'the assertion never ran'; a consumer applying any(matched == false) reads the second as green")
		})
	}
}

// THE DEFECT, spelled out end to end on the shape a real recording produces.
//
// Reproduced against the http-pokeapi sample: its recorded outgoing HTTP calls
// carry no per-test tier tag, so models.Mock.DeriveLifetime's legacy kind
// fallback classifies them session-tier, isReusableTierMock is true for every
// one of them, and the per-entry filter in buildDepResults removes the lot.
// The function nonetheless returned Checked=true, and the persisted report read
//
//	deps_checked: true
//	deps_consumed: 0
//	dep_result: []
//
// which models.Result.DepsChecked, models.Result.DependenciesChecked and the
// NDJSON `dependencies_checked` all document as "the assertion ran and found
// nothing missing". Nothing had ever been eligible to be checked. An agent
// applying the documented rule (`checked && any(matched == false)`) reads that
// as a clean dependency verdict — the exact false green this slice exists to
// close, re-created inside the surface built to close it.
func TestBuildDepResults_NothingEligibleIsNotAGreenAssertion(t *testing.T) {
	// Two ordinary recorded outgoing HTTP calls, both session-tier.
	expected := []models.MockEntry{{Name: "mock-2", Kind: "Http"}, {Name: "mock-3", Kind: "Http"}}
	reusable := map[string]bool{"mock-2": true, "mock-3": true}

	dep := buildDepResults(expected, nil, true, reusable, nil, nil)

	res := models.Result{DepResult: dep.Rows, DepsChecked: dep.Checked, DepsConsumed: dep.Consumed}
	if res.DependenciesChecked() {
		t.Fatalf("the report claims the dependency assertion RAN (deps_checked=%v, deps_consumed=%d, dep_result=%v) "+
			"for a test whose every mapped dependency was filtered out before the assertion could look at it. "+
			"A consumer reading `checked && no failed rows` reports 'no dependency regressions' for a question "+
			"that was never asked.", dep.Checked, dep.Consumed, dep.Rows)
	}
	// Consistency: an unchecked verdict must not carry half an answer.
	assert.Equal(t, 0, dep.Consumed, "an unchecked verdict must not report a consumed count")
	assert.Nil(t, dep.Rows, "an unchecked verdict must not report rows")
	assert.False(t, res.HasMissingDeps(), "nothing was asserted, so nothing can be missing")
}

// The exclusion itself is NOT the bug and must not be 'fixed' by asserting
// reusable-tier mocks: they are recorded once at app boot and shared across
// every test, so a per-test presence assertion over them goes missing at random
// and turns healthy tests RED.
//
// So the mixed mapping is the case that pins both halves at once: the per-test
// entry is asserted, and the reusable one is neither counted as consumed (even
// though the agent did serve it) nor reported as missing (even though this
// test's window never consumed it).
func TestBuildDepResults_MixedMappingAssertsOnlyThePerTestEntry(t *testing.T) {
	expected := []models.MockEntry{
		{Name: "session-1", Kind: "Http"},
		{Name: "per-test-1", Kind: "Http"},
	}
	consumedMocks := []models.MockState{
		// The agent really did serve the session mock during this test...
		{Name: "session-1", Kind: models.HTTP, Lifetime: models.LifetimeSession},
		consumed("per-test-1", models.HTTP),
	}
	dep := buildDepResults(expected, consumedMocks, true, map[string]bool{"session-1": true}, nil, nil)

	assert.True(t, dep.Checked, "one eligible dependency is a real assertion")
	assert.Equal(t, 1, dep.Consumed,
		"...and it must NOT be counted: the consumed count is per-test dependencies only, "+
			"or 'consumed 2 of 2' would be reported for a test that exercised one")
	assert.Nil(t, dep.Rows, "the per-test dependency was consumed, so nothing is missing")

	// Now drop the reusable mock from the consumed side: it still must not
	// produce a row, or every test that ran before the session mock was
	// (re)served would report a phantom missing dependency.
	dep = buildDepResults(expected, []models.MockState{consumed("per-test-1", models.HTTP)}, true,
		map[string]bool{"session-1": true}, nil, nil)
	assert.True(t, dep.Checked)
	assert.Equal(t, 1, dep.Consumed)
	assert.Nil(t, dep.Rows,
		"a reusable-tier mock that this test's window did not consume must never be reported missing: "+
			"it is not attributable to one test, so the row would be a false RED")
}

// BACKWARD COMPAT. The eligibility guard changes what a report says ONLY for
// the tests it turns from a false "checked" into an honest "not checked"; a
// test with no dependency bookkeeping at all must serialize exactly as it did
// before this field had a writer.
//
// The baseline is a models.Result carrying no dependency data whatsoever —
// which is byte-for-byte what every report already on disk holds — so this
// compares the real struct tags rather than a hand-copied literal that could
// drift from them.
func TestBuildDepResults_UncheckedReportIsByteIdenticalToPreSliceReports(t *testing.T) {
	preSlice, err := yaml.Marshal(models.Result{})
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}

	cases := map[string]depAssertion{
		"no mapping for this test":         buildDepResults(nil, nil, true, nil, nil, nil),
		"assertion context not armed":      buildDepResults([]models.MockEntry{{Name: "m1", Kind: "Http"}}, nil, false, nil, nil, nil),
		"every mapped dependency filtered": buildDepResults([]models.MockEntry{{Name: "s1", Kind: "Http"}}, nil, true, map[string]bool{"s1": true}, nil, nil),
		"every mapped dependency is DNS":   buildDepResults([]models.MockEntry{{Name: "d1", Kind: "DNS"}}, nil, true, nil, nil, nil),
	}
	for name, dep := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := yaml.Marshal(models.Result{
				DepResult:    dep.Rows,
				DepsChecked:  dep.Checked,
				DepsConsumed: dep.Consumed,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != string(preSlice) {
				t.Fatalf("an unchecked dependency verdict serialises to:\n%s\nwant the pre-slice-4 shape:\n%s\n"+
					"WHY THIS MATTERS: every report already on disk carries `dep_result: []` and no deps_* keys. "+
					"An unchecked test must stay byte-identical to those, or every consumer diffing reports across "+
					"the upgrade sees a change on tests where nothing happened.", got, preSlice)
			}
		})
	}
}

// models.DepRowName is documented as a stable identifier an agent correlates
// across runs, and models.DepResult calls its shape a one-way door. Indexing
// by the position in the EMITTED slice broke that: with expectations [a b c],
// the run that lost only `a` and the run that lost only `c` both emitted
// "deps[0]" — the same name for two different dependencies.
func TestBuildDepResults_RowNameIsStableAcrossRuns(t *testing.T) {
	expected := []models.MockEntry{
		{Name: "a", Kind: "Postgres"},
		{Name: "b", Kind: "Postgres"},
		{Name: "c", Kind: "Postgres"},
	}
	lookup := map[string]mockDisplayInfo{
		"a": {target: "db:5432 SELECT"},
		"b": {target: "db:5432 INSERT"},
		"c": {target: "db:5432 UPDATE"},
	}

	// nameOf runs one replay in which exactly `lost` went unobserved and
	// returns the single MISSING row's name.
	nameOf := func(t *testing.T, lost string) string {
		t.Helper()
		var consumedMocks []models.MockState
		for _, m := range expected {
			if m.Name == lost {
				continue
			}
			consumedMocks = append(consumedMocks, consumed(m.Name, models.Kind("Postgres")))
		}
		names := models.MissingDepNames(buildDepResults(expected, consumedMocks, true, nil, nil, lookup).Rows)
		if len(names) != 1 {
			t.Fatalf("expected exactly one missing row for %q, got %v", lost, names)
		}
		return names[0]
	}

	want := map[string]string{
		"a": "deps[0] postgres db:5432 SELECT (presence)",
		"b": "deps[1] postgres db:5432 INSERT (presence)",
		"c": "deps[2] postgres db:5432 UPDATE (presence)",
	}
	for _, lost := range []string{"a", "b", "c"} {
		if got := nameOf(t, lost); got != want[lost] {
			t.Errorf("run that lost %q named the row %q, want %q — the index must number the RECORDING, not the emitted rows",
				lost, got, want[lost])
		}
	}

	// And the name must not move when EVERY dependency goes missing either.
	got := models.MissingDepNames(buildDepResults(expected, nil, true, nil, nil, lookup).Rows)
	if len(got) != 3 || got[2] != want["c"] {
		t.Fatalf("all-missing run renamed the rows: %v", got)
	}
}

// The report must not grow with the number of dependencies a test consumes,
// IN ANY MODE: it is written per test-set to disk, re-read by `keploy report`
// and uploaded to the fleet report store, and --assert-dependencies is sold
// as a CI gate — precisely where reports are written and uploaded most.
// Persistence is decoupled from the verdict knob for exactly that reason.
func TestBuildDepResults_ConsumedRowsDoNotScaleTheReport(t *testing.T) {
	const n = 200
	expected := make([]models.MockEntry, 0, n)
	consumedMocks := make([]models.MockState, 0, n)
	for i := range n {
		name := "mock-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		expected = append(expected, models.MockEntry{Name: name, Kind: "Postgres"})
		consumedMocks = append(consumedMocks, consumed(name, models.Kind("Postgres")))
	}

	dep := buildDepResults(expected, consumedMocks, true, nil, nil, nil)
	if len(dep.Rows) != 0 {
		t.Fatalf("%d consumed dependencies produced %d persisted rows; the consumed side must be a "+
			"COUNT, not rows — at ~190 bytes of YAML each this is 3-11 MB per test-set report on a "+
			"Postgres-chatty suite, uploaded to the fleet report store, that no consumer reads:\n%+v",
			n, len(dep.Rows), dep.Rows)
	}
	if dep.Consumed != n {
		t.Errorf("Consumed = %d, want %d", dep.Consumed, n)
	}
	if !dep.Checked {
		t.Error("a run with 200 consumed dependencies and nothing missing must still be provably CHECKED")
	}
}

// The one hard-constraint case: instrument+mapping mode, nothing missing, knob
// off. The persisted result must be byte-identical to a pre-slice-4 one apart
// from the two scalars — no `dep_result` entries at all. An earlier revision
// wrote an aggregate row here, which cost a measured +224 bytes per test on
// EVERY report ever written, forever, to encode one bit and one small int.
func TestBuildDepResults_CleanTestPersistsNoRows(t *testing.T) {
	expected := []models.MockEntry{{Name: "m1", Kind: "Postgres"}, {Name: "m2", Kind: "Postgres"}}
	consumedMocks := []models.MockState{
		consumed("m1", models.Kind("Postgres")), consumed("m2", models.Kind("Postgres")),
	}

	tcResult := &models.TestResult{Status: models.TestStatusPassed}
	dep := buildDepResults(expected, consumedMocks, true, nil, nil, nil)
	missing, level := attachDepResults(tcResult, models.TestStatusPassed, dep, false)

	if len(missing) != 0 || level != depLogNone {
		t.Fatalf("a clean test logged something: missing=%v level=%v", missing, level)
	}
	if len(tcResult.Result.DepResult) != 0 {
		t.Fatalf("a clean test persisted %d dependency rows, want 0:\n%+v",
			len(tcResult.Result.DepResult), tcResult.Result.DepResult)
	}
	if !tcResult.Result.DependenciesChecked() {
		t.Error("a clean test must still be provably CHECKED, or `dep_result: []` is ambiguous")
	}
	if tcResult.Result.DepsConsumed != 2 {
		t.Errorf("DepsConsumed = %d, want 2", tcResult.Result.DepsConsumed)
	}

	// The YAML the report actually carries.
	out, err := yaml.Marshal(tcResult.Result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "deps[") {
		t.Errorf("a clean test persisted a dependency row:\n%s", out)
	}
	if !strings.Contains(string(out), "dep_result: []") {
		t.Errorf("a clean test must keep the pre-slice-4 `dep_result: []`:\n%s", out)
	}
}

// THE OTHER BLOAT AXIS. The flagship scenario for this slice — a downstream
// service removed, a worker that stopped producing, a mock pool whose names
// drifted wholesale — makes EVERY mapped dependency of EVERY test unconsumed
// at once. Uncapped, that wrote a 5.4 MB test-set report at 100 tests x 200
// dependencies where the pre-slice build wrote 115 KB. The cap bounds it while
// keeping the failure visible.
func TestBuildDepResults_MissingRowsAreCapped(t *testing.T) {
	const n = 200
	expected := make([]models.MockEntry, 0, n)
	for i := range n {
		expected = append(expected, models.MockEntry{
			Name: fmt.Sprintf("mock-%03d", i), Kind: "Postgres",
		})
	}

	// Nothing consumed: every one of the 200 is missing.
	rows := buildDepResults(expected, nil, true, nil, nil, nil).Rows

	// depMissingRowCap individual rows + the overflow row.
	if len(rows) != depMissingRowCap+1 {
		t.Fatalf("%d missing dependencies produced %d rows, want %d", n, len(rows), depMissingRowCap+1)
	}
	// The individual rows are the FIRST ones in RECORDED order, so which
	// dependencies get named is a property of the mapping and does not shuffle
	// between two runs over the same data.
	for i := range depMissingRowCap {
		want := models.DepRowName(i, models.DepTypePostgres, "")
		if rows[i].Name != want {
			t.Fatalf("row %d is %q, want %q (missing rows must follow the recorded order)", i, rows[i].Name, want)
		}
	}
	overflow := rows[depMissingRowCap]
	if !models.IsDepMissingOverflow(overflow) {
		t.Fatalf("expected the overflow row at index %d, got %+v", depMissingRowCap, overflow)
	}
	if want := models.DepMissingOverflowRow(n - depMissingRowCap); overflow.Name != want.Name {
		t.Errorf("overflow row = %q, want %q", overflow.Name, want.Name)
	}
	// Capping must not hide the failure: every downstream reader keys off
	// these two.
	res := models.Result{DepResult: rows}
	if !res.HasMissingDeps() {
		t.Fatal("capping the missing rows made the lost dependencies invisible to HasMissingDeps")
	}
	if got := len(models.MissingDepNames(rows)); got != depMissingRowCap+1 {
		t.Errorf("MissingDepNames returned %d names, want %d", got, depMissingRowCap+1)
	}
}

// Exactly at the cap there is nothing to overflow: an "and 0 more" row would
// be a lie.
func TestBuildDepResults_NoOverflowRowAtOrBelowTheCap(t *testing.T) {
	for _, n := range []int{1, depMissingRowCap - 1, depMissingRowCap} {
		expected := make([]models.MockEntry, 0, n)
		for i := range n {
			expected = append(expected, models.MockEntry{Name: fmt.Sprintf("mock-%03d", i), Kind: "Postgres"})
		}
		rows := buildDepResults(expected, nil, true, nil, nil, nil).Rows
		if len(rows) != n {
			t.Fatalf("n=%d produced %d rows, want %d (n missing, no overflow)", n, len(rows), n)
		}
		for _, row := range rows {
			if models.IsDepMissingOverflow(row) {
				t.Fatalf("n=%d emitted an overflow row for nothing: %+v", n, row)
			}
		}
	}
}

// The persisted shape must not depend on the verdict knob at all. buildDepResults
// no longer takes it; this pins that it never grows one again.
func TestBuildDepResults_IsIndependentOfTheVerdictKnob(t *testing.T) {
	fn := reflect.TypeOf(buildDepResults)
	for i := range fn.NumIn() {
		if fn.In(i).Kind() != reflect.Bool {
			continue
		}
		if i != 2 {
			t.Fatalf("buildDepResults grew a second bool parameter at index %d. "+
				"If that is --assert-dependencies again, see buildDepResults' sizing note: "+
				"a verdict knob must not change what is persisted.", i)
		}
	}
}

// The DepResult rows and the mockSetMismatch verdict signal must be computed
// from the same filters, otherwise --assert-dependencies can fail a test with
// nothing rendered to explain it.
func TestBuildDepResults_MissingRowsAgreeWithMockSubset(t *testing.T) {
	tests := []struct {
		name     string
		expected []models.MockEntry
		consumed []models.MockState
		reusable map[string]bool
	}{
		{
			name:     "all consumed",
			expected: []models.MockEntry{{Name: "m1", Kind: "Http"}, {Name: "m2", Kind: "Http"}},
			consumed: []models.MockState{consumed("m1", models.HTTP), consumed("m2", models.HTTP)},
		},
		{
			name:     "one missing",
			expected: []models.MockEntry{{Name: "m1", Kind: "Http"}, {Name: "m2", Kind: "Http"}},
			consumed: []models.MockState{consumed("m1", models.HTTP)},
		},
		{
			name:     "missing one is reusable so it is not asserted",
			expected: []models.MockEntry{{Name: "m1", Kind: "Http"}, {Name: "m2", Kind: "Http"}},
			consumed: []models.MockState{consumed("m1", models.HTTP)},
			reusable: map[string]bool{"m2": true},
		},
		{
			name:     "DNS never counts as missing",
			expected: []models.MockEntry{{Name: "m1", Kind: "Http"}, {Name: "dns", Kind: "DNS"}},
			consumed: []models.MockState{consumed("m1", models.HTTP)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reproduce the call site's filtered name slices verbatim.
			var filteredExpected []string
			for _, m := range tt.expected {
				if isDNSMockEntry(m, nil) || tt.reusable[m.Name] {
					continue
				}
				filteredExpected = append(filteredExpected, m.Name)
			}
			var filteredConsumed []string
			for _, m := range tt.consumed {
				if m.Kind == models.DNS || isReusableTierState(m) {
					continue
				}
				filteredConsumed = append(filteredConsumed, m.Name)
			}
			mockSetMismatch := !isMockSubset(filteredConsumed, filteredExpected)

			dep := buildDepResults(tt.expected, tt.consumed, true, tt.reusable, nil, nil)
			result := models.Result{DepResult: dep.Rows, DepsChecked: dep.Checked, DepsConsumed: dep.Consumed}
			assert.Equal(t, mockSetMismatch, result.HasMissingDeps(),
				"missing dependency rows must agree with the mockSetMismatch verdict signal")
			assert.True(t, result.DependenciesChecked(),
				"a valid assertion must always leave persisted evidence that it ran")
		})
	}
}

func TestMockTargetFromSpec(t *testing.T) {
	tests := []struct {
		name string
		mock *models.Mock
		want string
	}{
		{name: "nil mock", mock: nil, want: ""},
		{
			name: "http carries method and path, not just the host",
			mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://api.internal:8080/orders"},
			}},
			want: "GET api.internal:8080/orders",
		},
		{
			name: "two calls to one host are distinguishable",
			mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://api.internal:8080/payments"},
			}},
			want: "POST api.internal:8080/payments",
		},
		{
			name: "http wins over destAddr, which would collapse the path away",
			mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
				Metadata: map[string]string{"destAddr": "api.internal:8080"},
				HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://api.internal:8080/orders"},
			}},
			want: "GET api.internal:8080/orders",
		},
		{
			name: "destAddr is used for protocols with no request URL",
			mock: &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
				Metadata: map[string]string{"destAddr": "mysql:3306"},
			}},
			want: "mysql:3306",
		},
		{
			// The MySQL v2 recorder writes requestOperation, not operation, so
			// MockSummaryFromSpec returns a bare "MySQL" and is rejected as a
			// target. Without folding it into destAddr, EVERY MySQL row of a
			// test reads "mysql:3306" and the index is the only discriminator.
			name: "the MySQL v2 operation is folded into the address",
			mock: &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
				Metadata: map[string]string{
					"destAddr":          "mysql:3306",
					"requestOperation":  "COM_QUERY",
					"responseOperation": "OK",
				},
			}},
			want: "mysql:3306 COM_QUERY",
		},
		{
			// The response side names the server's reply status, not the call,
			// so it must never become the target on its own.
			name: "responseOperation alone is not an identity",
			mock: &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
				Metadata: map[string]string{"destAddr": "mysql:3306", "responseOperation": "OK"},
			}},
			want: "mysql:3306",
		},
		{
			name: "a v1 operation still wins over the request-side alias",
			mock: &models.Mock{Kind: models.Kind("PostgresV2"), Spec: models.MockSpec{
				Metadata: map[string]string{
					"destAddr":         "db:5432",
					"operation":        "INSERT",
					"requestOperation": "P",
				},
			}},
			want: "db:5432 INSERT",
		},
		{
			name: "the protocol-generic summary beats a bare operation token",
			mock: &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
				Metadata:      map[string]string{"operation": "find"},
				MongoRequests: []models.MongoRequest{{}},
			}},
			want: "MongoDB find",
		},
		{
			name: "operation is the last resort",
			mock: &models.Mock{Kind: models.Kind("Redis"), Spec: models.MockSpec{
				Metadata: map[string]string{"operation": "GET user:1"},
			}},
			want: "Redis GET user:1",
		},
		{
			// No destAddr, no summary (requestOperation is invisible to
			// MockSummaryFromSpec) — the bare verb is all that is left.
			name: "the request-side operation is the last resort too",
			mock: &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
				Metadata: map[string]string{"requestOperation": "COM_STMT_PREPARE"},
			}},
			want: "COM_STMT_PREPARE",
		},
		{
			name: "nothing identifying yields an empty target, not the bare kind",
			mock: &models.Mock{Kind: models.Kind("Redis"), Spec: models.MockSpec{}},
			want: "",
		},
		{
			name: "unparseable http url still names the method and the raw url",
			mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{Method: "GET", URL: "://nope"},
			}},
			want: "GET ://nope",
		},
		{
			name: "a relative http url keeps the path",
			mock: &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{Method: "GET", URL: "/orders/1"},
			}},
			want: "GET /orders/1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mockTargetFromSpec(tt.mock))
		})
	}
}

func TestDependencyAssertionKnob(t *testing.T) {
	tests := []struct {
		name               string
		mockSetMismatch    bool
		assertDependencies bool
		strictMockReject   bool
		strictFailure      bool
		nonDemotable       bool
		wantDepAssertFail  bool
		wantDemote         bool
	}{
		{
			name:            "knob off: a diverged mock set still demotes to OBSOLETE (historical default)",
			mockSetMismatch: true,
			wantDemote:      true,
		},
		{
			name:               "knob on: a diverged mock set becomes a real failure",
			mockSetMismatch:    true,
			assertDependencies: true,
			wantDepAssertFail:  true,
			wantDemote:         false,
		},
		{
			name:               "knob on with no divergence changes nothing",
			mockSetMismatch:    false,
			assertDependencies: true,
			wantDepAssertFail:  false,
			wantDemote:         false,
		},
		{
			name:             "schema-noise-strict rejection also vetoes the demotion",
			mockSetMismatch:  true,
			strictMockReject: true,
			wantDemote:       false,
		},
		{
			name:            "strict-failure keeps vetoing the demotion on its own",
			mockSetMismatch: true,
			strictFailure:   true,
			wantDemote:      false,
		},
		{
			name:            "no divergence never demotes",
			mockSetMismatch: false,
			wantDemote:      false,
		},
		{
			// THE CONSUMER RULE. Not a knob: no configuration exists in
			// which grading a consumer's unconsumed effect mock as OBSOLETE
			// (which does not fail the test set and does not change the exit
			// code) is correct.
			name:            "a non-demotable Kind vetoes the demotion with every knob off",
			mockSetMismatch: true,
			nonDemotable:    true,
			wantDemote:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			depAssertFail := dependencyAssertionRejects(tt.mockSetMismatch, tt.assertDependencies)
			assert.Equal(t, tt.wantDepAssertFail, depAssertFail)
			assert.Equal(t, tt.wantDemote,
				demoteToObsolete(tt.mockSetMismatch, tt.strictMockReject, depAssertFail, tt.strictFailure, tt.nonDemotable))
		})
	}
}

// resolveTestStatus is the demotion ALGEBRA, downstream of the promotions:
// `responseMatched` here already accounts for a veto having flipped it. The
// entry point RunTestSet calls is resolveTestOutcome (below), which is where
// the --assert-dependencies promotion itself lives.
func TestResolveTestStatus(t *testing.T) {
	tests := []struct {
		name             string
		testPass         bool
		mockSetMismatch  bool
		strictMockReject bool
		depAssertFail    bool
		strictFailure    bool
		neverDemotable   bool
		wantStatus       models.TestStatus
		wantFailsSet     bool
	}{
		{
			name: "a clean pass", testPass: true,
			wantStatus: models.TestStatusPassed, wantFailsSet: false,
		},
		{
			name:     "the silent-green case: response matched, dependency vanished, knob OFF",
			testPass: true, mockSetMismatch: true,
			wantStatus: models.TestStatusPassed, wantFailsSet: false,
		},
		{
			// UNREACHABLE FROM PRODUCTION, and written down so it stays that
			// way: resolveTestOutcome flips responseMatched to false before
			// calling this when the knob promotes, so this input combination
			// never occurs. If it ever did, PASSED would be the wrong answer —
			// which is exactly why the promotion cannot live here. See
			// TestResolveTestOutcome for the reachable knob-ON case.
			name:     "post-flip contract: a still-matching response is PASSED even with depAssertFail set",
			testPass: true, mockSetMismatch: true, depAssertFail: true,
			wantStatus: models.TestStatusPassed, wantFailsSet: false,
		},
		{
			name:       "a plain response failure is FAILED and reddens the set",
			testPass:   false,
			wantStatus: models.TestStatusFailed, wantFailsSet: true,
		},
		{
			name:     "knob OFF: a diverged mock set is demoted to OBSOLETE and does NOT redden the set",
			testPass: false, mockSetMismatch: true,
			wantStatus: models.TestStatusObsolete, wantFailsSet: false,
		},
		{
			name:     "knob ON: the same case is FAILED and reddens the set",
			testPass: false, mockSetMismatch: true, depAssertFail: true,
			wantStatus: models.TestStatusFailed, wantFailsSet: true,
		},
		{
			name:     "--strict-failure vetoes the demotion on its own",
			testPass: false, mockSetMismatch: true, strictFailure: true,
			wantStatus: models.TestStatusFailed, wantFailsSet: true,
		},
		{
			name:     "--schema-noise-strict vetoes the demotion on its own",
			testPass: false, mockSetMismatch: true, strictMockReject: true,
			wantStatus: models.TestStatusFailed, wantFailsSet: true,
		},
		{
			// RULE 3, THE WHOLE POINT OF neverDemotableKind. Without the veto this
			// row is OBSOLETE + failsSet:false, which is the silent green of
			// design §5 row 0: the worker stopped producing, the test set is
			// not marked failed, and the run exits 0.
			name:     "a non-demotable Kind is FAILED and reddens the set instead of being demoted",
			testPass: false, mockSetMismatch: true, neverDemotable: true,
			wantStatus: models.TestStatusFailed, wantFailsSet: true,
		},
		{
			// Post-flip contract, mirroring the depAssertFail row above:
			// resolveTestOutcome flips responseMatched to false before
			// calling this, so a still-matching response here stays PASSED.
			// The reachable case is in TestResolveTestOutcome.
			name:     "post-flip contract: a still-matching response is PASSED even with neverDemotable set",
			testPass: true, mockSetMismatch: true, neverDemotable: true,
			wantStatus: models.TestStatusPassed, wantFailsSet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, failsSet := resolveTestStatus(tt.testPass, tt.mockSetMismatch, tt.strictMockReject, tt.depAssertFail, tt.strictFailure, tt.neverDemotable)
			assert.Equal(t, tt.wantStatus, status)
			assert.Equal(t, tt.wantFailsSet, failsSet)
		})
	}
}

// resolveTestOutcome is THE verdict seam RunTestSet calls, and the only place
// the --assert-dependencies promotion exists. It takes the PRE-promotion
// response result, which is what makes the flagship case assertable at all:
// "the response matched but a recorded outgoing call was not observed" becomes
// a failure by flipping that input, so a function that only saw the flipped
// value could never be tested for it.
func TestResolveTestOutcome(t *testing.T) {
	tests := []struct {
		name               string
		responseMatched    bool
		mockSetMismatch    bool
		schemaNoiseStrict  bool
		assertDependencies bool
		strictFailure      bool
		neverDemotable     bool
		effectMockMissing  bool
		want               testOutcome
	}{
		{
			name:            "a clean pass",
			responseMatched: true,
			want: testOutcome{
				Status: models.TestStatusPassed, Log: mismatchLogNone,
			},
		},
		{
			name: "a plain response failure, no mock-set divergence",
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true, Log: mismatchLogNone,
			},
		},
		{
			name:               "the knob on a clean run changes nothing",
			responseMatched:    true,
			assertDependencies: true,
			want: testOutcome{
				Status: models.TestStatusPassed, Log: mismatchLogNone,
			},
		},
		{
			// THE SILENT-GREEN CASE, default behaviour: the response matched,
			// a recorded outgoing call was not observed, and the run stays
			// green. Visibility comes from the DepResult rows, not the verdict.
			name:            "response matched, dependency not observed, knob OFF: still PASSED and still green",
			responseMatched: true, mockSetMismatch: true,
			want: testOutcome{
				Status: models.TestStatusPassed, Log: mismatchLogIgnoredResponseMatched,
			},
		},
		{
			// THE FLAGSHIP PROMOTION. This is the whole reason
			// --assert-dependencies exists and is distinct from
			// --strict-failure, which cannot reach a test whose response
			// matched.
			name:            "response matched, dependency not observed, knob ON: FAILED and the set goes red",
			responseMatched: true, mockSetMismatch: true, assertDependencies: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogDependencyReject,
				DepAssertFail: true,
			},
		},
		{
			name:            "--strict-failure alone cannot reach a response that matched",
			responseMatched: true, mockSetMismatch: true, strictFailure: true,
			want: testOutcome{
				Status: models.TestStatusPassed, Log: mismatchLogIgnoredResponseMatched,
			},
		},
		{
			name:            "--schema-noise-strict promotes a matching response too, and says so",
			responseMatched: true, mockSetMismatch: true, schemaNoiseStrict: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogSchemaNoiseReject,
			},
		},
		{
			// Both promotions armed: schema-noise wins the LOG (it names the
			// more specific cause), the verdict is the same either way.
			name:            "schema-noise-strict wins the explanation when both knobs are on",
			responseMatched: true, mockSetMismatch: true, schemaNoiseStrict: true, assertDependencies: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogSchemaNoiseReject,
				DepAssertFail: true,
			},
		},
		{
			name:            "response failed AND the mock set diverged, knob OFF: the historical OBSOLETE demotion",
			mockSetMismatch: true,
			want: testOutcome{
				Status: models.TestStatusObsolete, RecordMismatch: true, Log: mismatchLogObsolete,
			},
		},
		{
			// The wording matters: telling a user to re-record a test keploy
			// is about to mark FAILED points them at the wrong fix.
			name:            "response failed, knob ON: FAILED, and the log names the flag instead of saying re-record",
			mockSetMismatch: true, assertDependencies: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogVetoedFailure,
				VetoFlags: "--assert-dependencies", DepAssertFail: true,
			},
		},
		{
			name:            "response failed, --strict-failure: FAILED, and the log names that flag",
			mockSetMismatch: true, strictFailure: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogVetoedFailure,
				VetoFlags: "--strict-failure",
			},
		},
		{
			name:            "both vetoes are named",
			mockSetMismatch: true, assertDependencies: true, strictFailure: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogVetoedFailure,
				VetoFlags: "--assert-dependencies, --strict-failure", DepAssertFail: true,
			},
		},
		{
			// THE FLAGSHIP CONSUMER REGRESSION, at the seam that decides it.
			// The worker's effects compared clean (there were none to
			// compare) and its effect mock went unconsumed — for a consumer
			// those two statements cannot both be true, because an effect
			// mock nothing consumed IS an effect the worker did not produce.
			// With no knobs at all this must be FAILED and must redden the
			// set; the historical answer was PASSED + green.
			name:            "response matched, effect mock unconsumed, non-demotable Kind: FAILED with no knob at all",
			responseMatched: true, mockSetMismatch: true, neverDemotable: true, effectMockMissing: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogNonDemotableReject,
			},
		},
		{
			// And the log must NOT be one of the two that name a flag: there
			// is no invocation to change here, so pointing the reader at one
			// sends them to the wrong place.
			name:            "response failed AND the effect mock was unconsumed, non-demotable Kind",
			mockSetMismatch: true, neverDemotable: true, effectMockMissing: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogVetoedFailure,
				VetoFlags: "this test case Kind is never demoted to obsolete",
			},
		},
		{
			name:            "a non-demotable Kind that simply passed is untouched",
			responseMatched: true, neverDemotable: true, effectMockMissing: true,
			want: testOutcome{
				Status: models.TestStatusPassed, Log: mismatchLogNone,
			},
		},
		{
			// The non-demotion outranks --schema-noise-strict's log because
			// it names the more actionable cause for this Kind; the verdict
			// is the same either way.
			name:            "non-demotable wins the explanation over schema-noise-strict",
			responseMatched: true, mockSetMismatch: true, neverDemotable: true, effectMockMissing: true, schemaNoiseStrict: true,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogSchemaNoiseReject,
			},
		},
		{
			// THE BLOCKER THIS SPLIT EXISTS FOR. The JUDGE failed this
			// consumer test — an effect diff, a completion timeout, a named
			// refusal — and the mock set diverged on something that is NOT an
			// effect mock: a cached OffsetFetch the client skipped (design §4
			// P4's own example), or the trigger itself when a parser forgets
			// the DeleteFilteredMock / GetConsumedMocks bookkeeping.
			//
			// While one boolean carried both claims, narrowing it for the
			// promotion also narrowed this: the test was persisted OBSOLETE,
			// did not fail the test set, and the run exited 0 — design §5's
			// false-pass row 0 reopened on the one Kind whose stated guard is
			// "the CONSUMER verdict is non-demotable".
			name:            "the JUDGE failed a consumer test and the diverged mock is NOT an effect mock: still FAILED, never OBSOLETE",
			mockSetMismatch: true, neverDemotable: true, effectMockMissing: false,
			want: testOutcome{
				Status: models.TestStatusFailed, FailsTestSet: true,
				RecordMismatch: true, Log: mismatchLogVetoedFailure,
				VetoFlags: "this test case Kind is never demoted to obsolete",
			},
		},
		{
			// THE OTHER HALF OF THE SPLIT, and the false RED it prevents. The
			// judge PASSED this consumer test and the only unconsumed mock is
			// coordination traffic. Promoting here would fail a clean test and
			// tell the reader the worker stopped producing, which is not what
			// happened. The by-Kind bit must NOT reach the promotion arm.
			name:            "the judge passed and only coordination traffic went unconsumed: still PASSED",
			responseMatched: true, mockSetMismatch: true, neverDemotable: true, effectMockMissing: false,
			want: testOutcome{
				Status: models.TestStatusPassed, Log: mismatchLogIgnoredResponseMatched,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTestOutcome(tt.responseMatched, tt.mockSetMismatch, tt.schemaNoiseStrict, tt.assertDependencies, tt.strictFailure, tt.neverDemotable, tt.effectMockMissing)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Whatever else changes, a green default run must stay green: with every knob
// off, no input can produce a FAILED test set that the pre-slice-4 code would
// not also have produced (i.e. FailsTestSet implies the response failed and
// the mock set agreed).
func TestResolveTestOutcome_DefaultsAreBackwardCompatible(t *testing.T) {
	for _, responseMatched := range []bool{false, true} {
		for _, mismatch := range []bool{false, true} {
			got := resolveTestOutcome(responseMatched, mismatch, false, false, false, false, false)
			want, wantFails := resolveTestStatus(responseMatched, mismatch, false, false, false, false)
			assert.Equal(t, want, got.Status,
				"responseMatched=%v mismatch=%v", responseMatched, mismatch)
			assert.Equal(t, wantFails, got.FailsTestSet,
				"responseMatched=%v mismatch=%v", responseMatched, mismatch)
			assert.False(t, got.DepAssertFail)
		}
	}
}

// A diverged mock set normally suppresses the response diff as "re-record
// noise". Under --assert-dependencies the same test is about to be marked
// FAILED, so suppressing its diff hands the user a red test with nothing to
// look at.
func TestShouldEmitFailureLogs(t *testing.T) {
	tests := []struct {
		name               string
		mockSetMismatch    bool
		assertDependencies bool
		neverDemotable     bool
		want               bool
	}{
		{name: "no divergence: diffs as always", want: true},
		{name: "no divergence, knob on", assertDependencies: true, want: true},
		{name: "divergence, knob off: historical suppression", mockSetMismatch: true, want: false},
		{
			name:            "divergence, knob on: the test is going red, so show the diff",
			mockSetMismatch: true, assertDependencies: true, want: true,
		},
		{
			// For a consumer test the divergence IS the finding, so the
			// historical suppression would hide the diff on exactly the
			// regression the contract exists to catch.
			name:            "divergence on a non-demotable Kind: never suppressed",
			mockSetMismatch: true, neverDemotable: true, want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldEmitFailureLogs(tt.mockSetMismatch, tt.assertDependencies, tt.neverDemotable))
		})
	}
}

// recordUnexercised is the single writer of the set warnUnexercisedDependencies
// reports on. Emptying that set is otherwise an invisible mutation: the wiring
// test pins the CALL to the summary, but a summary over an empty map is a
// silent no-op.
func TestRecordUnexercised(t *testing.T) {
	tests := []struct {
		name   string
		level  depLogLevel
		status models.TestStatus
		want   map[string]models.TestStatus
	}{
		{
			name:  "reported-only: collected, with its status",
			level: depLogDebug, status: models.TestStatusPassed,
			want: map[string]models.TestStatus{"tc": models.TestStatusPassed},
		},
		{
			name:  "a demoted test is collected too, as OBSOLETE",
			level: depLogDebug, status: models.TestStatusObsolete,
			want: map[string]models.TestStatus{"tc": models.TestStatusObsolete},
		},
		{
			name:  "nothing missing: nothing collected",
			level: depLogNone, status: models.TestStatusPassed,
			want: map[string]models.TestStatus{},
		},
		{
			// Under the knob the per-test Error already fired and the run is
			// red; a summary Warn on top would be noise.
			name:  "knob on: the per-test Error already covered it",
			level: depLogError, status: models.TestStatusFailed,
			want: map[string]models.TestStatus{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string]models.TestStatus{}
			recordUnexercised(got, "tc", tt.status, tt.level)
			assert.Equal(t, tt.want, got)
		})
	}

	// A nil set must not panic.
	recordUnexercised(nil, "tc", models.TestStatusPassed, depLogDebug)
}

// --retry-passing re-runs the passing tests up to five times and the LAST
// cycle's rows are the ones persisted (finalTestCaseResults is overwritten each
// cycle). A cumulative set would keep a test that lost a dependency in cycle 1
// and consumed it in cycle 2, so the end-of-test-set Warn would name a test
// whose own report carries no missing row — a WARN contradicting the report it
// points the user at, in exactly the flake scenario --retry-passing exists to
// smooth over.
func TestRecordUnexercised_LastCycleWins(t *testing.T) {
	tests := []struct {
		name   string
		cycles []depLogLevel
		want   map[string]models.TestStatus
	}{
		{
			name:   "lost it, then recovered on retry: dropped",
			cycles: []depLogLevel{depLogDebug, depLogNone},
			want:   map[string]models.TestStatus{},
		},
		{
			name:   "recovered, then lost it again: kept",
			cycles: []depLogLevel{depLogDebug, depLogNone, depLogDebug},
			want:   map[string]models.TestStatus{"tc": models.TestStatusPassed},
		},
		{
			name:   "still missing on every cycle: kept exactly once",
			cycles: []depLogLevel{depLogDebug, depLogDebug, depLogDebug},
			want:   map[string]models.TestStatus{"tc": models.TestStatusPassed},
		},
		{
			// The knob turns the per-test line into an Error and the run is
			// already red, so the summary drops the test as well.
			name:   "escalated to the knob's Error on a later cycle: dropped",
			cycles: []depLogLevel{depLogDebug, depLogError},
			want:   map[string]models.TestStatus{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string]models.TestStatus{}
			for _, level := range tt.cycles {
				recordUnexercised(got, "tc", models.TestStatusPassed, level)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// The per-test-set summary must not claim "the response still matched" over a
// population that includes demoted tests, whose response did NOT match — that
// is exactly the subset a user is most likely to go and investigate.
func TestUnexercisedSummary(t *testing.T) {
	tests := []struct {
		name       string
		in         map[string]models.TestStatus
		wantNames  []string
		wantSample []string
		wantPassed int
	}{
		{
			name: "mixed population: only the PASSED ones are counted as still-matching",
			in: map[string]models.TestStatus{
				"tc-b": models.TestStatusPassed,
				"tc-a": models.TestStatusObsolete,
				"tc-c": models.TestStatusPassed,
			},
			wantNames:  []string{"tc-a", "tc-b", "tc-c"},
			wantSample: []string{"tc-a", "tc-b", "tc-c"},
			wantPassed: 2,
		},
		{
			name:       "all demoted: zero still matched",
			in:         map[string]models.TestStatus{"tc-a": models.TestStatusObsolete},
			wantNames:  []string{"tc-a"},
			wantSample: []string{"tc-a"},
			wantPassed: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, sample, passed := unexercisedSummary(tt.in)
			assert.Equal(t, tt.wantNames, names)
			assert.Equal(t, tt.wantSample, sample)
			assert.Equal(t, tt.wantPassed, passed)
		})
	}
}

// A fully drifted suite must not emit a thousand-name log line.
func TestUnexercisedSummary_SampleIsCapped(t *testing.T) {
	in := map[string]models.TestStatus{}
	for i := range 50 {
		in[string(rune('a'+i%26))+string(rune('a'+i/26))] = models.TestStatusPassed
	}
	names, sample, passed := unexercisedSummary(in)
	assert.Len(t, names, 50)
	assert.Len(t, sample, depWarnSampleSize)
	assert.Equal(t, 50, passed)
	assert.Equal(t, names[:depWarnSampleSize], sample)
}

// attachDepResults is the writer seam RunTestSet calls. Neutering it (dropping
// the assignment, or dropping the category, or flattening the log level) has to
// fail here.
func TestAttachDepResults(t *testing.T) {
	missingRow := models.DepResult{
		Name: "deps[0] postgres db:5432 (presence)", Type: "postgres", Meta: missingMeta(),
	}
	tests := []struct {
		name               string
		status             models.TestStatus
		dep                depAssertion
		assertDependencies bool
		existingCategories []models.FailureCategory
		wantMissing        []string
		wantLevel          depLogLevel
		wantCategories     []models.FailureCategory
	}{
		{
			name:           "the assertion never ran: nothing is written, nothing is logged",
			status:         models.TestStatusPassed,
			wantLevel:      depLogNone,
			wantCategories: nil,
		},
		{
			name:           "everything consumed: the scalars persist, no rows, no category, no log",
			status:         models.TestStatusPassed,
			dep:            depAssertion{Checked: true, Consumed: 3},
			wantLevel:      depLogNone,
			wantCategories: nil,
		},
		{
			name:        "PASSED with a missing dependency keeps the rows but NEVER gets a failure category",
			status:      models.TestStatusPassed,
			dep:         depAssertion{Checked: true, Consumed: 3, Rows: []models.DepResult{missingRow}},
			wantMissing: []string{missingRow.Name},
			wantLevel:   depLogDebug,
			// The silent-green case: visible via the rows, not via a label.
			wantCategories: nil,
		},
		{
			name:           "OBSOLETE with a missing dependency is labelled",
			status:         models.TestStatusObsolete,
			dep:            depAssertion{Checked: true, Rows: []models.DepResult{missingRow}},
			wantMissing:    []string{missingRow.Name},
			wantLevel:      depLogDebug,
			wantCategories: []models.FailureCategory{models.DependencyMissing},
		},
		{
			name:               "FAILED under the knob is labelled and logged at Error",
			status:             models.TestStatusFailed,
			dep:                depAssertion{Checked: true, Rows: []models.DepResult{missingRow}},
			assertDependencies: true,
			wantMissing:        []string{missingRow.Name},
			wantLevel:          depLogError,
			wantCategories:     []models.FailureCategory{models.DependencyMissing},
		},
		{
			name:               "the category is appended to what the matcher already set, without duplicating",
			status:             models.TestStatusFailed,
			dep:                depAssertion{Checked: true, Rows: []models.DepResult{missingRow}},
			existingCategories: []models.FailureCategory{models.AppConnectionError, models.DependencyMissing},
			wantMissing:        []string{missingRow.Name},
			wantLevel:          depLogDebug,
			wantCategories:     []models.FailureCategory{models.AppConnectionError, models.DependencyMissing},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tcResult := &models.TestResult{Status: tt.status}
			tcResult.FailureInfo.Category = tt.existingCategories

			missing, level := attachDepResults(tcResult, tt.status, tt.dep, tt.assertDependencies)

			assert.Equal(t, tt.dep.Rows, tcResult.Result.DepResult, "the rows must land on the persisted result")
			assert.Equal(t, tt.dep.Checked, tcResult.Result.DepsChecked,
				"DepsChecked is the one bit that disambiguates `dep_result: []`; dropping it makes "+
					"'checked and clean' indistinguishable from 'never checked' on disk")
			assert.Equal(t, tt.dep.Consumed, tcResult.Result.DepsConsumed,
				"DepsConsumed is the only persisted trace of the dependencies the test DID exercise")
			assert.Equal(t, tt.wantMissing, missing)
			assert.Equal(t, tt.wantLevel, level)
			assert.Equal(t, tt.wantCategories, tcResult.FailureInfo.Category)
		})
	}
}

// The matcher owns FailureInfo.Category and the persisted Result shares its
// backing array. Appending in place would mutate data the writer does not own.
func TestAttachDepResults_DoesNotMutateTheCallersCategorySlice(t *testing.T) {
	shared := make([]models.FailureCategory, 1, 4) // spare capacity: a plain append writes in place
	shared[0] = models.AppConnectionError
	alias := shared[:1:4]

	tcResult := &models.TestResult{Status: models.TestStatusFailed}
	tcResult.FailureInfo.Category = alias

	attachDepResults(tcResult, models.TestStatusFailed, depAssertion{
		Checked: true,
		Rows:    []models.DepResult{{Name: "deps[0] postgres x (presence)", Meta: missingMeta()}},
	}, false)

	if len(shared) != 1 || shared[0] != models.AppConnectionError {
		t.Fatalf("caller's slice header changed: %v", shared)
	}
	if got := shared[:2]; got[1] == models.DependencyMissing {
		t.Fatalf("the category was appended into the caller's backing array: %v", got)
	}
	assert.Equal(t,
		[]models.FailureCategory{models.AppConnectionError, models.DependencyMissing},
		tcResult.FailureInfo.Category)
}

func TestAttachDepResults_NilResultIsSafe(t *testing.T) {
	missing, level := attachDepResults(nil, models.TestStatusFailed, depAssertion{
		Checked: true,
		Rows:    []models.DepResult{{Name: "deps[0] x (presence)", Meta: missingMeta()}},
	}, true)
	assert.Nil(t, missing)
	assert.Equal(t, depLogNone, level)
}

func TestDependencyAssertionInertReason(t *testing.T) {
	tests := []struct {
		name                string
		instrument          bool
		useMappingBased     bool
		isMappingEnabled    bool
		consumedFetchFailed bool
		deferredStreaming   bool
		noEligibleDeps      bool
		wantSubstring       string
		// wantAbsent pins a string the reason must NOT contain. Precedence
		// between two simultaneously-true reasons is only half-tested by
		// asserting the winner's wording: a message that concatenated both
		// would pass that and still mis-direct the reader.
		wantAbsent string
	}{
		{
			name:       "everything armed: the assertion can run",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			wantSubstring: "",
		},
		{
			name:       "base-path / remote agent: the per-test mapping is never armed",
			instrument: false, useMappingBased: true, isMappingEnabled: true,
			wantSubstring: "not instrument mode",
		},
		{
			name:       "mapping disabled",
			instrument: true, useMappingBased: true, isMappingEnabled: false,
			wantSubstring: "test.disableMapping",
		},
		{
			name:       "a legacy test set with no usable mappings.yaml",
			instrument: true, useMappingBased: false, isMappingEnabled: true,
			wantSubstring: "--update-test-mapping",
		},
		{
			// THE SIXTH REASON. Every set-wide precondition holds, so
			// nothing else in this function fires — but RunTestSet's Phase-2
			// pass writes no DepResult and never resolves an outcome, so the
			// flag is inert for those test cases anyway. Without this case
			// --assert-dependencies over an SSE suite exits 0 in silence.
			name:       "a healthy test set that defers streaming test cases",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			deferredStreaming: true,
			wantSubstring:     "streaming",
		},
		{
			// PRECEDENCE. A set-wide precondition disables the assertion for
			// the streaming tests too, so naming the precondition (which the
			// user can fix) beats naming the deferral (which they cannot).
			name:       "a set-wide precondition outranks the streaming deferral",
			instrument: false, useMappingBased: true, isMappingEnabled: true,
			deferredStreaming: true,
			wantSubstring:     "not instrument mode",
		},
		{
			// THE FIFTH REASON, and the one an ordinary recording hits. Every
			// precondition holds and the mapping is full of entries — they are
			// all session/connection-tier, so the per-test assertion has
			// nothing eligible to run over and reports NOT CHECKED. Without a
			// named reason the user sees `dependencies_checked: false` on every
			// test and has no way to tell it from a --base-path run.
			name:       "an armed test set whose every mapped dependency is reusable-tier",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			noEligibleDeps: true,
			wantSubstring:  "session/connection-tier",
		},
		{
			// The reason has to say what the REPORT will show, because the
			// honest report for this state is indistinguishable on the wire
			// from every other not-run mode.
			name:       "the no-eligible-dependency reason names the verdict the report carries",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			noEligibleDeps: true,
			wantSubstring:  "dependencies_checked=false",
		},
		{
			// PRECEDENCE, second axis. The tier classification comes from the
			// RECORDING, so it applies to the deferred streaming test cases
			// too; the deferral says nothing about the tests the main loop
			// already asserted.
			name:       "no eligible dependency outranks the streaming deferral",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			deferredStreaming: true, noEligibleDeps: true,
			wantSubstring: "session/connection-tier",
		},
		{
			// ...and a set-wide precondition still outranks BOTH: it is the
			// one the user can actually fix.
			name:       "a set-wide precondition outranks the no-eligible-dependency reason",
			instrument: true, useMappingBased: false, isMappingEnabled: true,
			noEligibleDeps: true,
			wantSubstring:  "--update-test-mapping",
		},
		{
			// THE FOURTH REASON. Every set-wide precondition holds and the
			// mapping has eligible entries — the run still could not ask the
			// question, because fetching the mocks the test consumed failed.
			// Before this arm the function returned "" for this state, so
			// --assert-dependencies reported dependencies_checked=false for
			// those tests and said NOTHING about why. That is the same silent
			// class as the other five.
			name:       "an armed test set whose per-test consumed-mock fetch failed",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true,
			wantSubstring:       "consumed-mock fetch failed",
		},
		{
			// THE MIS-ATTRIBUTION THIS ARM EXISTS TO STOP. Both states are
			// true at once: the fetch failed AND the mapping happens to be
			// all-reusable. Only the fetch is the operator's to fix, so the
			// fetch must win. With the arms in the other order the run tells
			// them to go re-tag a recording that was never the problem.
			name:       "a failed consumed fetch outranks the tier reason",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true, noEligibleDeps: true,
			wantSubstring: "consumed-mock fetch failed",
		},
		{
			// ...and it must NOT name the tier, which is the whole point of
			// the previous case. Asserted as an absence because
			// wantSubstring alone cannot see a message that says both.
			name:       "the failed-fetch reason does not blame the recording's tier",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true, noEligibleDeps: true,
			wantAbsent: "session/connection-tier",
		},
		{
			// PRECEDENCE, upper bound. A set-wide precondition is still more
			// informative than a per-test transport error: it disables the
			// assertion for every test in the set, including this one.
			name:       "a set-wide precondition outranks the failed consumed fetch",
			instrument: false, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true,
			wantSubstring:       "not instrument mode",
		},
		{
			// PRECEDENCE, lower bound: the fetch error is about the tests the
			// main loop just ran, so it beats the streaming deferral too.
			name:       "a failed consumed fetch outranks the streaming deferral",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true, deferredStreaming: true,
			wantSubstring: "consumed-mock fetch failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dependencyAssertionInertReason(tt.instrument, tt.useMappingBased, tt.isMappingEnabled, tt.consumedFetchFailed, tt.deferredStreaming, tt.noEligibleDeps)
			if tt.wantAbsent != "" {
				assert.NotContains(t, got, tt.wantAbsent,
					"the reason names a state that did not win precedence: %q", got)
			}
			if tt.wantSubstring == "" {
				if tt.wantAbsent != "" {
					return
				}
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tt.wantSubstring)
		})
	}
}

// THE BODY of warnDependencyAssertionInert, which the AST wiring test cannot
// reach: that one pins the CALL and its arguments, so every one of the three
// guards inside could be deleted with the whole suite still green.
//
// Each row below is the failure a deleted guard produces:
//   - "the knob was never asked for" dies when the AssertDependencies gate
//     goes, and every user on --base-path / --disable-mapping / a legacy test
//     set gets a brand-new warning per test set on upgrade.
//   - "called once per test, warns once per test set" dies when the *warned
//     latch goes, turning one line per test set into one per test.
//   - "a healthy test set says nothing" dies when the `reason == ""` early
//     return goes, warning on every well-formed suite.
func TestWarnDependencyAssertionInert(t *testing.T) {
	tests := []struct {
		name                string
		assertDeps          bool
		instrument          bool
		useMappingBased     bool
		isMappingEnabled    bool
		consumedFetchFailed bool
		deferredStreaming   bool
		noEligibleDeps      bool
		calls               int
		wantLogs            int
		wantMsg             string
		wantReason          string
	}{
		{
			name:       "the knob was never asked for: an inert test set is not the user's problem",
			assertDeps: false,
			instrument: false, useMappingBased: true, isMappingEnabled: true,
			calls: 1, wantLogs: 0,
		},
		{
			name:       "knob on, precondition missing: one warning naming it",
			assertDeps: true,
			instrument: false, useMappingBased: true, isMappingEnabled: true,
			calls: 1, wantLogs: 1,
			wantMsg:    "is inert for this test set",
			wantReason: "not instrument mode",
		},
		{
			name:       "called once per test, warns once per test set",
			assertDeps: true,
			instrument: true, useMappingBased: false, isMappingEnabled: true,
			calls: 2, wantLogs: 1,
			wantMsg:    "is inert for this test set",
			wantReason: "--update-test-mapping",
		},
		{
			name:       "a healthy test set says nothing",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			calls: 1, wantLogs: 0,
		},
		{
			// M-1: the streaming half. The set is healthy, so the only thing
			// that can produce this warning is the deferral itself.
			name:       "knob on and streaming tests deferred: the scope-accurate warning",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			deferredStreaming: true,
			calls:             1, wantLogs: 1,
			// NOT "no dependency can fail a test here": in a mixed test set
			// the assertion ran normally for every non-streaming test, and
			// the set-wide sentence would be a false alarm about those.
			wantMsg:    "does not run for this test set's streaming test cases",
			wantReason: "streaming (SSE/chunked) test cases",
		},
		{
			name:       "knob OFF and streaming tests deferred: silence",
			assertDeps: false,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			deferredStreaming: true,
			calls:             1, wantLogs: 0,
		},
		{
			// The Phase-2 latch is Phase-1's latch: a mixed test set that
			// already warned must not warn again when the streaming pass
			// starts.
			name:       "the streaming pass shares the test set's latch",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			deferredStreaming: true,
			calls:             3, wantLogs: 1,
			wantMsg: "does not run for this test set's streaming test cases",
		},
		{
			// THE NEW REASON, through the warner. The scope sentence is its
			// own: this is decided per test case from that test's mapped
			// entries, so the blanket "inert for this test set" would overclaim
			// on a mixed set — and the line must say NOT CHECKED, because the
			// whole point is that an empty dep_result here is not a clean
			// dependency verdict.
			name:       "knob on and nothing eligible: the scope-accurate NOT-CHECKED warning",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			noEligibleDeps: true,
			calls:          1, wantLogs: 1,
			wantMsg:    "no eligible dependency to assert",
			wantReason: "session/connection-tier",
		},
		{
			// Same latch discipline as every other reason: one line per test
			// set, however many of its test cases have nothing eligible.
			name:       "nothing eligible warns once per test set, not once per test",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			noEligibleDeps: true,
			calls:          4, wantLogs: 1,
			wantMsg: "no eligible dependency to assert",
		},
		{
			name:       "knob OFF and nothing eligible: silence",
			assertDeps: false,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			noEligibleDeps: true,
			calls:          1, wantLogs: 0,
		},
		{
			// ONLY WHEN IT APPLIES: a test whose mapping has an eligible
			// dependency must not drag the whole test set into a warning.
			name:       "an armed test set with an eligible dependency says nothing",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			noEligibleDeps: false,
			calls:          3, wantLogs: 0,
		},
		{
			// THE FETCH-FAILURE REASON, through the warner. Everything is
			// armed and the mapping HAS eligible entries, so no other arm can
			// fire — before this reason existed this row produced zero logs
			// while the report carried dependencies_checked=false.
			name:       "knob on and the consumed-mock fetch failed: the transport reason, not silence",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true,
			calls:               1, wantLogs: 1,
			wantMsg:    "could not read the consumed mocks",
			wantReason: "consumed-mock fetch failed",
		},
		{
			// Latch discipline, same as every other reason.
			name:       "a failed consumed fetch warns once per test set",
			assertDeps: true,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true,
			calls:               4, wantLogs: 1,
			wantMsg: "could not read the consumed mocks",
		},
		{
			name:       "knob OFF and the consumed fetch failed: silence",
			assertDeps: false,
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			consumedFetchFailed: true,
			calls:               1, wantLogs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			r := &Replayer{
				logger:     zap.New(core),
				instrument: tt.instrument,
				config:     &config.Config{Test: config.Test{AssertDependencies: tt.assertDeps}},
			}
			warned := false
			for range tt.calls {
				r.warnDependencyAssertionInert("test-set-0", tt.useMappingBased, tt.isMappingEnabled, tt.consumedFetchFailed, tt.deferredStreaming, tt.noEligibleDeps, &warned)
			}

			entries := logs.All()
			if len(entries) != tt.wantLogs {
				t.Fatalf("got %d warning(s) over %d call(s), want %d: %v", len(entries), tt.calls, tt.wantLogs, entries)
			}
			if tt.wantLogs == 0 {
				if warned {
					t.Errorf("the latch was set even though nothing was emitted")
				}
				return
			}
			if !strings.Contains(entries[0].Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", entries[0].Message, tt.wantMsg)
			}
			fields := entries[0].ContextMap()
			if fields["testset"] != "test-set-0" {
				t.Errorf("testset = %v, want test-set-0", fields["testset"])
			}
			reason, _ := fields["reason"].(string)
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tt.wantReason)
			}
			if reason == "" {
				t.Errorf("the warning names no reason, so it tells the user nothing to fix: %v", entries[0])
			}
		})
	}
}

// A nil latch is a programming error, not a reason to panic mid-run.
func TestWarnDependencyAssertionInert_NilLatchIsSafe(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Replayer{
		logger:     zap.New(core),
		instrument: false,
		config:     &config.Config{Test: config.Test{AssertDependencies: true}},
	}
	r.warnDependencyAssertionInert("test-set-0", true, true, false, false, false, nil)
	if logs.Len() != 0 {
		t.Fatalf("expected no output for a nil latch, got %v", logs.All())
	}
}

func TestVetoFlagName(t *testing.T) {
	tests := []struct {
		depAssertFail bool
		strictFailure bool
		nonDemotable  bool
		want          string
	}{
		{want: ""},
		{depAssertFail: true, want: "--assert-dependencies"},
		{strictFailure: true, want: "--strict-failure"},
		{depAssertFail: true, strictFailure: true, want: "--assert-dependencies, --strict-failure"},
		// A NON-FLAG REASON MUST NOT READ AS A FLAG. Whoever sees this line
		// must not go looking for an invocation to change.
		{nonDemotable: true, want: "this test case Kind is never demoted to obsolete"},
		{depAssertFail: true, nonDemotable: true, want: "--assert-dependencies, this test case Kind is never demoted to obsolete"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, vetoFlagName(tt.depAssertFail, tt.strictFailure, tt.nonDemotable))
	}
}

// A healthy test set where individual tests simply make no outgoing calls must
// NOT produce the inert warning: hasExpectedMocks is a per-TEST condition, and
// warning on it would fire on every well-formed suite and train users to
// ignore the level.
func TestDependencyAssertionInertReason_IsTestSetScoped(t *testing.T) {
	assert.Empty(t, dependencyAssertionInertReason(true, true, true, false, false, false),
		"a test case with no mapped dependencies is normal, not a misconfiguration")
}

// The per-test-set summary is the only WARN-level surface for the default
// (knob off) mode, so what it CLAIMS matters.
//
// It used to say "the response still matched, so they were not failed" over a
// population that includes OBSOLETE tests, whose response did not match — that
// is why they were demoted. And "never made" overstates the evidence: consumed
// mocks are drained when the response comes back, so a call the app makes
// after writing its response is attributed to the next test.
func TestWarnUnexercisedDependencies_ClaimsOnlyWhatTheDataSupports(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Replayer{logger: zap.New(core)}

	r.warnUnexercisedDependencies("test-set-0", map[string]models.TestStatus{
		"tc-passed":   models.TestStatusPassed,
		"tc-obsolete": models.TestStatusObsolete,
	})

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one summary WARN, got %d: %v", len(entries), entries)
	}
	msg := entries[0].Message
	for _, banned := range []string{"never made", "the response still matched, so"} {
		if strings.Contains(msg, banned) {
			t.Errorf("the summary claims %q, which the drained per-test window cannot support:\n%s", banned, msg)
		}
	}
	if !strings.Contains(msg, "--assert-dependencies") {
		t.Errorf("the summary must name the knob that turns this into a verdict:\n%s", msg)
	}

	fields := entries[0].ContextMap()
	if fields["tests"] != int64(2) {
		t.Errorf("tests = %v, want 2", fields["tests"])
	}
	// The PASSED subset is reported separately instead of the message
	// asserting it over everything.
	if fields["responseStillMatched"] != int64(1) {
		t.Errorf("responseStillMatched = %v, want 1 (tc-obsolete's response did NOT match)", fields["responseStillMatched"])
	}
}

// Nothing collected, nothing said.
func TestWarnUnexercisedDependencies_SilentWhenEmpty(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Replayer{logger: zap.New(core)}
	r.warnUnexercisedDependencies("test-set-0", nil)
	if logs.Len() != 0 {
		t.Fatalf("expected no output, got %v", logs.All())
	}
}

// THE SIZE ASSERTION, measured over the REAL writer output rather than a
// hand-built row set. Mirrors report.TestDependencyRowsDoNotBloatThe
// PersistedReport for the other axis: that one pins the consumed side, this
// one pins the missing side.
//
// Reports are written per test-set, re-read by `keploy report` and uploaded to
// the fleet report store. Uncapped, 100 tests x 200 mapped dependencies all
// unconsumed serialised to 5.4 MB against 115 KB for the same run before this
// slice — 47x, with the verdict knob OFF and no opt-in — for a shape that is
// not pathological but is the slice's own flagship scenario.
func TestBuildDepResults_AllMissingStaysBounded(t *testing.T) {
	const deps = 200
	expected := make([]models.MockEntry, 0, deps)
	for i := range deps {
		expected = append(expected, models.MockEntry{Name: fmt.Sprintf("mock-%03d", i), Kind: "Postgres"})
	}

	capped, err := yaml.Marshal(buildDepResults(expected, nil, true, nil, nil, nil).Rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// What the same input serialises to with no cap at all, computed here so
	// the assertion states a RATIO rather than a machine-dependent constant.
	uncapped := make([]models.DepResult, 0, deps)
	for i := range deps {
		uncapped = append(uncapped, models.DepResult{
			Name: models.DepRowName(i, models.DepTypePostgres, ""),
			Type: models.DepTypePostgres,
			Meta: missingMeta(),
		})
	}
	full, err := yaml.Marshal(uncapped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if len(capped) >= len(full)/3 {
		t.Fatalf("the all-missing shape serialised to %d bytes against %d uncapped; "+
			"the per-test cap (%d rows) is not bounding it",
			len(capped), len(full), depMissingRowCap)
	}
	t.Logf("all-missing per test: %d bytes capped vs %d bytes uncapped (%d dependencies)",
		len(capped), len(full), deps)
}

// THE TWO HALVES OF THE ELIGIBILITY QUESTION AGREE, FOR EVERY SHAPE.
//
// `dependencies_checked: false` is written by buildDepResults; the line telling
// the user WHY is chosen from noEligibleDeps, which RunTestSet computes as
// `len(expectedMocks) > 0 && len(filteredExpectedNames) == 0`. Two computations
// of one question. If they disagree the user gets one of two silent regressions:
// an unexplained not-checked report, or a warning about test sets whose
// assertion actually ran.
//
// TestEligibilityFilterHasExactlyOneDefinition pins that they share a function.
// This pins the property that sharing it BUYS — that the answers match — over
// the shapes that actually occur, so the invariant survives someone deciding to
// inline the filter again for performance.
//
// The direction that matters is the biconditional: `not checked` must mean
// `nothing eligible` and vice versa, whenever `valid` holds.
func TestNoEligibleDepsAndCheckedAreTheSameQuestion(t *testing.T) {
	httpEntry := models.MockEntry{Name: "mock-2", Kind: "Http"}
	pgEntry := models.MockEntry{Name: "mock-9", Kind: "Postgres"}
	dnsEntry := models.MockEntry{Name: "mock-dns", Kind: "DNS"}

	tests := []struct {
		name     string
		expected []models.MockEntry
		consumed []models.MockState
		reusable map[string]bool
		kinds    map[string]models.Kind
	}{
		{
			name:     "the ordinary recording: every entry session-tier",
			expected: []models.MockEntry{httpEntry, pgEntry},
			reusable: map[string]bool{"mock-2": true, "mock-9": true},
		},
		{
			name:     "one eligible entry, unconsumed",
			expected: []models.MockEntry{httpEntry, pgEntry},
			reusable: map[string]bool{"mock-2": true},
		},
		{
			name:     "one eligible entry, consumed",
			expected: []models.MockEntry{httpEntry, pgEntry},
			consumed: []models.MockState{consumed("mock-9", models.Kind("Postgres"))},
			reusable: map[string]bool{"mock-2": true},
		},
		{
			name:     "every entry eligible",
			expected: []models.MockEntry{httpEntry, pgEntry},
		},
		{
			name:     "only DNS is mapped",
			expected: []models.MockEntry{dnsEntry},
		},
		{
			name:     "DNS and a reusable entry together",
			expected: []models.MockEntry{dnsEntry, httpEntry},
			reusable: map[string]bool{"mock-2": true},
		},
		{
			name:     "DNS resolved through the kind lookup rather than the entry",
			expected: []models.MockEntry{{Name: "mock-dns2"}},
			kinds:    map[string]models.Kind{"mock-dns2": models.DNS},
		},
		{
			name:     "the recording maps nothing at all",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exactly how RunTestSet derives its two values.
			eligible := eligibleExpectedEntries(tt.expected, tt.kinds, tt.reusable)
			filteredExpectedNames := make([]string, 0, len(eligible))
			for _, m := range eligible {
				filteredExpectedNames = append(filteredExpectedNames, m.Name)
			}
			noEligibleDeps := len(tt.expected) > 0 && len(filteredExpectedNames) == 0

			dep := buildDepResults(tt.expected, tt.consumed, true, tt.reusable, tt.kinds, nil)

			// The biconditional, stated over the state a user can observe.
			// `valid` is true throughout, so the ONLY thing that can make the
			// writer report not-checked is the eligibility filter — which is
			// the same filter the warning is keyed off.
			if noEligibleDeps && dep.Checked {
				t.Fatalf("the warner says nothing was eligible but the writer persisted deps_checked=true "+
					"(consumed=%d, rows=%v). The report then claims the assertion ran and found nothing "+
					"missing, while the run separately warns that it had nothing to assert.",
					dep.Consumed, dep.Rows)
			}
			if len(tt.expected) > 0 && !noEligibleDeps && !dep.Checked {
				t.Fatalf("the writer reports NOT CHECKED for a test with %d eligible dependency/ies, but "+
					"noEligibleDeps is false so no warning explains it. The user gets "+
					"`dependencies_checked: false` with no reason given.", len(filteredExpectedNames))
			}

			// ...and the scalars stay consistent with the bit either way: a
			// not-checked verdict must carry no rows and no count, or a
			// consumer reading dep_result/deps_consumed without first reading
			// the bit sees data that the bit says does not exist.
			if !dep.Checked && (dep.Consumed != 0 || len(dep.Rows) != 0) {
				t.Fatalf("a NOT-CHECKED verdict carries data: consumed=%d rows=%v", dep.Consumed, dep.Rows)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rule 3: a missing effect must never be demotable.
// ---------------------------------------------------------------------------

// TWO PREDICATES, TWO DIFFERENT QUESTIONS, AND THEY MUST NOT SHARE A BOOLEAN.
//
//	neverDemotableKind      — may a test the judge ALREADY FAILED be graded
//	                          OBSOLETE? Keyed on Kind alone; for CONSUMER the
//	                          answer is never.
//	missingEffectMockPromotes — must a test the judge PASSED be promoted to
//	                          FAILED? Only when an unconsumed mock could be
//	                          carrying an effect claim.
//
// Every table above takes those as literals from its own row, so they pin the
// algebra GIVEN the value and never the value itself — `return false` here
// compiles, keeps every identifier the AST wiring test looks for, and silently
// restores the demotion for consumer tests: the worker stopped producing, the
// test set is not marked failed, and the run exits 0 reporting verified_green.
func TestNeverDemotableKind(t *testing.T) {
	assert.True(t, neverDemotableKind(models.CONSUMER),
		"a consumer test the judge failed must never be graded OBSOLETE, whatever mock went unconsumed")
	for _, k := range []models.Kind{models.HTTP, models.HTTP2, models.GRPC_EXPORT, models.Kind(""), models.Kind("SomeKindThatDoesNotExistYet")} {
		assert.False(t, neverDemotableKind(k), "kind %q must keep the historical demotion", k)
	}
}

// The promotion arm's predicate. The Kind is necessary and NOT sufficient:
// mockSetMismatch fires for any unconsumed per-test mock, and a consumer test
// legitimately maps per-test coordination mocks (design §4 P4 keeps
// OffsetFetch per-test) that a client with a cached position simply does not
// call. Promoting on those would fail a clean consumer test and hand the
// reader a message about the worker's production that has nothing to do with
// what happened.
//
// IT FAILS CLOSED ON AN UNCLASSIFIABLE MOCK. The lookup is best-effort — a nil
// mockDB or a registry read error leaves it empty — and excusing a miss would
// report the flagship "the worker stopped writing" regression as PASSED
// because a side lookup did not load. The last three rows pin that direction.
func TestMissingEffectMockPromotes(t *testing.T) {
	// A Kafka-shaped mapping: the trigger, one produced effect, one
	// coordination call of the same family, one mapped database write.
	lookup := map[string]mockDisplayInfo{
		"mock-trigger": {kind: models.KAFKA, role: models.RoleTrigger},
		"mock-effect":  {kind: models.KAFKA, role: models.RoleEffect},
		"mock-coord":   {kind: models.KAFKA},
		"mock-write":   {kind: models.Postgres},
	}
	all := []string{"mock-trigger", "mock-effect", "mock-coord", "mock-write"}

	tests := []struct {
		name     string
		kind     models.Kind
		expected []string
		consumed []string
		lookup   map[string]mockDisplayInfo
		want     bool
	}{
		{
			name: "an unconsumed effect mock promotes", kind: models.CONSUMER,
			expected: all, consumed: []string{"mock-trigger", "mock-coord", "mock-write"}, want: true,
		},
		{
			name: "an unconsumed mapped write promotes too — that is spec.writes", kind: models.CONSUMER,
			expected: all, consumed: []string{"mock-trigger", "mock-effect", "mock-coord"}, want: true,
		},
		{
			// THE FALSE-RED ROW. A per-test coordination call the client
			// skipped is exactly what the OBSOLETE demotion exists for.
			name: "an unconsumed coordination mock does NOT promote", kind: models.CONSUMER,
			expected: all, consumed: []string{"mock-trigger", "mock-effect", "mock-write"}, want: false,
		},
		{
			// An undelivered trigger is keploy failing, not the worker. The
			// gate names that by itself.
			name: "an unconsumed trigger does NOT promote", kind: models.CONSUMER,
			expected: all, consumed: []string{"mock-effect", "mock-coord", "mock-write"}, want: false,
		},
		{
			name: "nothing unconsumed, nothing to promote", kind: models.CONSUMER,
			expected: all, consumed: all, want: false,
		},
		{
			// FAIL CLOSED. The registry did not load, so nothing can be
			// POSITIVELY identified as same-family coordination traffic. The
			// unconsumed write mock is still a write mock.
			name:     "an EMPTY lookup promotes rather than excusing",
			kind:     models.CONSUMER,
			expected: []string{"mock-trigger", "mock-write"}, consumed: []string{"mock-trigger"},
			lookup: map[string]mockDisplayInfo{},
			want:   true,
		},
		{
			// Same direction, one step less degraded: the roles are known but
			// the Kinds are not, so "same family as the trigger" cannot be
			// established and the mock is treated as a possible effect.
			name:     "an entry with no Kind promotes rather than excusing",
			kind:     models.CONSUMER,
			expected: []string{"mock-trigger", "mock-coord"}, consumed: []string{"mock-trigger"},
			lookup: map[string]mockDisplayInfo{"mock-trigger": {role: models.RoleTrigger}, "mock-coord": {}},
			want:   true,
		},
		{
			name:     "a recording with no role metadata at all still promotes: nothing identifies a trigger to compare against",
			kind:     models.CONSUMER,
			expected: []string{"mock-a", "mock-b"}, consumed: nil, want: true,
		},
		{name: "HTTP", kind: models.HTTP, expected: all, consumed: nil, want: false},
		{name: "HTTP2", kind: models.HTTP2, expected: all, consumed: nil, want: false},
		{name: "gRPC", kind: models.GRPC_EXPORT, expected: all, consumed: nil, want: false},
		{name: "empty kind", kind: models.Kind(""), expected: all, consumed: nil, want: false},
		{name: "unknown kind", kind: models.Kind("SomeKindThatDoesNotExistYet"), expected: all, consumed: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := lookup
			if tt.lookup != nil {
				l = tt.lookup
			}
			assert.Equal(t, tt.want, missingEffectMockPromotes(tt.kind, tt.expected, tt.consumed, l),
				"the OBSOLETE demotion exists for a mock pool that drifted away from the recording; "+
					"for a consumer test's EFFECT mocks the reading is inverted, because those mocks ARE its assertions")
		})
	}
}

// The veto is UNCONDITIONAL. There is no configuration in which grading a
// consumer's unconsumed effect mock as OBSOLETE is correct, so no combination
// of the other four inputs may re-enable the demotion.
func TestDemoteToObsoleteIsAlwaysVetoedByNeverDemotable(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		for _, strictMock := range []bool{false, true} {
			for _, depAssert := range []bool{false, true} {
				for _, strictFail := range []bool{false, true} {
					assert.False(t,
						demoteToObsolete(mismatch, strictMock, depAssert, strictFail, true),
						"mismatch=%v strictMockReject=%v depAssertFail=%v strictFailure=%v",
						mismatch, strictMock, depAssert, strictFail)
				}
			}
		}
	}
}

// The end-to-end pin design §9 asks for, computed FROM THE KIND rather than
// from a literal: a Consumer test whose effect mock went unconsumed must be
// FAILED, must fail the test set (which is what makes the exit code non-zero
// and `keploy status` red), and must NOT be OBSOLETE or IGNORED.
//
// The same inputs with an HTTP Kind must keep the historical answer exactly,
// which is what makes this a safe change for every suite that passes today.
func TestAnUnconsumedEffectMockFailsTheSetForAConsumerAndNotForHTTP(t *testing.T) {
	// "the response compared clean, and an expected mock was never consumed"
	// with every knob at its default.
	const responseMatched, mockSetMismatch = true, true

	// The unconsumed mock is an EFFECT mock, which is what the rule is about:
	// the worker did not produce the message the recording says it produces.
	lookup := map[string]mockDisplayInfo{
		"mock-trigger": {kind: models.KAFKA, role: models.RoleTrigger},
		"mock-effect":  {kind: models.KAFKA, role: models.RoleEffect},
	}
	expected := []string{"mock-trigger", "mock-effect"}
	consumed := []string{"mock-trigger"}
	neverDemotableConsumer := neverDemotableKind(models.CONSUMER)
	promoteConsumer := missingEffectMockPromotes(models.CONSUMER, expected, consumed, lookup)
	neverDemotableHTTP := neverDemotableKind(models.HTTP)
	promoteHTTP := missingEffectMockPromotes(models.HTTP, expected, consumed, lookup)

	consumerOutcome := resolveTestOutcome(responseMatched, mockSetMismatch, false, false, false, neverDemotableConsumer, promoteConsumer)
	assert.Equal(t, models.TestStatusFailed, consumerOutcome.Status,
		"the flagship regression must not be OBSOLETE")
	assert.True(t, consumerOutcome.FailsTestSet,
		"OBSOLETE does not fail the test set, and that asymmetry IS the silent-green hole")
	assert.Equal(t, mismatchLogNonDemotableReject, consumerOutcome.Log)
	assert.False(t, consumerOutcome.DepAssertFail,
		"this verdict owes nothing to --assert-dependencies")

	httpOutcome := resolveTestOutcome(responseMatched, mockSetMismatch, false, false, false, neverDemotableHTTP, promoteHTTP)
	assert.Equal(t, models.TestStatusPassed, httpOutcome.Status,
		"an HTTP suite that passes today must keep passing")
	assert.False(t, httpOutcome.FailsTestSet)
	assert.Equal(t, mismatchLogIgnoredResponseMatched, httpOutcome.Log)

	// And the same for the response-failed half.
	consumerFailed := resolveTestOutcome(false, mockSetMismatch, false, false, false, neverDemotableConsumer, promoteConsumer)
	assert.Equal(t, models.TestStatusFailed, consumerFailed.Status)
	assert.True(t, consumerFailed.FailsTestSet)
	assert.Equal(t, mismatchLogVetoedFailure, consumerFailed.Log)
	assert.NotContains(t, consumerFailed.VetoFlags, "--",
		"there is no flag to name here; pointing the reader at an invocation sends them to the wrong place")

	httpFailed := resolveTestOutcome(false, mockSetMismatch, false, false, false, neverDemotableHTTP, promoteHTTP)
	assert.Equal(t, models.TestStatusObsolete, httpFailed.Status,
		"the historical demotion is untouched for every other Kind")
	assert.False(t, httpFailed.FailsTestSet)
}

// THE COMPOSITION THAT WAS A SILENT GREEN, spelled out end to end from the two
// predicates the call site computes.
//
// The judge FAILED this consumer test — an effect diff, a completion timeout,
// any named refusal — and the mock set diverged on a mock that is NOT an
// effect mock: design §4 P4's own example, a cached OffsetFetch the client
// skipped. While one boolean carried both claims the test was persisted
// OBSOLETE, did not fail the test set, and the run exited 0, with the failure
// logs suppressed on top of it so there was nothing in the report to notice.
func TestAJudgeFailedConsumerTestIsNeverObsoleteWhateverWentUnconsumed(t *testing.T) {
	lookup := map[string]mockDisplayInfo{
		"trigger-1":     {kind: models.KAFKA, role: models.RoleTrigger},
		"effect-1":      {kind: models.KAFKA, role: models.RoleEffect},
		"offsetfetch-1": {kind: models.KAFKA},
	}
	expected := []string{"trigger-1", "effect-1", "offsetfetch-1"}
	consumed := []string{"trigger-1", "effect-1"}

	neverDemotable := neverDemotableKind(models.CONSUMER)
	effectMockMissing := missingEffectMockPromotes(models.CONSUMER, expected, consumed, lookup)
	if effectMockMissing {
		t.Fatal("guard: this case is only interesting while the narrow predicate is false")
	}

	// responseMatched=false: the JUDGE said this test failed.
	out := resolveTestOutcome(false, true, false, false, false, neverDemotable, effectMockMissing)
	if out.Status != models.TestStatusFailed {
		t.Fatalf("status = %q, want FAILED: a consumer test the judge failed may never be graded obsolete", out.Status)
	}
	if !out.FailsTestSet {
		t.Fatal("the test set must go red; OBSOLETE not failing the set IS the silent-green hole")
	}
	if out.Log != mismatchLogVetoedFailure {
		t.Fatalf("log = %v, want mismatchLogVetoedFailure", out.Log)
	}
	if !shouldEmitFailureLogs(true, false, neverDemotable) {
		t.Fatal("and its explanation must not be suppressed: a red test with no categories, summary or findings is unusable")
	}

	// The same shape on an HTTP test keeps the historical demotion exactly.
	httpOut := resolveTestOutcome(false, true, false, false, false,
		neverDemotableKind(models.HTTP), missingEffectMockPromotes(models.HTTP, expected, consumed, lookup))
	if httpOut.Status != models.TestStatusObsolete || httpOut.FailsTestSet {
		t.Fatalf("HTTP must be unchanged, got status=%q failsSet=%v", httpOut.Status, httpOut.FailsTestSet)
	}
}

// The diff must reach the user for the same case. shouldEmitFailureLogs is
// what decides that, and for a consumer the mock-set divergence IS the
// finding, so the historical suppression would hand back a red test with
// nothing to look at.
func TestFailureLogsAreNeverSuppressedForANonDemotableKind(t *testing.T) {
	assert.True(t, shouldEmitFailureLogs(true, false, neverDemotableKind(models.CONSUMER)))
	assert.False(t, shouldEmitFailureLogs(true, false, neverDemotableKind(models.HTTP)),
		"the historical suppression is untouched for every other Kind")

	// THE ROW THE BY-KIND BIT EXISTS FOR. The mock sets diverge only on
	// COORDINATION traffic — no effect mock is missing — so the narrow
	// promotion predicate is false. If shouldEmitFailureLogs were fed that
	// narrow bit, a consumer test the judge FAILED would be reported with no
	// categories, no summary and no findings list: a red test with nothing to
	// look at, on exactly the Kind whose rows ARE the finding.
	coordOnly := map[string]mockDisplayInfo{
		"mock-trigger": {kind: models.KAFKA, role: models.RoleTrigger},
		"mock-coord":   {kind: models.KAFKA},
	}
	coordExpected := []string{"mock-trigger", "mock-coord"}
	coordConsumed := []string{"mock-trigger"}
	assert.False(t, missingEffectMockPromotes(models.CONSUMER, coordExpected, coordConsumed, coordOnly),
		"guard: this row is only meaningful while the narrow predicate is false")
	assert.True(t, shouldEmitFailureLogs(true, false, neverDemotableKind(models.CONSUMER)),
		"a consumer failure is always explained, whatever mock happened to go unconsumed")
}

// ---------------------------------------------------------------------------
// existingEffectRows: the guard that stops the sync writer deleting the
// judge's verdict.
// ---------------------------------------------------------------------------

func TestExistingEffectRows(t *testing.T) {
	effect := models.DepResult{Name: "effects[0] kafka produce order-events key=o-1", Type: "kafka"}
	effectUnexpected := models.DepResult{Name: "effects[*] kafka produce other", Type: "kafka"}
	dep := models.DepResult{Name: "deps[0] postgres db:5432 (presence)", Type: "postgres"}

	tests := []struct {
		name string
		in   []models.DepResult
		want []models.DepResult
	}{
		{name: "nil in, nil out", in: nil, want: nil},
		{
			// The --retry-passing second cycle: attachDepResults runs again
			// against a freshly built result, and a stale deps row it did not
			// write must not accumulate.
			name: "a sync-path row is dropped, not kept",
			in:   []models.DepResult{dep},
			want: nil,
		},
		{
			name: "the judge's rows survive, in order",
			in:   []models.DepResult{effect, dep, effectUnexpected},
			want: []models.DepResult{effect, effectUnexpected},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, existingEffectRows(tt.in))
		})
	}
}

// attachDepResults APPENDS. Assigning would silently delete the judge's rows —
// the entire verdict of a consumer test — while leaving every call, argument
// and enclosing condition in RunTestSet untouched, which is precisely the
// mutation an inspection-only review cannot see.
func TestAttachDepResultsKeepsTheJudgesRowsAndDropsStaleSyncRows(t *testing.T) {
	effect := models.DepResult{
		Name: "effects[0] kafka produce order-events key=o-1", Type: "kafka",
		Meta: []models.DepMetaResult{{Normal: false, Key: "effects.0.body.status", Expected: "CONFIRMED", Actual: "PENDING"}},
	}
	stale := models.DepResult{Name: "deps[0] postgres db:5432 (presence)", Type: "postgres", Meta: missingMeta()}
	fresh := models.DepResult{Name: "deps[1] postgres db:5432 (presence)", Type: "postgres", Meta: missingMeta()}

	t.Run("consumer: effects survive, stale deps do not, order is effects then deps", func(t *testing.T) {
		tcResult := &models.TestResult{Kind: models.CONSUMER, Status: models.TestStatusFailed}
		tcResult.Result.DepResult = []models.DepResult{effect, stale}

		attachDepResults(tcResult, models.TestStatusFailed, depAssertion{
			Checked: true, Consumed: 2, Rows: []models.DepResult{fresh},
		}, false)

		assert.Equal(t, []models.DepResult{effect, fresh}, tcResult.Result.DepResult)
	})

	t.Run("non-consumer: the output is exactly dep.Rows, nil-vs-empty included", func(t *testing.T) {
		// BACKWARD COMPATIBILITY. `append(nil, ...)` over an empty row slice
		// must still yield nil, or `dep_result:` stops serializing identically
		// to a pre-change report.
		tcResult := &models.TestResult{Kind: models.HTTP, Status: models.TestStatusPassed}
		attachDepResults(tcResult, models.TestStatusPassed, depAssertion{Checked: true, Consumed: 5}, false)
		assert.Nil(t, tcResult.Result.DepResult)

		tcResult2 := &models.TestResult{Kind: models.HTTP, Status: models.TestStatusPassed}
		attachDepResults(tcResult2, models.TestStatusPassed, depAssertion{
			Checked: true, Consumed: 5, Rows: []models.DepResult{fresh},
		}, false)
		assert.Equal(t, []models.DepResult{fresh}, tcResult2.Result.DepResult)
	})
}
