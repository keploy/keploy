package matcher

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"unsafe"

	"go.uber.org/zap"
)

func parseDoc(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

// exactByStrings is the string entry point a caller replaces:
// ValidateAndMarshalJSON, then JSONDiffWithNoiseControl without ordering.
func exactByStrings(exp, act string, noise map[string][]string) bool {
	vj, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
	if err != nil || !vj.IsIdentical() {
		return false
	}
	res, err := JSONDiffWithNoiseControl(vj, noise, false, zap.NewNop())
	return err == nil && res.IsExact()
}

func exactParsed(exp, act interface{}, noise map[string][]string) bool {
	vj := NewValidatedJSON(exp, act)
	if !vj.IsIdentical() {
		return false
	}
	res, err := JSONDiffWithNoiseControl(vj, noise, false, zap.NewNop())
	return err == nil && res.IsExact()
}

// A fixed corpus of the shapes the match key and the collected fields must get
// right: member order, array order and length, -0, global noise at depth, path
// noise with and without patterns, a member added or missing under noise, a
// top-level type change, global keys that differ only in case.
var preparedCorpus = []struct {
	exp, act string
	noise    map[string][]string
}{
	{`{"a":1,"b":"x"}`, `{"b":"x","a":1}`, nil},
	{`{"a":1,"b":"x"}`, `{"a":2,"b":"x"}`, nil},
	{`{"a":-0}`, `{"a":0}`, nil},
	{`{"a":[1,2]}`, `{"a":[2,1]}`, nil},
	{`{"a":[1,2]}`, `{"a":[1,2,3]}`, map[string][]string{"a": {}}},
	{`{"x":{"id":"1","k":2}}`, `{"x":{"id":"2","k":2}}`, map[string][]string{"id": {}}},
	{`{"x":{"id":"1","k":2}}`, `{"x":{"id":"2","k":2}}`, map[string][]string{"id": {"^2$"}}},
	{`{"x":{"id":"1","k":2}}`, `{"x":{"id":"3","k":2}}`, map[string][]string{"id": {"^2$"}}},
	{`{"x":{"id":"1","k":2}}`, `{"x":{"k":2}}`, map[string][]string{"x.id": {}}},
	{`{"x":{"id":"1","k":2}}`, `{"x":{"k":2,"id":"1","extra":true}}`, map[string][]string{"x.extra": {}}},
	{`{"x":{"id":"1","k":2}}`, `{"x":{"k":2,"id":"1","extra":true}}`, nil},
	{`{"ts":"a","n":{"ts":"b"}}`, `{"ts":"c","n":{"ts":"d"}}`, map[string][]string{"TS": {}}},
	{`{"Data":{"CreatedAt":"a"}}`, `{"Data":{"CreatedAt":"b"}}`, map[string][]string{"data.createdat": {}}},
	{`{"a":{"b":1}}`, `{"a":null}`, map[string][]string{"a.b": {}}},
	{`[{"a":1},{"a":2}]`, `[{"a":1},{"a":3}]`, map[string][]string{"a": {"^[23]$"}}},
	{`{"a":1}`, `[1]`, nil},
	{`{"a":"1"}`, `{"a":1}`, nil},
	{`{"a":null}`, `{"a":"x"}`, map[string][]string{"q.a": {}}},
	{`{"n":{"createdAt":"z"}}`, `{"n":{"createdAt":"y"}}`, map[string][]string{"CreatedAt": {}, "createdAt": {"^v[0-9]$"}}},
	{`{"n":{"createdAt":"z"}}`, `{"n":{"createdAt":"y"}}`, map[string][]string{"CreatedAt": {"^v[0-9]$"}, "createdAt": {}}},
}

func TestNewValidatedJSONComparesAsTheStringEntryPoint(t *testing.T) {
	for i, c := range preparedCorpus {
		if got, want := exactParsed(parseDoc(t, c.exp), parseDoc(t, c.act), c.noise), exactByStrings(c.exp, c.act, c.noise); got != want {
			t.Errorf("case %d %s vs %s noise %v: parsed=%v strings=%v", i, c.exp, c.act, c.noise, got, want)
		}
	}
}

func TestCountJSONFieldDiffsCountsJSONFieldDiffs(t *testing.T) {
	for i, c := range preparedCorpus {
		want := len(JSONFieldDiffs(c.exp, c.act, c.noise, "body.", 0))
		got := CountJSONFieldDiffs(CollectJSONFields(parseDoc(t, c.exp), c.noise), CollectJSONFields(parseDoc(t, c.act), c.noise))
		if got != want {
			t.Errorf("case %d %s vs %s noise %v: count=%d, JSONFieldDiffs=%d", i, c.exp, c.act, c.noise, got, want)
		}
	}
}

func TestJSONMatchKeyCorpus(t *testing.T) {
	for i, c := range preparedCorpus {
		e, a := parseDoc(t, c.exp), parseDoc(t, c.act)
		if exactByStrings(c.exp, c.act, c.noise) && JSONMatchKey(e, c.noise) != JSONMatchKey(a, c.noise) {
			t.Errorf("case %d %s vs %s noise %v: exact, but the keys differ", i, c.exp, c.act, c.noise)
		}
	}
	// The key is not vacuous: different documents no noise excuses differ.
	if JSONMatchKey(parseDoc(t, `{"a":1}`), nil) == JSONMatchKey(parseDoc(t, `{"a":2}`), nil) {
		t.Fatal("different values hash alike")
	}
	if JSONMatchKey(parseDoc(t, `{"a":"1"}`), nil) == JSONMatchKey(parseDoc(t, `{"a":1}`), nil) {
		t.Fatal("a string and a number hash alike")
	}
}

// randDoc builds a random document of the shape message payloads have: objects
// of scalars, nested objects and arrays, with a few member names reused at
// several depths.
func randDoc(r *rand.Rand, depth int) interface{} {
	names := []string{"id", "uuid", "createdAt", "event", "Session", "data", "items", "n", "flag"}
	scalar := func() interface{} {
		switch r.Intn(5) {
		case 0:
			return fmt.Sprintf("v%d", r.Intn(4))
		case 1:
			return float64(r.Intn(4) - 1)
		case 2:
			return r.Intn(2) == 0
		case 3:
			return nil
		default:
			return fmt.Sprintf("s-%d", r.Intn(3))
		}
	}
	if depth <= 0 {
		return scalar()
	}
	switch r.Intn(6) {
	case 0, 1, 2:
		m := map[string]interface{}{}
		for i := r.Intn(5); i >= 0; i-- {
			m[names[r.Intn(len(names))]] = randDoc(r, depth-1)
		}
		return m
	case 3:
		a := make([]interface{}, r.Intn(3))
		for i := range a {
			a[i] = randDoc(r, depth-1)
		}
		return a
	default:
		return scalar()
	}
}

// mutate returns a copy of v with one random change: a scalar changed, a member
// added or removed, an array element changed, reordered or dropped.
func mutate(r *rand.Rand, v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, e := range t {
			out[k] = e
		}
		keys := make([]string, 0, len(out))
		for k := range out {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		switch {
		case len(keys) == 0 || r.Intn(5) == 0:
			out[fmt.Sprintf("extra%d", r.Intn(2))] = randDoc(r, 1)
		case r.Intn(5) == 0:
			delete(out, keys[r.Intn(len(keys))])
		default:
			k := keys[r.Intn(len(keys))]
			out[k] = mutate(r, out[k])
		}
		return out
	case []interface{}:
		out := append([]interface{}(nil), t...)
		switch {
		case len(out) == 0:
			return append(out, randDoc(r, 1))
		case r.Intn(4) == 0 && len(out) > 1:
			out[0], out[1] = out[1], out[0]
		case r.Intn(4) == 0:
			out = out[1:]
		default:
			i := r.Intn(len(out))
			out[i] = mutate(r, out[i])
		}
		return out
	default:
		return randDoc(r, 1)
	}
}

// randNoise names members of doc: global keys, and paths (whole or as a
// substring), each unconditional or with a pattern.
func randNoise(r *rand.Rand, doc interface{}) map[string][]string {
	var paths []string
	var walk func(v interface{}, p string)
	walk = func(v interface{}, p string) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, e := range t {
				cp := k
				if p != "" {
					cp = p + "." + k
				}
				paths = append(paths, cp)
				walk(e, cp)
			}
		case []interface{}:
			for _, e := range t {
				walk(e, p)
			}
		}
	}
	walk(doc, "")
	sort.Strings(paths)
	noise := map[string][]string{}
	if len(paths) == 0 {
		return noise
	}
	for i := r.Intn(3); i >= 0; i-- {
		p := paths[r.Intn(len(paths))]
		switch r.Intn(3) {
		case 0: // global key: the last segment
			p = p[strings.LastIndex(p, ".")+1:]
			// Sometimes in another case, so two keys of one set may differ
			// only in case ("createdAt", "CreatedAt").
			if r.Intn(2) == 0 {
				p = strings.ToUpper(p[:1]) + p[1:]
			}
		case 1:
			if strings.Contains(p, ".") {
				p = strings.ToUpper(p[:1]) + p[1:]
			}
		}
		var pats []string
		switch r.Intn(3) {
		case 0:
			pats = []string{"^v[0-9]$"}
		case 1:
			pats = []string{"^s-"}
		}
		noise[p] = pats
	}
	return noise
}

