package matcher

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// DecodedJSONFieldsDiffering compares two documents both ways and whole, and
// reads noise as collectJSON reads it: each row is one way two documents are,
// or are not, the same outside the noise, and names where they differ.
func TestDecodedJSONFieldsDiffering(t *testing.T) {
	redacted := func(v string) bool { return strings.HasPrefix(v, "REDACTED") }
	for _, tc := range []struct {
		name     string
		exp, act string
		known    map[string][]string
		learned  []string // typeStrict, each also in known with no patterns
		excluded bool     // the obfuscator redacted recorded values starting REDACTED
		want     []string
	}{
		{name: "the same document", exp: `{"q":"ada","f":{"x":1},"ids":[1,2]}`, act: `{"ids":[1,2],"f":{"x":1},"q":"ada"}`},
		{name: "a value differs", exp: `{"q":"ada"}`, act: `{"q":"bob"}`, want: []string{"q"}},
		{name: "a type differs", exp: `{"n":1}`, act: `{"n":"1"}`, want: []string{"n"}},
		{name: "a field only the live call has", exp: `{"q":"ada"}`, act: `{"q":"ada","admin":true}`, want: []string{"admin"}},
		{name: "a field only the recording has", exp: `{"q":"ada","x":1}`, act: `{"q":"ada"}`, want: []string{"x"}},
		{name: "a nested field only the live call has", exp: `{"f":{"x":1}}`, act: `{"f":{"x":1,"y":2}}`, want: []string{"f.y"}},
		{name: "an object only the live call has", exp: `{"q":"ada"}`, act: `{"q":"ada","f":{"y":2}}`, want: []string{"f"}},
		{name: "an object against a scalar", exp: `{"f":{"x":1}}`, act: `{"f":1}`, want: []string{"f"}},
		{name: "an array against a scalar", exp: `{"ids":[1]}`, act: `{"ids":1}`, want: []string{"ids"}},
		{name: "a scalar against an object", exp: `{"f":1}`, act: `{"f":{"x":1}}`, want: []string{"f"}},
		{name: "an array element differs, not the last", exp: `{"ids":[3,2]}`, act: `{"ids":[1,2]}`, want: []string{"ids[]"}},
		{name: "an array in another order", exp: `{"ids":[1,2]}`, act: `{"ids":[2,1]}`, want: []string{"ids[]"}},
		{name: "an array of another length", exp: `{"ids":[1,1]}`, act: `{"ids":[1]}`, want: []string{"ids[]"}},
		{name: "an array element's field only the live call has", exp: `{"items":[{"a":1}]}`, act: `{"items":[{"a":1,"b":2}]}`, want: []string{"items[].b"}},
		{name: "documents of another kind", exp: `[1]`, act: `{"a":1}`, want: []string{""}},
		{name: "a root array element differs", exp: `[1,2]`, act: `[1,3]`, want: []string{"[]"}},
		{name: "declared noise hides a field only the live call has", exp: `{"q":"ada"}`, act: `{"q":"ada","admin":true}`, known: map[string][]string{"admin": {}}},
		{name: "declared noise hides an object and all it holds", exp: `{"f":{"x":1}}`, act: `{"f":{"x":2,"y":3}}`, known: map[string][]string{"f": {}}},
		{name: "declared noise on an array's elements hides their count", exp: `{"ids":[1,2]}`, act: `{"ids":[7]}`, known: map[string][]string{"ids[]": {}}},
		{name: "declared noise on an array element's field", exp: `{"items":[{"ts":1},{"ts":2}]}`, act: `{"items":[{"ts":3},{"ts":4}]}`, known: map[string][]string{"items[].ts": {}}},
		{name: "learned noise: another value of the type", exp: `{"ts":1}`, act: `{"ts":2}`, known: map[string][]string{"ts": {}}, learned: []string{"ts"}},
		{name: "learned noise: another type", exp: `{"ts":1}`, act: `{"ts":"1"}`, known: map[string][]string{"ts": {}}, learned: []string{"ts"}, want: []string{"ts"}},
		{name: "learned noise: a field only the live call has", exp: `{"q":1}`, act: `{"q":1,"ts":2}`, known: map[string][]string{"ts": {}}, learned: []string{"ts"}, want: []string{"ts"}},
		{name: "patterns: both values inside", exp: `{"id":"u-1"}`, act: `{"id":"u-2"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}},
		{name: "patterns: the live value outside", exp: `{"id":"u-1"}`, act: `{"id":"x"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}, want: []string{"id"}},
		{name: "patterns: the recorded value outside", exp: `{"id":"x"}`, act: `{"id":"u-2"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}, want: []string{"id"}},
		{name: "patterns: the same value outside them", exp: `{"id":"x"}`, act: `{"id":"x"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}},
		{name: "patterns: the same text of another type", exp: `{"id":"1"}`, act: `{"id":1}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}, want: []string{"id"}},
		{name: "patterns: a field only the live call has", exp: `{"q":1}`, act: `{"q":1,"id":"u-2"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}, want: []string{"id"}},
		{name: "patterns: a redacted recorded value, the live one inside", exp: `{"id":"REDACTED-1"}`, act: `{"id":"u-2"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}, excluded: true},
		{name: "patterns: a redacted recorded value, the live one outside", exp: `{"id":"REDACTED-1"}`, act: `{"id":"x"}`, known: map[string][]string{"id": {"^u-[0-9]+$"}}, excluded: true, want: []string{"id"}},
		{name: "a redacted recorded value is not compared", exp: `{"pw":"REDACTED-1"}`, act: `{"pw":"hunter2"}`, excluded: true},
		{name: "a redacted recorded field the call lacks", exp: `{"pw":"REDACTED-1","q":1}`, act: `{"q":1}`, excluded: true, want: []string{"pw"}},
		{name: "null against an object", exp: `{"f":null}`, act: `{"f":{"x":1}}`, want: []string{"f"}},
		{name: "a longer array in the call", exp: `{"ids":[1]}`, act: `{"ids":[1,1]}`, want: []string{"ids[]"}},
		{name: "a value that is not redacted is", exp: `{"pw":"secret"}`, act: `{"pw":"hunter2"}`, excluded: true, want: []string{"pw"}},
		{name: "noise read without case, named in the documents'", exp: `{"Q":"ada","TS":1}`, act: `{"Q":"bob","TS":2}`, known: map[string][]string{"ts": {}}, want: []string{"Q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var exp, act interface{}
			if err := json.Unmarshal([]byte(tc.exp), &exp); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.act), &act); err != nil {
				t.Fatal(err)
			}
			var typeStrict map[string]struct{}
			for _, k := range tc.learned {
				if typeStrict == nil {
					typeStrict = map[string]struct{}{}
				}
				typeStrict[k] = struct{}{}
			}
			var excluded func(string) bool
			if tc.excluded {
				excluded = redacted
			}
			got := DecodedJSONFieldsDiffering(exp, act, tc.known, typeStrict, excluded)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s against %s under %v (learned %v) differ at %q, want %q", tc.act, tc.exp, tc.known, tc.learned, got, tc.want)
			}
		})
	}
}

// Of several fields that differ, each is named once, sorted.
func TestDecodedJSONFieldsDifferingNamesEachOnceSorted(t *testing.T) {
	var exp, act interface{}
	_ = json.Unmarshal([]byte(`{"z":1,"a":[1,2],"m":{"x":1}}`), &exp)
	_ = json.Unmarshal([]byte(`{"z":2,"a":[3,4],"m":{"x":2},"b":true}`), &act)
	for range 10 { // the fields are found in map order, random on every walk
		got := DecodedJSONFieldsDiffering(exp, act, nil, nil, nil)
		if want := []string{"a[]", "b", "m.x", "z"}; !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
