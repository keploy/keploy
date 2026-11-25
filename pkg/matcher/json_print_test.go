package matcher

import (
	"encoding/json"
	"maps"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// decodedJSON is doc decoded as json.Unmarshal decodes it into an
// interface{}.
func decodedJSON(t *testing.T, doc string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("%s: %v", doc, err)
	}
	return v
}

// Two documents DecodedJSONFieldsDiffering finds the same, under a
// printer's noise or under part of it, print the same, with the proven
// switch off and on: two prints that differ are a field the comparison
// finds differing. Two documents that differ where no noise or redacted
// value is read print differently.
func TestAPrintIsTheSameWhereTheDocumentsAreTheSame(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, tc := range fieldsDifferingRows {
			t.Run(tc.name, func(t *testing.T) {
				if proven {
					t.Setenv(provenswitch.Env, "1")
				}
				exp, act := decodedJSON(t, tc.exp), decodedJSON(t, tc.act)
				got := DecodedJSONFieldsDiffering(exp, act, tc.known, tc.typeStrict(), tc.excludedFunc())
				more := maps.Clone(tc.known)
				if more == nil {
					more = map[string][]string{}
				}
				more["no field of the rows"] = []string{"^x$"}
				for _, noise := range []map[string][]string{tc.known, more} {
					p := NewJSONPrinter(noise)
					pe, decided := p.Print(exp, tc.excludedFunc())
					pa, _ := p.Print(act, nil)
					if got == nil && decided && pe != pa {
						t.Fatalf("proven %v: %s and %s are the same under %v (learned %v), and their prints under %v differ", proven, tc.exp, tc.act, tc.known, tc.learned, noise)
					}
					if got != nil && len(tc.known) == 0 && !tc.excluded && pe == pa {
						t.Fatalf("proven %v: %s and %s differ at %q, and their prints under %v are the same", proven, tc.exp, tc.act, got, noise)
					}
				}
			})
		}
	}
}

// A printer leaves out what an entry names whatever its patterns, and
// whether it is learned: a field a pattern excuses, or only a learned
// entry's type checks, is left out, so the documents print the same where
// the comparison compares that field and finds it differing.
func TestAPrintLeavesOutWhatAnEntryNamesWhateverItsKind(t *testing.T) {
	for _, tc := range []struct {
		name, exp, act string
		known          map[string][]string
	}{
		{name: "a value outside the patterns", exp: `{"id":"u-1","q":1}`, act: `{"id":"x","q":1}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}},
		{name: "a field only one side has", exp: `{"q":1}`, act: `{"q":1,"id":"u-2"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}},
		{name: "an array's elements, their count too", exp: `{"ids":[1,2],"q":1}`, act: `{"ids":[3],"q":1}`, known: map[string][]string{"ids[]": {"^1$"}}},
		{name: "a field anywhere in the path", exp: `{"a":{"ts":1},"q":1}`, act: `{"a":{"ts":"x"},"q":1}`, known: map[string][]string{"ts": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewJSONPrinter(tc.known)
			pe, _ := p.Print(decodedJSON(t, tc.exp), nil)
			pa, _ := p.Print(decodedJSON(t, tc.act), nil)
			if pe != pa {
				t.Fatalf("%s and %s print differently under %v", tc.exp, tc.act, tc.known)
			}
		})
	}
}

// A print that reads a recorded value the obfuscator redacted, which the
// comparison does not compare, is undecided; one where an entry leaves that
// field out, or no value it reads is redacted, is decided.
func TestAPrintThatReadsARedactedValueIsUndecided(t *testing.T) {
	redacted := fieldsDifferingRow{excluded: true}.excludedFunc()
	for _, tc := range []struct {
		name, doc string
		known     map[string][]string
		decided   bool
	}{
		{name: "a redacted value", doc: `{"pw":"REDACTED-1","q":"a"}`},
		{name: "a redacted value in an array", doc: `{"pws":["a","REDACTED-1"]}`},
		{name: "a redacted value an entry leaves out", doc: `{"pw":"REDACTED-1","q":"a"}`, known: map[string][]string{"pw": {}}, decided: true},
		{name: "no redacted value", doc: `{"pw":"a","q":"b"}`, decided: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, decided := NewJSONPrinter(tc.known).Print(decodedJSON(t, tc.doc), redacted); decided != tc.decided {
				t.Fatalf("%s under %v: decided %v, want %v", tc.doc, tc.known, decided, tc.decided)
			}
		})
	}
}

// Under no noise a print builds no path: printing a document of strings,
// booleans and nulls, nested in objects and arrays, allocates nothing.
func TestAPrintUnderNoNoiseBuildsNoPath(t *testing.T) {
	doc := decodedJSON(t, `{"q":"ada","f":{"x":true,"y":null,"g":{"h":"i"}},"ids":["a",["b",{"c":"d"}]]}`)
	p := NewJSONPrinter(nil)
	if n := testing.AllocsPerRun(50, func() { p.Print(doc, nil) }); n != 0 {
		t.Fatalf("a print under no noise allocates %v times, want none", n)
	}
}
