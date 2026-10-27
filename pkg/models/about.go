package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// The words of the provenance format on disk, in one place: renaming one
// before the format is agreed is a change here (the struct tags that spell
// AboutKey are pinned to it by TestAboutKeyIsTheOnlySpelling).
const (
	// AboutKey is the key of the block that says where a decision came
	// from: noise.about.<path>, variables.<name>.about, clocks.<path>.about.
	AboutKey = "about"

	// AboutByLearned and AboutByHand are About.By: keploy's learn step wrote
	// the decision, or a person did.
	AboutByLearned = "learned"
	AboutByHand    = "hand"

	// AboutRoundFull and AboutRoundKeployTest are About.Round, in the round
	// names of verdicts.yaml: keploy mock record's full round, and the
	// schema-noise detection of keploy test.
	AboutRoundFull       = "full"
	AboutRoundKeployTest = "keploy-test"

	// AboutBetweenTests is About.Test for a decision learned on a call no
	// test was open for (a start-up or pool call).
	AboutBetweenTests = "between tests"
)

// About is where one learned decision on a recorded call came from: an
// ignored field (MockSpec.NoiseAbout), a variable (MockVariable.About) or a
// clock read (MockSpec.ClockAbout). It is written for people and agents who
// read or edit a set; replay never reads it, and it changes no match, bind,
// served answer or verdict. A decision with no About has an unknown origin:
// a person wrote it, or a keploy older than this format did.
//
// On disk, next to the decision it explains:
//
//	noise:
//	    req: [body.eventId]
//	    about:
//	        body.eventId:
//	            by: learned
//	            why: changed between the recording and the full round, ...
//	            seen: {record: 3f2b..., full: 9a01...}
//	            round: full
//	            run: 2026-10-27T05:00:00Z
//	            test: api::TestAppCRUD
//	            call: http audit:8080 POST /v1/events #1
//	            keploy: 3.8.91
//	            undo: delete body.eventId from noise.req and from noise.about ...
//	            sum: 3f9a1c0e
type About struct {
	// By is AboutByLearned or AboutByHand.
	By string `json:"by,omitempty" yaml:"by,omitempty" bson:"by,omitempty"`
	// Why is what keploy saw there and the rule that decided, in plain
	// words. It holds no values (Seen does); it names other calls by their
	// call key (Call), never by mock name, which a carry renames.
	Why string `json:"why,omitempty" yaml:"why,omitempty" bson:"why,omitempty"`
	// Seen is what each round saw at the place, each value cut.
	Seen *AboutSeen `json:"seen,omitempty" yaml:"seen,omitempty" bson:"seen,omitempty"`
	// Round is the round that learned it (AboutRoundFull,
	// AboutRoundKeployTest).
	Round string `json:"round,omitempty" yaml:"round,omitempty" bson:"round,omitempty"`
	// Run is when the run that learned it started, UTC, RFC 3339.
	Run string `json:"run,omitempty" yaml:"run,omitempty" bson:"run,omitempty"`
	// Test is the test open when it was learned, as mappings.yaml names it,
	// or AboutBetweenTests.
	Test string `json:"test,omitempty" yaml:"test,omitempty" bson:"test,omitempty"`
	// Call is this call in a form that survives renames: protocol,
	// destination as recorded, operation, then #n, its place among the
	// test's calls with the same key.
	Call string `json:"call,omitempty" yaml:"call,omitempty" bson:"call,omitempty"`
	// Keploy is the version of the keploy that wrote it.
	Keploy string `json:"keploy,omitempty" yaml:"keploy,omitempty" bson:"keploy,omitempty"`
	// Undo is the edit that undoes the decision, and what the next record
	// does then.
	Undo string `json:"undo,omitempty" yaml:"undo,omitempty" bson:"undo,omitempty"`
	// Sum is 8 hex characters over the decision as keploy wrote it: one
	// missing or not matching says a person edited it since.
	Sum string `json:"sum,omitempty" yaml:"sum,omitempty" bson:"sum,omitempty"`
	// Other is the keys of the block this keploy does not know, as written:
	// a rewrite keeps them (a newer keploy's keys survive an older one).
	Other UnknownKeys `json:"-" yaml:",inline" bson:"other,omitempty"`
}

// AboutSeen is what each round saw at a decision's place: the recording
// and record's full round.
type AboutSeen struct {
	Record string      `json:"record,omitempty" yaml:"record,omitempty" bson:"record,omitempty"`
	Full   string      `json:"full,omitempty" yaml:"full,omitempty" bson:"full,omitempty"`
	Other  UnknownKeys `json:"-" yaml:",inline" bson:"other,omitempty"`
}

// Clone returns a copy of a that shares no map or pointer with it.
func (a About) Clone() About {
	if a.Seen != nil {
		s := *a.Seen
		s.Other = maps.Clone(s.Other)
		a.Seen = &s
	}
	a.Other = maps.Clone(a.Other)
	return a
}

// CloneAbout returns a copy of a that shares no map or pointer with it; nil
// for nil.
func CloneAbout(a *About) *About {
	if a == nil {
		return nil
	}
	c := a.Clone()
	return &c
}

// CloneAbouts returns a copy of m that shares nothing with it; nil for an
// empty m.
func CloneAbouts(m map[string]About) map[string]About {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]About, len(m))
	for k, a := range m {
		out[k] = a.Clone()
	}
	return out
}

// MergeAbouts is a and b, abouts by place, as one (neither is changed): the
// first learner wins, so a place in both keeps a's.
func MergeAbouts(a, b map[string]About) map[string]About {
	out := CloneAbouts(a)
	for k, v := range b {
		if _, ok := out[k]; ok {
			continue
		}
		if out == nil {
			out = make(map[string]About, len(b))
		}
		out[k] = v.Clone()
	}
	return out
}

