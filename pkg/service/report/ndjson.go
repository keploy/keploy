package report

import (
	"fmt"
	"io"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// EffectsSchemaVersion is the version of the `keploy report --format json`
// projection defined below. It is part of the agent contract: bump it only for
// a BREAKING change (a field removed, renamed, or given a different meaning).
// Adding a new optional field is not breaking and must not bump it.
const EffectsSchemaVersion = "1"

// `keploy report --format json` emits NDJSON: one self-contained JSON object
// per test result, newline-delimited, test-sets in sorted order and tests in
// report order so two runs over the same data produce byte-identical output.
//
// Schema (schema_version "1"):
//
//	{
//	  "schema_version": "1",
//	  "test_run_id":    "test-run-3",
//	  "test_set_id":    "test-set-0",
//	  "test_case_id":   "test-1",
//	  "kind":           "Http",              // models.Kind
//	  "status":         "FAILED",            // models.TestStatus
//	  "failure_categories": ["DEPENDENCY_MISSING"],   // omitted when empty
//	  "dependencies_checked":  true,
//	  "dependencies_consumed": 7,
//	  "effects": [
//	    {
//	      "name":     "deps[0] postgres PostgreSQL INSERT (presence)",
//	      "type":     "postgres",
//	      "key":      "presence",
//	      "expected": "consumed",
//	      "actual":   "not consumed",
//	      "matched":  false
//	    }
//	  ]
//	}
//
// One `effects` entry per models.DepMetaResult, flattened with its parent row's
// name and type so a consumer never has to walk a nested structure. `effects`
// is always present, and every entry in it is a FAILED assertion: the
// dependencies a test DID exercise are the `dependencies_consumed` count, not
// entries.
//
// READ `dependencies_checked` FIRST. It is false when the per-test dependency
// assertion did not RUN for this test — a --base-path / remote-agent run,
// --disable-mapping, a test set with no usable mappings.yaml, a failed
// per-test consumed-mock fetch, the deferred streaming path, a test the
// mapping records no dependency for, or a test whose every mapped dependency
// is INELIGIBLE. `effects` is then `[]`, which is NOT the same statement as
// "this test lost nothing": the question was never asked. A consumer that
// skips this check reports "no dependency regressions" for a run in which
// nothing was checked, which is the false-green this whole projection exists
// to close.
//
// ELIGIBILITY IS THE ONE THAT SURPRISES PEOPLE, so it is stated plainly here
// rather than left to be discovered: the assertion covers only mocks recorded
// as PER-TEST tier. Session/connection-tier mocks are excluded (recorded once
// at app boot and shared across every test, so a per-test presence assertion
// over them goes missing at random and reds healthy tests) and so is DNS.
// models.Mock.DeriveLifetime classifies an UNTAGGED HTTP / HTTP2 / Postgres /
// MySQL / Generic mock as session-tier, so for a recording whose mocks carry no
// per-test tier tag NOTHING is eligible and every verdict in this document
// carries `dependencies_checked: false`. That is the honest answer, not a bug
// in the run — but it does mean an ordinary recorded outgoing HTTP call is NOT
// automatically covered by this assertion. The replayer logs one warning per
// test set naming that reason when --assert-dependencies is on.
//
//	lost_a_dependency := v.dependencies_checked && any(e.matched == false)
//	unknown           := !v.dependencies_checked
//
// `dependencies_consumed` is how many recorded dependencies WERE exercised.
// Meaningful only when `dependencies_checked` is true, where 0 is a real
// answer ("checked, exercised nothing") and not a missing one. It is a count,
// not a list, because one persisted row per consumed dependency costs ~190
// bytes of YAML in a report written per test-set and uploaded to the fleet
// report store — 3-11 MB per report on a Postgres-chatty suite, of
// `consumed/consumed` boilerplate no consumer reads. The identities are
// carried by the report's own MatchedCalls.
//
// WHAT `effects` CONTAINS:
//
//   - `"key": "presence"`, `"matched": false` — one entry per dependency the
//     recording says the test exercised that was NOT observed during the
//     test's window. These are the failed assertions and they are ALWAYS
//     individually present. `type` is a stable protocol FAMILY
//     (models.DepTypeForKind), never a parser version: a Postgres dependency
//     is `"postgres"` whether the mock was recorded by the v1, v2 or v3
//     parser. `name` is stable across runs — its index numbers the RECORDED
//     dependency list, not the emitted rows.
//   - `"key": "missing_count"`, `"matched": false` — at most ONE overflow
//     entry, present only when a test lost more dependencies than the
//     per-test cap (replay.depMissingRowCap, 50). `expected` is "0" and
//     `actual` is how many further dependencies went unobserved beyond the
//     ones named individually above. It exists because the flagship scenario
//     — a downstream service removed, a worker that stopped producing, a mock
//     pool whose names drifted wholesale — makes EVERY mapped dependency of
//     EVERY test missing at once, which uncapped serialised to a 5.4 MB
//     test-set report against 115 KB before this slice. It carries
//     `matched: false` so the documented "did this test lose a dependency"
//     rule below still fires; what it does not carry is the identity of those
//     dependencies, which stays in the test set's mappings.yaml.
//
// A consumer that keys on `matched == false` alone needs no special handling
// for the overflow entry. One that assumes every failed entry has
// `key == "presence"` does: check the key, or count only presence entries when
// you want individually named dependencies. The overflow entry uses the
// `deps[*]` index token in its `name` and is identified by `key`, never by
// parsing the name.
//
// WHY THE CONTAINER IS `effects` WHILE ITS ROW NAMES SAY `deps[i]`, decided
// before schema_version "1" ships and the key becomes unbumpable. `effects` is
// the UNION container for both producers of dependency-shaped assertions: this
// slice's sync-path presence rows (`deps[i]`, presence-only, covering outgoing
// reads as well as writes) and slice 5's consumer projector rows
// (`effects[i]`, carrying field-level diffs decoded from a protocol payload —
// keploy-consumer-design-v2.md §2). Renaming the container to `dependencies`
// would be wrong the moment slice 5 lands, because a consumer effect is not a
// dependency; keeping the two ROW-NAME prefixes disjoint is what lets a
// consumer tell the producers apart without a schema-version bump. The row
// prefixes are a documented one-way door (models/depresult.go); so is this key.
//
// `keploy test --assert-dependencies` changes only the `status` a lost
// dependency produces (PASSED/OBSOLETE -> FAILED). It does not change the
// shape of this document, so a consumer's parsing is mode-independent.
//
// NOTE ON WHAT `matched: false` CLAIMS. Consumed mocks are drained when the
// response comes back, so a call the app makes AFTER writing its response (an
// audit write, an analytics POST, a cache set) is attributed to the next test.
// The claim is "not observed during this test's window", not "never made".
//
// This is a READ-ONLY projection over the persisted report data
// (models.TestReport). It must never reach into replay internals — that
// decoupling is what lets an agent loop parse a verdict without Keploy's
// replay loop being in the picture at all
// (keploy-consumer-design-v2.md §7 slice 4).

// effectRow is one flattened dependency assertion.
type effectRow struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Key      string `json:"key"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Matched  bool   `json:"matched"`
}

// testVerdict is one NDJSON line.
type testVerdict struct {
	SchemaVersion     string   `json:"schema_version"`
	TestRunID         string   `json:"test_run_id"`
	TestSetID         string   `json:"test_set_id"`
	TestCaseID        string   `json:"test_case_id"`
	Kind              string   `json:"kind"`
	Status            string   `json:"status"`
	FailureCategories []string `json:"failure_categories,omitempty"`
	// DependenciesChecked says whether the per-test dependency assertion RAN
	// for this test. Without it `effects: []` is ambiguous — see the contract
	// note above.
	DependenciesChecked bool `json:"dependencies_checked"`
	// DependenciesConsumed is how many recorded dependencies this test DID
	// exercise. Always emitted (no omitempty): 0 is a real answer for a
	// checked test, and a consumer must not have to distinguish "absent" from
	// "zero" on a field whose whole job is to be a magnitude.
	DependenciesConsumed int         `json:"dependencies_consumed"`
	Effects              []effectRow `json:"effects"`
}

// buildEffectRows flattens a test's dependency rows into the wire shape. Pure
// so it can be table-tested without a Report, a config or a filesystem.
// Always returns a non-nil slice so `effects` marshals as [] rather than null.
func buildEffectRows(t models.TestResult) []effectRow {
	rows := make([]effectRow, 0, len(t.Result.DepResult))
	for _, d := range t.Result.DepResult {
		for _, m := range d.Meta {
			key := m.Key
			if key == "" {
				key = models.DepKeyPresence
			}
			rows = append(rows, effectRow{
				Name:     d.Name,
				Type:     d.Type,
				Key:      key,
				Expected: m.Expected,
				Actual:   m.Actual,
				Matched:  m.Normal,
			})
		}
	}
	return rows
}

// buildTestVerdict projects one persisted test result onto the NDJSON schema.
func buildTestVerdict(runID, testSetID string, t models.TestResult) testVerdict {
	var categories []string
	for _, c := range t.FailureInfo.Category {
		categories = append(categories, string(c))
	}
	return testVerdict{
		SchemaVersion:        EffectsSchemaVersion,
		TestRunID:            runID,
		TestSetID:            testSetID,
		TestCaseID:           t.TestCaseID,
		Kind:                 string(t.Kind),
		Status:               string(t.Status),
		FailureCategories:    categories,
		DependenciesChecked:  t.Result.DependenciesChecked(),
		DependenciesConsumed: t.Result.DepsConsumed,
		Effects:              buildEffectRows(t),
	}
}

// buildTestVerdicts projects a whole run. Test-set names are sorted so the
// output is deterministic across runs; within a set, report order is kept.
// testSetID falls back to the map key when the persisted report has no
// TestSet field (older reports).
func buildTestVerdicts(runID string, reports map[string]*models.TestReport) []testVerdict {
	names := make([]string, 0, len(reports))
	for name := range reports {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []testVerdict
	for _, name := range names {
		rep := reports[name]
		if rep == nil {
			continue
		}
		testSetID := rep.TestSet
		if testSetID == "" {
			testSetID = name
		}
		for _, t := range rep.Tests {
			out = append(out, buildTestVerdict(runID, testSetID, t))
		}
	}
	return out
}

// writeNDJSON emits one JSON object per line.
func writeNDJSON(w io.Writer, verdicts []testVerdict) error {
	jw := utils.NewJSONWriterOut(w, true)
	for i := range verdicts {
		if err := jw.Write(verdicts[i]); err != nil {
			return fmt.Errorf("failed to write ndjson verdict for %s/%s: %w",
				verdicts[i].TestSetID, verdicts[i].TestCaseID, err)
		}
	}
	return nil
}

// generateNDJSON writes the `--format json` projection through the report's
// own buffered writer.
func (r *Report) generateNDJSON(runID string, reports map[string]*models.TestReport) error {
	if err := writeNDJSON(r.out, buildTestVerdicts(runID, reports)); err != nil {
		return err
	}
	return r.out.Flush()
}
