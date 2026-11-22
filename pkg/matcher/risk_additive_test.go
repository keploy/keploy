package matcher

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// "ONLY NEW FIELDS WERE ADDED" IS GRADED ONLY WHEN THE ANSWER CONTAINS THE
// RECORDING, ELEMENT BY ELEMENT. The path maps fold an array's elements into
// one "[]" path where the last element's value wins, so a value changed in an
// earlier element beside an added field was graded Low, and keploy test's
// additive-schema auto-pass, which trusts Low, passed it (a price 10 -> 99,
// or a paid order's id moved onto the pending order). Each recorded element
// must pair with a distinct answer element that contains it: by index, or,
// under ignoreOrdering, by a matching of the elements.
func TestComputeFailureAssessmentJSON_OnlyAddedIsGradedOnlyUnderContainment(t *testing.T) {
	const (
		twoItems = `{"items":[{"price":10,"n":"a"},{"price":20,"n":"b"}]}`
		orders   = `{"orders":[{"id":"ord_REC1","status":"paid"},{"id":"ord_lit","status":"pending"},{"id":"ord_z","status":"shipped"}]}`
	)
	for _, c := range []struct {
		name           string
		exp, act       string
		noise          map[string][]string
		ignoreOrdering bool
		want           models.RiskLevel
		reason         string
	}{
		{name: "an earlier element's value changed beside an added field", exp: twoItems,
			act: `{"items":[{"price":99,"n":"a","x":1},{"price":20,"n":"b","x":1}]}`, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "the same under ignoreOrdering", exp: twoItems,
			act: `{"items":[{"price":99,"n":"a","x":1},{"price":20,"n":"b","x":1}]}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "the last element's value changed beside an added field", exp: twoItems,
			act: `{"items":[{"price":10,"n":"a","x":1},{"price":99,"n":"b","x":1}]}`, want: models.Medium},
		{name: "an id moved between elements, a field added to each", exp: orders,
			act:            `{"orders":[{"id":"ord_lit","status":"paid","x":1},{"id":"ord_REC1","status":"pending","x":1},{"id":"ord_z","status":"shipped","x":1}]}`,
			ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "an id moved between elements, a field added to the last only", exp: orders,
			act:            `{"orders":[{"id":"ord_lit","status":"paid"},{"id":"ord_REC1","status":"pending"},{"id":"ord_z","status":"shipped","x":1}]}`,
			ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "a field removed from one element that another keeps", exp: `{"items":[{"a":1,"b":2},{"a":1,"b":2}]}`,
			act: `{"items":[{"a":1,"b":2,"c":3},{"a":1,"c":3}]}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "an element added", exp: `{"items":[{"a":1}]}`, act: `{"items":[{"a":1},{"a":1,"b":1}]}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "an element removed", exp: `{"items":[{"a":1},{"a":1}],"k":1}`, act: `{"items":[{"a":1}],"k":1,"x":1}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "a scalar element changed", exp: `{"tags":["b","a"]}`, act: `{"tags":["c","a"],"x":1}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "a field added to every element, same order", exp: twoItems,
			act: `{"items":[{"price":10,"n":"a","x":1},{"price":20,"n":"b","x":1}]}`, want: models.Low},
		{name: "a field added to every element, reordered, under ignoreOrdering", exp: twoItems,
			act: `{"items":[{"price":20,"n":"b","x":1},{"price":10,"n":"a","x":1}]}`, ignoreOrdering: true, want: models.Low},
		{name: "reordered without ignoreOrdering", exp: `{"items":[{"a":1},{"a":2},{"a":3}]}`,
			act: `{"items":[{"a":2,"x":1},{"a":1,"x":1},{"a":3,"x":1}]}`, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "the same under ignoreOrdering", exp: `{"items":[{"a":1},{"a":2},{"a":3}]}`,
			act: `{"items":[{"a":2,"x":1},{"a":1,"x":1},{"a":3,"x":1}]}`, ignoreOrdering: true, want: models.Low},
		// {"a":1} fits both answer elements; the first that fits is the one
		// {"a":1,"b":2} needs. A matching moves it to the other.
		{name: "an element the first fit would take from the only element another fits", exp: `{"items":[{"a":1},{"a":1,"b":2}]}`,
			act: `{"items":[{"a":1,"b":2,"x":1},{"a":1,"y":1}]}`, ignoreOrdering: true, want: models.Low},
		{name: "the same by index", exp: `{"items":[{"a":1},{"a":1,"b":2}]}`,
			act: `{"items":[{"a":1,"b":2,"x":1},{"a":1,"y":1}]}`, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "nested arrays", exp: `{"o":[{"lines":[{"sku":"a"},{"sku":"b"}]},{"lines":[{"sku":"c"}]}]}`,
			act: `{"o":[{"lines":[{"sku":"b","q":1},{"sku":"a","q":1}]},{"lines":[{"sku":"c","q":1}]}]}`, ignoreOrdering: true, want: models.Low},
		{name: "nested arrays, an inner element changed", exp: `{"o":[{"lines":[{"sku":"a"},{"sku":"b"}]},{"lines":[{"sku":"c"}]}]}`,
			act: `{"o":[{"lines":[{"sku":"b","q":1},{"sku":"z","q":1}]},{"lines":[{"sku":"c","q":1}]}]}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		{name: "values changed inside an array the path maps read as identical", exp: `{"items":[{"a":1},{"a":2}]}`,
			act: `{"items":[{"a":2},{"a":2}]}`, ignoreOrdering: true, want: models.Medium, reason: "An array no longer holds what it held"},
		// Contained, but the path maps read v[] as retyped: main's HIGH
		// stands, so it is neither auto-passed nor learned as noise.
		{name: "a mixed array reordered beside an added field", exp: `{"v":[1,"s"]}`, act: `{"v":["s",1],"x":1}`, ignoreOrdering: true, want: models.High},
		{name: "a noised value changed in an earlier element", exp: twoItems,
			act: `{"items":[{"price":99,"n":"a","x":1},{"price":20,"n":"b","x":1}]}`, noise: map[string][]string{"price": nil}, want: models.Low},
		{name: "a noised path changed in an earlier element", exp: twoItems,
			act: `{"items":[{"price":99,"n":"a","x":1},{"price":20,"n":"b","x":1}]}`, noise: map[string][]string{"items.price": nil}, want: models.Low},
		// Main's path maps name an element's field items[].ts, so a dotted
		// entry items.ts never applied there and this was MEDIUM; the
		// comparison applies it, and so does containment (PN1).
		{name: "a noised path changed in the last element", exp: `{"items":[{"id":1,"ts":"a"},{"id":2,"ts":"b"}]}`,
			act: `{"items":[{"id":1,"ts":"a","x":1},{"id":2,"ts":"CHG","x":1}]}`, noise: map[string][]string{"items.ts": nil}, want: models.Low},
		{name: "the same under ignoreOrdering", exp: `{"items":[{"id":1,"ts":"a"},{"id":2,"ts":"b"}]}`,
			act: `{"items":[{"id":1,"ts":"a","x":1},{"id":2,"ts":"CHG","x":1}]}`, noise: map[string][]string{"items.ts": nil}, ignoreOrdering: true, want: models.Low},
		// A field removed from every element, beside an added one, under a
		// noise entry over it: the answer is contained (the noise covers the
		// field), but the path maps read items[].ts as removed, which keeps
		// main's HIGH: never auto-passed, never learned as noise (TR9-15).
		{name: "a noised field removed from every element beside an added field", exp: `{"items":[{"id":1,"ts":"a"},{"id":2,"ts":"b"}]}`,
			act: `{"items":[{"id":1,"x":1},{"id":2,"x":1}]}`, noise: map[string][]string{"items.ts": nil}, want: models.High, reason: "Removed fields"},
		{name: "the same under ignoreOrdering", exp: `{"items":[{"id":1,"ts":"a"},{"id":2,"ts":"b"}]}`,
			act: `{"items":[{"id":1,"x":1},{"id":2,"x":1}]}`, noise: map[string][]string{"items.ts": nil}, ignoreOrdering: true, want: models.High, reason: "Removed fields"},
		{name: "a pattern the changed value does not match", exp: twoItems,
			act: `{"items":[{"price":99,"n":"a","x":1},{"price":20,"n":"b","x":1}]}`, noise: map[string][]string{"price": {"^1\\d$"}}, want: models.Medium, reason: "An array no longer holds what it held"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := ComputeFailureAssessmentJSON(c.exp, c.act, c.noise, c.ignoreOrdering)
			if err != nil || a == nil {
				t.Fatalf("assessment: %v, %v", a, err)
			}
			if a.Risk != c.want {
				t.Errorf("risk %s, want %s (reasons %q)", a.Risk, c.want, a.Reasons)
			}
			if c.want == models.Low && (len(a.Category) != 1 || a.Category[0] != models.SchemaAdded) {
				t.Errorf("categories %v, want SCHEMA_ADDED", a.Category)
			}
			if c.want == models.Low && (len(a.RemovedFields)+len(a.TypeChanges)+len(a.ValueChanges) != 0 || len(a.AddedFields) == 0) {
				t.Errorf("only new fields, yet added %v removed %v retyped %v changed %v", a.AddedFields, a.RemovedFields, a.TypeChanges, a.ValueChanges)
			}
			switch {
			case c.want == models.High && c.reason == "Removed fields":
				if len(a.RemovedFields) == 0 {
					t.Errorf("a removed field is not named: %+v", a)
				}
			case c.want == models.High && len(a.TypeChanges) == 0:
				t.Errorf("a retyped field is not named: %+v", a)
			}
			if c.reason != "" && !strings.Contains(strings.Join(a.Reasons, " "), c.reason) {
				t.Errorf("reasons %q do not say %q", a.Reasons, c.reason)
			}
			if c.want != models.Low && strings.Contains(strings.Join(a.Reasons, " "), "only new fields added") {
				t.Errorf("reasons %q still say only new fields were added", a.Reasons)
			}
		})
	}
}

