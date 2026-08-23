package report

import (
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The four scalars of a consumer delivery window were WRITE-ONLY: replay set
// them and nothing anywhere read them back, so `end_reason` — the field an
// agent keys remedies on, and the only thing that separates "the worker
// produced the wrong thing" from "we stopped looking too early" — reached
// neither the CLI, JUnit nor --format json.

func consumerTestResult() models.TestResult {
	return models.TestResult{
		Kind:       models.CONSUMER,
		TestCaseID: "test-7",
		Status:     models.TestStatusFailed,
		Consumer: &models.ConsumerResultInfo{
			TriggerAccepted: true,
			ExpectedEffects: 1,
			ObservedEffects: 0,
			EndReason:       models.ConsumerEndReasonTimeout,
		},
		Result: models.Result{
			DepsChecked: true,
			DepResult: []models.DepResult{{
				Name: "effects[0] kafka produce order-events key=o-4c1",
				Type: "kafka",
				Meta: []models.DepMetaResult{{
					Normal: false, Key: "effects.0.presence",
					Expected: models.EffectPresenceObserved, Actual: models.EffectPresenceMissing,
				}},
			}},
		},
	}
}

func TestRenderDepResultsShowsTheConsumerWindow(t *testing.T) {
	var sb strings.Builder
	renderDepResults(&sb, consumerTestResult())
	out := sb.String()

	if !strings.Contains(out, "ended timeout") {
		t.Fatalf("the end reason must reach the CLI; got:\n%s", out)
	}
	if !strings.Contains(out, "0 of 1 effects observed") {
		t.Fatalf("the counts must reach the CLI; got:\n%s", out)
	}
	if !strings.Contains(out, "effects[0] kafka produce order-events key=o-4c1") {
		t.Fatalf("the effect row must still render; got:\n%s", out)
	}
}

// A window that closed for the WRONG reason is worth printing even when every
// effect matched, because "not fully observed" is not the same as "satisfied".
func TestRenderDepResultsShowsTheWindowEvenWithNoRows(t *testing.T) {
	tr := consumerTestResult()
	tr.Result.DepResult = nil
	var sb strings.Builder
	renderDepResults(&sb, tr)
	if !strings.Contains(sb.String(), "ended timeout") {
		t.Fatalf("got:\n%s", sb.String())
	}
}

// BACKWARD COMPATIBILITY. An HTTP result carries a nil Consumer, and its
// rendered block must be byte-identical to a build without this field.
func TestRenderDepResultsIsUnchangedForANonConsumerTest(t *testing.T) {
	tr := consumerTestResult()
	tr.Kind = models.HTTP
	tr.Consumer = nil

	var sb strings.Builder
	renderDepResults(&sb, tr)
	if strings.Contains(sb.String(), "window:") {
		t.Fatalf("a non-consumer test must render no window line; got:\n%s", sb.String())
	}
	if !strings.HasPrefix(sb.String(), models.FormatDepResults(tr.Result.DepResult)) {
		t.Fatalf("the dependency block changed shape; got:\n%s", sb.String())
	}
}

func TestNDJSONCarriesTheConsumerWindow(t *testing.T) {
	v := buildTestVerdict("run-1", "test-set-0", consumerTestResult())
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c, ok := decoded["consumer"].(map[string]any)
	if !ok {
		t.Fatalf("the NDJSON line carries no consumer object:\n%s", raw)
	}
	if c["end_reason"] != string(models.ConsumerEndReasonTimeout) {
		t.Fatalf("end_reason = %v", c["end_reason"])
	}
	if c["expected_effects"].(float64) != 1 || c["observed_effects"].(float64) != 0 {
		t.Fatalf("counts = %v / %v", c["expected_effects"], c["observed_effects"])
	}
	if c["trigger_accepted"] != true {
		t.Fatalf("trigger_accepted = %v", c["trigger_accepted"])
	}
}

// omitempty on a nil pointer: an existing consumer of this stream must see the
// same bytes it saw before the field existed.
func TestNDJSONOmitsTheConsumerObjectForEveryOtherKind(t *testing.T) {
	tr := consumerTestResult()
	tr.Kind = models.HTTP
	tr.Consumer = nil
	raw, err := json.Marshal(buildTestVerdict("run-1", "test-set-0", tr))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "consumer") {
		t.Fatalf("an HTTP verdict must not carry a consumer key:\n%s", raw)
	}
}

// The dependency renderer had ONE label, "MISSING", because it had one
// producer. Applied to a consumer effect row it is simply false — a payload
// whose `status` field changed is not missing — and it sends the reader
// looking for a message that never arrived. models.IsEffectRow exists for
// exactly this dispatch, and until now nothing in production called it.
func TestEffectRowsAreLabelledByWhatWentWrong(t *testing.T) {
	row := func(name, key string) models.DepResult {
		return models.DepResult{Name: name, Type: "kafka", Meta: []models.DepMetaResult{{
			Normal: false, Key: key, Expected: "a", Actual: "b",
		}}}
	}
	tests := []struct {
		name string
		in   models.DepResult
		want string
	}{
		{"a sync-path row is unchanged", row("deps[0] postgres db:5432 (presence)", models.DepKeyPresence), "MISSING"},
		{"a recorded effect that never arrived", row("effects[0] kafka produce t", "effects.0.presence"), "MISSING"},
		{"an effect the recording does not have", row("effects[*] kafka produce t", "effects.*.unexpected"), "EXTRA"},
		{"a routing change", row("effects[0] kafka produce t", "effects.0.identity"), "REROUTED"},
		{"a payload the projector declined to model", row("effects[0] kafka produce t", "effects.0.decoded"), "OPAQUE"},
		{"a payload field diff", row("effects[0] kafka produce t", "effects.0.body.status"), "CHANGED"},
		{"a whole-body diff", row("effects[0] kafka produce t", "effects.0.body"), "CHANGED"},
		{"a named refusal", row("effects[*] kafka refused", "effects.refusal"), "REFUSED"},
		{"a window that closed for the wrong reason", row("effects[*] kafka window", "effects.end_reason"), "WINDOW"},
		{"a count disagreement", row("effects[*] kafka count", "effects.count"), "COUNT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := models.DepRowLabel(tt.in); got != tt.want {
				t.Fatalf("DepRowLabel = %q, want %q", got, tt.want)
			}
			if !strings.Contains(models.FormatDepResults([]models.DepResult{tt.in}), "  "+tt.want+" "+tt.in.Name) {
				t.Fatalf("the rendered block does not carry the label:\n%s", models.FormatDepResults([]models.DepResult{tt.in}))
			}
		})
	}
}

// JUnit calls a row a "dependency". An effects[i] row is an assertion about
// what the worker PRODUCED, not about a call it made.
func TestJUnitNamesEffectRowsAsEffects(t *testing.T) {
	lines := depFailureLines([]models.DepResult{
		{Name: "deps[0] postgres db:5432 (presence)", Type: "postgres", Meta: []models.DepMetaResult{{Key: models.DepKeyPresence, Expected: "consumed", Actual: "not consumed"}}},
		{Name: "effects[0] kafka produce order-events", Type: "kafka", Meta: []models.DepMetaResult{{Key: "effects.0.body.status", Expected: "CONFIRMED", Actual: "PENDING"}}},
	})
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.HasPrefix(lines[0], "dependency deps[0]") {
		t.Fatalf("the sync-path wording must be unchanged: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "effect effects[0]") {
		t.Fatalf("an effect row must not be called a dependency: %q", lines[1])
	}
}
