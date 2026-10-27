package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A key of a block this keploy does not know is kept as written through
// YAML and JSON, its type included: a string that reads as a number stays a
// string, a number stays a number, and a map and a list stay whole. So a
// newer keploy's keys survive a rewrite by this one, in either format.
func TestUnknownKeysKeepTheirValueAndTypeAcrossFormats(t *testing.T) {
	in := `by: learned
why: plain words
x-int: 7
x-str: "123"
x-map:
    a: [1, two]
    b: null
x-bool: true
`
	var a About
	if err := yaml.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	if a.By != AboutByLearned || a.Why != "plain words" {
		t.Fatalf("known keys: %+v", a)
	}
	if len(a.Other) != 4 {
		t.Fatalf("unknown keys kept: %v, want 4", a.Other)
	}
	j, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON map[string]any
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if fromJSON["x-str"] != "123" || fromJSON["x-int"] != float64(7) || fromJSON["x-bool"] != true {
		t.Fatalf("JSON types: %s", j)
	}
	var b About
	if err := json.Unmarshal(j, &b); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := yaml.Unmarshal([]byte(in), &want); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("YAML -> JSON -> YAML changed the block:\n%s\nwant\n%s", out, in)
	}
	if strings.Contains(string(out), "{") {
		t.Fatalf("a value read from JSON is written in flow style:\n%s", out)
	}
}

// A JSON key matches a field ignoring case, as encoding/json does, so a key
// that only differs in case is read into the field, not kept twice.
func TestAJSONKeyInAnotherCaseIsTheField(t *testing.T) {
	var a About
	if err := json.Unmarshal([]byte(`{"By":"hand","x-new":1}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.By != AboutByHand || len(a.Other) != 1 {
		t.Fatalf("got %+v", a)
	}
}

// A clone shares no map or pointer with what it copies.
func TestACloneOfAboutSharesNothing(t *testing.T) {
	a := About{By: AboutByLearned, Seen: &AboutSeen{Record: "r"}, Other: UnknownKeys{"x": {YAML: "1\n"}}}
	c := a.Clone()
	c.Seen.Record = "changed"
	c.Other["y"] = UnknownValue{YAML: "2\n"}
	if a.Seen.Record != "r" || len(a.Other) != 1 {
		t.Fatalf("the clone shares with the original: %+v", a)
	}
	v := MockVariables{"id": {Recorded: "1", About: &About{By: AboutByHand}, Other: UnknownKeys{"x": {YAML: "1\n"}}}}
	vc := v.Clone()
	vc["id"].About.By = "changed"
	vc["id"].Other["y"] = UnknownValue{}
	if v["id"].About.By != AboutByHand || len(v["id"].Other) != 1 {
		t.Fatalf("a variables clone shares with the original: %+v", v["id"])
	}
}

// A kept key that names a field is never written beside the field: the
// field is, so a JSON document holds each key once.
func TestAKeptKeyNamingAFieldIsNotWrittenTwice(t *testing.T) {
	a := About{By: AboutByLearned, Other: UnknownKeys{"by": {YAML: "hand\n"}, "x-new": {YAML: "1\n"}}}
	j, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(j) != `{"by":"learned","x-new":1}` {
		t.Fatalf("got %s", j)
	}
}