// AN UNDECIDED CONTAINMENT IS NO CONTAINMENT. Pairing reordered elements can
// cost a comparison per pair; past the budget the walk gives up, and the
// change is not graded as only new fields.
func TestComputeFailureAssessmentJSON_AnArrayTooLargeToPairIsNotGradedOnlyAdded(t *testing.T) {
	defer func(b int) { containmentBudget = b }(containmentBudget)
	var exp, act []string
	for i := 0; i < 8; i++ {
		exp = append(exp, fmt.Sprintf(`{"k":%d}`, i))
		act = append(act, fmt.Sprintf(`{"k":%d,"x":1}`, 7-i))
	}
	e, a := `{"items":[`+strings.Join(exp, ",")+`]}`, `{"items":[`+strings.Join(act, ",")+`]}`

	containmentBudget = 8 * 8
	got, err := ComputeFailureAssessmentJSON(e, a, nil, true)
	if err != nil || got == nil || got.Risk != models.Low {
		t.Fatalf("within the budget a reversed array with a field added to each element is only new fields: %+v, %v", got, err)
	}
	containmentBudget = 10
	got, err = ComputeFailureAssessmentJSON(e, a, nil, true)
	if err != nil || got == nil || got.Risk != models.Medium || !strings.Contains(strings.Join(got.Reasons, " "), "too many elements to pair") {
		t.Fatalf("past the budget: %+v, %v; want MEDIUM, saying the arrays were not paired", got, err)
	}

	// Elements in the same order cost one comparison each: each is tried
	// first against the element at its own index, so n of them fit a budget
	// of n (R8T-01).
	act = nil
	for i := 0; i < 8; i++ {
		act = append(act, fmt.Sprintf(`{"k":%d,"x":1}`, i))
	}
	containmentBudget = 8
	got, err = ComputeFailureAssessmentJSON(e, `{"items":[`+strings.Join(act, ",")+`]}`, nil, true)
	if err != nil || got == nil || got.Risk != models.Low {
		t.Fatalf("eight elements in the same order, a field added to each, under a budget of eight: %+v, %v; want LOW", got, err)
	}
}

