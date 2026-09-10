package matcher

import (
	"testing"

	"go.uber.org/zap"
)

// TestFailureAssessment_AgreesWithTheMatcher pins that the failure report and
// the pass/fail verdict use the same notion of "noisy".
//
// collectJSON used to treat any pattern-guarded entry as an unconditional skip,
// so it dropped subtrees the matcher was still comparing. The user got a red
// test alongside a report claiming nothing had changed.
func TestFailureAssessment_AgreesWithTheMatcher(t *testing.T) {
	cases := []struct {
		name        string
		expected    string
		actual      string
		noise       map[string][]string
		wantFailure bool
	}{
		{
			name:        "pattern-guarded container: matcher fails, report must explain it",
			expected:    `{"a":{"b":{"c":"x"}}}`,
			actual:      `{"a":{"b":{"c":"x","d":1}}}`,
			noise:       map[string][]string{"a.b": {"^zzz$"}},
			wantFailure: true,
		},
		{
			name:        "pattern-guarded scalar the pattern matches: genuinely noise",
			expected:    `{"a":{"b":"ord-1"}}`,
			actual:      `{"a":{"b":"ord-2"}}`,
			noise:       map[string][]string{"a.b": {`^ord-\d+$`}},
			wantFailure: false,
		},
		{
			name:        "pattern-guarded scalar outside the pattern: a real difference",
			expected:    `{"a":{"b":"ord-1"}}`,
			actual:      `{"a":{"b":"HACK"}}`,
			noise:       map[string][]string{"a.b": {`^ord-\d+$`}},
			wantFailure: true,
		},
		{
			name:        "unconditional entry hides the subtree from both",
			expected:    `{"a":{"b":{"c":"x"}}}`,
			actual:      `{"a":{"b":{"c":"y","d":1}}}`,
			noise:       map[string][]string{"a.b": {}},
			wantFailure: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp, act := c.expected, c.actual
			v, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
			if err != nil {
				t.Fatalf("ValidateAndMarshalJSON: %v", err)
			}
			res, err := JSONDiffWithNoiseControl(v, c.noise, false, nil)
			if err != nil {
				t.Fatalf("JSONDiffWithNoiseControl: %v", err)
			}
			matcherFailed := !res.IsExact()
			if matcherFailed != c.wantFailure {
				t.Fatalf("matcher verdict failed=%v, want %v", matcherFailed, c.wantFailure)
			}

			fa, err := ComputeFailureAssessmentJSON(c.expected, c.actual, c.noise, false)
			if err != nil {
				t.Fatalf("ComputeFailureAssessmentJSON: %v", err)
			}
			reportSawSomething := fa != nil && (len(fa.AddedFields) > 0 ||
				len(fa.RemovedFields) > 0 ||
				len(fa.TypeChanges) > 0 ||
				len(fa.ValueChanges) > 0)

			if matcherFailed && !reportSawSomething {
				t.Errorf("matcher failed the test but the report found nothing to explain it: %+v", fa)
			}
			if !matcherFailed && reportSawSomething {
				t.Errorf("matcher passed but the report claims a change: %+v", fa)
			}
		})
	}
}