// CloneUnknowns returns a copy of m that shares nothing with it; nil for an
// empty m.
func CloneUnknowns(m map[string]UnknownKeys) map[string]UnknownKeys {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]UnknownKeys, len(m))
	for k, u := range m {
		out[k] = maps.Clone(u)
	}
	return out
}

type aboutJSON About

// MarshalJSON writes a with the keys it does not know (Other) beside its own.
func (a About) MarshalJSON() ([]byte, error) {
	return MarshalJSONWithOther(aboutJSON(a), a.Other)
}

// UnmarshalJSON reads a, keeping the keys it does not know in Other.
func (a *About) UnmarshalJSON(data []byte) error {
	var v aboutJSON
	other, err := UnmarshalJSONWithOther(data, &v)
	if err != nil {
		return err
	}
	v.Other = other
	*a = About(v)
	return nil
}

type aboutSeenJSON AboutSeen

// MarshalJSON writes s with the keys it does not know (Other) beside its own.
func (s AboutSeen) MarshalJSON() ([]byte, error) {
	return MarshalJSONWithOther(aboutSeenJSON(s), s.Other)
}

// UnmarshalJSON reads s, keeping the keys it does not know in Other.
func (s *AboutSeen) UnmarshalJSON(data []byte) error {
	var v aboutSeenJSON
	other, err := UnmarshalJSONWithOther(data, &v)
	if err != nil {
		return err
	}
	v.Other = other
	*s = AboutSeen(v)
	return nil
}

// UnknownKeys is, of a block of a recording, the keys this keploy does not know,
// each with its value as written, so a rewrite keeps them. A struct holds
// it in a field tagged yaml:",inline" and json:"-", and writes it in JSON
// with MarshalJSONWithOther.
type UnknownKeys map[string]UnknownValue

// UnknownValue is one value of a key this keploy does not know, as YAML
// text: it rides every codec a recording uses (YAML, JSON, gob) as written.
type UnknownValue struct {
	YAML string `json:"yaml" yaml:"yaml" bson:"yaml"`
}

// UnmarshalYAML keeps the value as written.
func (u *UnknownValue) UnmarshalYAML(n *yaml.Node) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	u.YAML = string(b)
	return nil
}

// MarshalYAML writes the value as it was read.
func (u UnknownValue) MarshalYAML() (any, error) {
	n, err := u.node()
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (u UnknownValue) node() (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(u.YAML), &doc); err != nil {
		return nil, fmt.Errorf("a kept unknown value is not YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return doc.Content[0], nil
}

// MarshalJSON writes the value as JSON.
func (u UnknownValue) MarshalJSON() ([]byte, error) {
	var v any
	if err := yaml.Unmarshal([]byte(u.YAML), &v); err != nil {
		return nil, fmt.Errorf("a kept unknown value is not YAML: %w", err)
	}
	return json.Marshal(jsonable(v))
}

// UnmarshalJSON keeps the value as written. JSON is YAML; only its flow
// style and quotes are dropped, where the type stays the same without them.
func (u *UnknownValue) UnmarshalJSON(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	blockStyle(&doc)
	b, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	u.YAML = string(b)
	return nil
}

// blockStyle clears the presentation styles of n and its children; the
// encoder quotes a string again where the type needs it.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// jsonable is v, as yaml.v3 decodes it into an interface, with every map
// keyed by strings, as encoding/json needs.
func jsonable(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = jsonable(e)
		}
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = jsonable(e)
		}
		return out
	case []any:
		for i, e := range t {
			t[i] = jsonable(e)
		}
		return t
	default:
		return v
	}
}

// MarshalJSONWithOther is the JSON of v, a struct whose type has no
// MarshalJSON (a conversion of the type that has one), with the keys of
// other beside its fields. A key of other that names a field of v is not
// written: the field is.
func MarshalJSONWithOther(v any, other UnknownKeys) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(other) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	known := jsonKeys(reflect.TypeOf(v))
	for k, u := range other {
		if known[strings.ToLower(k)] {
			continue
		}
		raw, err := u.MarshalJSON()
		if err != nil {
			return nil, err
		}
		m[k] = raw
	}
	return json.Marshal(m)
}

// UnmarshalJSONWithOther decodes data into v, a pointer to a struct whose
// type has no UnmarshalJSON, and returns the keys of data that name no
// field of v, as written; nil when there are none.
func UnmarshalJSONWithOther(data []byte, v any) (UnknownKeys, error) {
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, err
	}
	known := jsonKeys(reflect.TypeOf(v).Elem())
	var out UnknownKeys
	for k, raw := range m {
		// encoding/json matches a key to a field ignoring case.
		if known[strings.ToLower(k)] {
			continue
		}
		var u UnknownValue
		if err := u.UnmarshalJSON(raw); err != nil {
			return nil, err
		}
		if out == nil {
			out = UnknownKeys{}
		}
		out[k] = u
	}
	return out, nil
}

var jsonKeyCache sync.Map // reflect.Type -> map[string]bool

// jsonKeys is the JSON keys, lowered, of struct type t's fields, those of
// embedded structs it promotes included.
func jsonKeys(t reflect.Type) map[string]bool {
	if c, ok := jsonKeyCache.Load(t); ok {
		return c.(map[string]bool)
	}
	keys := map[string]bool{}
	addJSONKeys(t, keys)
	jsonKeyCache.Store(t, keys)
	return keys
}

func addJSONKeys(t reflect.Type, keys map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			addJSONKeys(f.Type, keys)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys[strings.ToLower(name)] = true
	}
}