// JSONContainsWithNoiseControl is the comparison with members only the
// answer has allowed: every rule of JSONDiffWithNoiseControl holds for what
// the recording has.
func TestJSONContainsWithNoiseControl(t *testing.T) {
	for _, c := range []struct {
		name, exp, act string
		noise          map[string][]string
		ignoreOrdering bool
		contains       bool
	}{
		{"equal", `{"a":1,"b":[1,2]}`, `{"a":1,"b":[1,2]}`, nil, false, true},
		{"a member added at depth", `{"a":{"b":1}}`, `{"a":{"b":1,"c":2},"d":3}`, nil, false, true},
		{"a member missing", `{"a":1,"b":2}`, `{"a":1,"c":2}`, nil, false, false},
		{"a member missing under unconditional noise", `{"a":1,"b":2}`, `{"a":1,"c":2}`, map[string][]string{"b": nil}, false, true},
		{"a value changed", `{"a":1}`, `{"a":2,"c":1}`, nil, false, false},
		{"a type changed", `{"a":1}`, `{"a":"1","c":1}`, nil, false, false},
		{"null kept", `{"a":null}`, `{"a":null,"c":1}`, nil, false, true},
		{"the root's type changed", `{"a":1}`, `[{"a":1}]`, nil, false, false},
		{"an element added", `[1,2]`, `[1,2,3]`, nil, true, false},
		{"scalars reordered", `[1,2,2]`, `[2,1,2]`, nil, true, true},
		{"scalars reordered, by index", `[1,2]`, `[2,1]`, nil, false, false},
		{"a duplicate element is not paired twice", `[{"a":1},{"a":1}]`, `[{"a":1,"x":1},{"a":2,"x":1}]`, nil, true, false},
		{"an empty array", `{"a":[]}`, `{"a":[],"b":1}`, nil, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var e, a interface{}
			if err := json.Unmarshal([]byte(c.exp), &e); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(c.act), &a); err != nil {
				t.Fatal(err)
			}
			got, decided, err := JSONContainsWithNoiseControl(NewValidatedJSON(e, a), c.noise, c.ignoreOrdering, nil)
			if err != nil || !decided || got != c.contains {
				t.Errorf("contains=%v decided=%v err=%v; want contains=%v", got, decided, err, c.contains)
			}
		})
	}
}