// Property: a pair JSONDiffWithNoiseControl finds exact has equal keys, and the
// collected fields count JSONFieldDiffs' diffs, over random documents,
// mutations and noise. The run must see enough exact pairs of different
// documents to mean something.
func TestPreparedJSONProperties(t *testing.T) {
	r := rand.New(rand.NewSource(20260930))
	exactDifferent, pairs := 0, 0
	for i := 0; i < 20000; i++ {
		exp := randDoc(r, 3)
		act := exp
		for m := r.Intn(3); m >= 0; m-- {
			act = mutate(r, act)
		}
		noise := randNoise(r, exp)
		if r.Intn(4) == 0 {
			noise = randNoise(r, act)
		}
		eb, _ := json.Marshal(exp)
		ab, _ := json.Marshal(act)
		es, as := string(eb), string(ab)
		// Round-trip, so both sides are exactly what json.Unmarshal yields.
		e, a := parseDoc(t, es), parseDoc(t, as)
		pairs++
		exact := exactByStrings(es, as, noise)
		if exactParsed(e, a, noise) != exact {
			t.Fatalf("NewValidatedJSON disagrees with ValidateAndMarshalJSON on %s vs %s noise %v", es, as, noise)
		}
		if exact {
			if es != as {
				exactDifferent++
			}
			if JSONMatchKey(e, noise) != JSONMatchKey(a, noise) {
				t.Fatalf("exact under noise %v, but the keys differ:\n exp %s\n act %s", noise, es, as)
			}
		}
		want := len(JSONFieldDiffs(es, as, noise, "body.", 0))
		if got := CountJSONFieldDiffs(CollectJSONFields(e, noise), CollectJSONFields(a, noise)); got != want {
			t.Fatalf("count %d, JSONFieldDiffs %d:\n exp %s\n act %s\n noise %v", got, want, es, as, noise)
		}
	}
	if exactDifferent < 500 {
		t.Fatalf("only %d of %d pairs were different documents the matcher found exact; the key property went untested", exactDifferent, pairs)
	}
	t.Logf("%d pairs, %d different-but-exact", pairs, exactDifferent)
}

// Bytes is what the collected fields take: 16 bytes each, one per leaf.
func TestJSONFieldsBytes(t *testing.T) {
	if n := unsafe.Sizeof(fieldHash{}); n != 16 {
		t.Fatalf("a field takes %d bytes, Bytes counts 16", n)
	}
	f := CollectJSONFields(parseDoc(t, `{"a":1,"b":{"c":"x","d":[1,2]},"e":null}`), nil)
	if got, want := f.Bytes(), 16*len(f.fields); got != want || len(f.fields) == 0 {
		t.Fatalf("Bytes() = %d for %d fields, want %d", got, len(f.fields), want)
	}
	if (JSONFields{}).Bytes() != 0 {
		t.Fatal("no fields take memory")
	}
}
