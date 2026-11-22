package matcher

import (
	"encoding/json"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

func decodeArray(t *testing.T, s string) []interface{} {
	t.Helper()
	var v []interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

func TestArrayReordered(t *testing.T) {
	for _, tc := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"same order", `[1,2,3]`, `[1,2,3]`, false},
		{"scalars swapped", `[1,2,3]`, `[2,1,3]`, true},
		{"one scalar drifted in place", `[1,2,3]`, `[1,2,4]`, false},
		{"objects in the recorded order, one leaf drifted", `[{"id":1,"n":"a"},{"id":2,"n":"b"}]`, `[{"id":1,"n":"x"},{"id":2,"n":"b"}]`, false},
		{"objects swapped", `[{"id":1,"n":"a"},{"id":2,"n":"b"}]`, `[{"id":2,"n":"b"},{"id":1,"n":"a"}]`, true},
		// The golden: no element equals any recorded element, so a permutation
		// test sees nothing, yet every element prefers the other index by its id.
		{"reorder plus one drifting leaf per element", `[{"id":1,"expires_in":100},{"id":2,"expires_in":200}]`, `[{"id":2,"expires_in":161},{"id":1,"expires_in":61}]`, true},
		{"duplicates in the same order", `[{"a":1},{"a":1}]`, `[{"a":1},{"a":1}]`, false},
		{"a tie is not a preference", `[{"a":1,"b":1},{"a":1,"b":2}]`, `[{"a":1,"b":3},{"a":1,"b":4}]`, false},
		{"nested arrays move with their element", `[{"k":"x","v":[1,2]},{"k":"y","v":[3]}]`, `[{"k":"y","v":[3]},{"k":"x","v":[1,2]}]`, true},
		{"empty containers are leaves too", `[[],[1]]`, `[[1],[]]`, true},
		{"types differ: \"1\" is not 1", `[1,"1"]`, `["1",1]`, true},
		{"a single element has no other index", `[1]`, `[2]`, false},
		{"lengths differ", `[1,2]`, `[2,1,3]`, false},
		{"empty", `[]`, `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArrayReordered(decodeArray(t, tc.recorded), decodeArray(t, tc.live)); got != tc.want {
				t.Fatalf("ArrayReordered(%s, %s) = %v, want %v", tc.recorded, tc.live, got, tc.want)
			}
		})
	}
}

// The documented false refusal: a drifted value that happens to equal a
// recorded neighbour reads as a reorder. It keeps the test red with a reason;
// it can never make one green.
func TestArrayReorderedRefusesAnExplainableDriftRatherThanGreening(t *testing.T) {
	if !ArrayReordered(decodeArray(t, `[1,2,3]`), decodeArray(t, `[1,2,1]`)) {
		t.Fatal("the last element now equals recorded[0] and nothing at its own index: the rule prefers another index, by design")
	}
}

func TestResponseIgnoreOrdering(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	if !ResponseIgnoreOrdering(true) || ResponseIgnoreOrdering(false) {
		t.Fatal("switch off: the configured ignoreOrdering must pass through unchanged")
	}
	t.Setenv(provenswitch.Env, "1")
	if ResponseIgnoreOrdering(true) {
		t.Fatal("KEPLOY_PROVEN=1: response arrays must be compared in order even with test.ignoreOrdering on")
	}
}
