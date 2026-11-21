package matcher

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// testUUID is a canonical UUID distinct for every i.
func testUUID(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }

func TestIsIDKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"3F2504E0-4F89-11D3-9A0C-0305E82C3301", true},  // case is ignored
		{"3f2504e04f89-11d3-9a0c-0305e82c3301-", false}, // 36 long, hyphens misplaced
		{"3f2504e0-4f89-11d3-9a0c-0305e82c330", false},  // one short
		{"3f2504e0-4f89-11d3-9a0c-0305e82c330g", false}, // not hex
		{"3f2504e0x4f89x11d3x9a0cx0305e82c3301", false}, // no hyphens where a UUID has them
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", true},            // ULID
		{"01arz3ndektsv4rrffq69g5fav", true},
		{"81ARZ3NDEKTSV4RRFFQ69G5FAV", false}, // a ULID starts 0-7
		{"01ARZ3NDEKTSV4RRFFQ69G5FAU", false}, // U is not Crockford base 32
		{"01ARZ3NDEKTSV4RRFFQ69G5FAI", false}, // nor I
		{"01ARZ3NDEKTSV4RRFFQ69G5FAL", false}, // nor L
		{"01ARZ3NDEKTSV4RRFFQ69G5FAO", false}, // nor O
		{"01ARZ3NDEKTSV4RRFFQ69G5FA", false},  // 25 long
		{"5f8d0d55b54764421b7156c3", true},    // ObjectId, 24 hex
		{"0123456789abcdef", true},            // 16 hex
		{"0123456789abcde", false},            // 15 hex
		{"abcdefabcdefabcd", false},           // hex letters only: a word, not an id
		{"0123456789abcdeg", false},
		{"1700000000", true},                                // 10 digits
		{"170000000", false},                                // 9 digits
		{"2026", false},                                     // a year
		{"200", false},                                      // a status
		{"20261118", false},                                 // a date
		{"opt_3f2504e0-4f89-11d3-9a0c-0305e82c3301", false}, // contains an id, is a name
		{"traceId", false},
		{"", false},
		{NoiseIDKey, false},
	} {
		if got := IsIDKey(tc.key); got != tc.want {
			t.Errorf("IsIDKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestCollapseIDKeys(t *testing.T) {
	u, v := testUUID(1), testUUID(2)
	for _, tc := range []struct{ path, want string }{
		{"results.options." + u + ".traceId", "results.options.{id}.traceId"},
		{u + ".traceId", "{id}.traceId"},
		{"a." + u + ".b." + v + ".x", "a.{id}.b.{id}.x"},
		{"a." + u, "a.{id}"},
		// The request and risk dialects mark an array after its key.
		{"a." + u + "[].x", "a.{id}[].x"},
		{"a." + u + "[][].x", "a.{id}[][].x"},
		// Ten digits and more are an id; nine are a name.
		{"series.1700000000.v", "series.{id}.v"},
		{"series.170000000.v", "series.170000000.v"},
		// Ordinary keys, numeric ones included, are kept as they are.
		{"data.2026.count", "data.2026.count"},
		{"Results.Options.traceId", "Results.Options.traceId"},
		{"a.results.options.{id}.traceId", "a.results.options.{id}.traceId"},
		{"", ""},
	} {
		got := CollapseIDKeys(tc.path)
		if got != tc.want {
			t.Errorf("CollapseIDKeys(%q) = %q, want %q", tc.path, got, tc.want)
		}
		if again := CollapseIDKeys(got); again != got {
			t.Errorf("CollapseIDKeys is not idempotent on %q: %q", got, again)
		}
	}
}

func TestNamesIDKeys(t *testing.T) {
	for _, tc := range []struct {
		entry string
		want  bool
	}{
		{"options.{id}.traceid", true},
		{"{id}.traceid", true},
		{"options.{id}", true},
		{"options.{id}[].x", true},
		{"options.x{id}.traceid", false}, // part of a name, not the segment
		{"options.{id}x.traceid", false},
		{"options.traceid", false},
	} {
		if got := namesIDKeys(tc.entry); got != tc.want {
			t.Errorf("namesIDKeys(%q) = %v, want %v", tc.entry, got, tc.want)
		}
	}
}

func TestNoiseKeyCovers(t *testing.T) {
	u, v := testUUID(1), testUUID(2)
	for _, tc := range []struct {
		key, entry string
		want       bool
	}{
		{"options." + u + ".traceId", "options.{id}.traceId", true},
		{"options." + v + ".traceId", "OPTIONS.{ID}.TRACEID", true}, // without case
		{"results.options." + u + ".traceId", "options.{id}.traceid", true},
		{"options." + u + ".traceIdSuffix", "options.{id}.traceid", true}, // a substring, as every entry is
		// The segment stands for id keys only.
		{"options.summary.traceId", "options.{id}.traceid", false},
		{"options.2026.traceId", "options.{id}.traceid", false},
		// An entry naming one id names that id only.
		{"options." + u + ".traceId", "options." + u + ".traceid", true},
		{"options." + v + ".traceId", "options." + u + ".traceid", false},
		// A literal id beside the segment is compared collapsed, like for like.
		{"a." + v + ".b." + v + ".x", "a." + u + ".b.{id}.x", true},
		{"options.traceId", "options.traceid", true},
	} {
		if got := NoiseKeyCovers(tc.key, tc.entry); got != tc.want {
			t.Errorf("NoiseKeyCovers(%q, %q) = %v, want %v", tc.key, tc.entry, got, tc.want)
		}
	}
}

func TestNoiseEntryKey(t *testing.T) {
	u := testUUID(7)
	if k, id := NoiseEntryKey("Options." + u + ".TraceId"); k != "options."+u+".traceid" || id {
		t.Fatalf("an entry naming one id is only lowercased: %q, %v", k, id)
	}
	if k, id := NoiseEntryKey("A." + u + ".B.{ID}.X"); k != "a.{id}.b.{id}.x" || !id {
		t.Fatalf("an entry naming id keys is held collapsed: %q, %v", k, id)
	}
}

// parseIDKeyDoc parses a JSON document as a producer hands it to
// CollapseIDKeyedPaths.
func parseIDKeyDoc(t *testing.T, body string) interface{} {
	t.Helper()
	var doc interface{}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
	return doc
}

func TestCollapseIDKeyedPaths(t *testing.T) {
	a, b := testUUID(1), testUUID(2)
	doc := parseIDKeyDoc(t, `{"options":{"`+a+`":{"t":1,"p":1,"q":1},"`+b+`":{"t":1,"p":1,"q":1}},"name":1,"flat":{"x":1},"`+a+`":1}`)
	yes := map[string]bool{
		"options." + a + ".t": true, "options." + b + ".t": true, // qualifies under every id
		"options." + a + ".p": true,                          // under one id only; q under none
		"name":                true, "flat.x": true, a: true, // no id key in a dotted path, and a dot-free id
	}
	asked := map[string]int{}
	got := CollapseIDKeyedPaths(doc, func(p string) bool { asked[p]++; return yes[p] })
	want := []string{a, "flat.x", "name", "options." + a + ".p", "options.{id}.t"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("CollapseIDKeyedPaths = %v, want %v", got, want)
	}
	// Every node is a path, objects included, and each is judged once.
	for _, p := range []string{"options", "options." + a, "flat", "options." + b + ".q"} {
		if asked[p] != 1 {
			t.Errorf("qualifies(%q) asked %d times, want once", p, asked[p])
		}
	}
}

// The one entry may reach only what its per-id paths reach: a path it would
// silence that does not qualify, and that no per-id path silences already,
// keeps the field per id. Each case is checked through the matcher too: the
// returned entries must keep the non-qualifying field asserted.
func TestCollapseIDKeyedPathsReachesNothingNew(t *testing.T) {
	u0, u1, p0, o0 := testUUID(1), testUUID(2), testUUID(3), testUUID(4)
	for _, tc := range []struct {
		name string
		// body is the recorded document, with %s where the non-qualifying
		// field's value goes; time marks the qualifying values.
		body, field string
		want        []string
	}{
		{
			// Another id's field whose name the entry's field begins.
			name:  "another id's longer field",
			body:  `{"options":{"` + u0 + `":{"hold":"time"},"` + u1 + `":{"holdReason":%q}}}`,
			field: "options." + u1 + ".holdReason",
			want:  []string{"options." + u0 + ".hold"},
		},
		{
			// A nested id map whose collapsed path holds the entry.
			name:  "nested id map",
			body:  `{"options":{"` + u0 + `":{"ts":"time"},"` + u1 + `":{"ts":"time"}},"packages":{"` + p0 + `":{"options":{"` + o0 + `":{"ts":%q}}}}}`,
			field: "packages." + p0 + ".options." + o0 + ".ts",
			want:  []string{"options." + u0 + ".ts", "options." + u1 + ".ts"},
		},
		{
			// A sibling id map whose name ends with the entry's map name.
			name:  "sibling id map",
			body:  `{"options":{"` + u0 + `":{"ts":"time"},"` + u1 + `":{"ts":"time"}},"addonOptions":{"` + o0 + `":{"ts":%q}}}`,
			field: "addonOptions." + o0 + ".ts",
			want:  []string{"options." + u0 + ".ts", "options." + u1 + ".ts"},
		},
		{
			// An id map at the root: "{id}.ts" is inside "meta.{id}.ts".
			name:  "root id map",
			body:  `{"` + u0 + `":{"ts":"time"},"` + u1 + `":{"ts":"time"},"meta":{"` + o0 + `":{"ts":%q}}}`,
			field: "meta." + o0 + ".ts",
			want:  []string{u0 + ".ts", u1 + ".ts"},
		},
		{
			// A key spelled "{id}" is read as an id key by an entry naming
			// id keys, so the entry reaches it too.
			name:  "a key spelled {id}",
			body:  `{"options":{"` + u0 + `":{"ts":"time"},"{id}":{"ts":%q}}}`,
			field: "options.{id}.ts",
			want:  []string{"options." + u0 + ".ts"},
		},
		{
			// An id-level entry reaching an object under another id.
			name:  "id-level entry over an object",
			body:  `{"slots":{"` + u0 + `":"time","` + u1 + `":{"label":%q}}}`,
			field: "slots." + u1 + ".label",
			want:  []string{"slots." + u0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := func(v string) string {
				return strings.ReplaceAll(fmt.Sprintf(tc.body, v), `"time"`, `"2026-11-19T10:00:00Z"`)
			}
			rec := body("label-a")
			got := CollapseIDKeyedPaths(parseIDKeyDoc(t, rec), func(p string) bool { return isLeafTime(t, rec, p) })
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("CollapseIDKeyedPaths = %v, want %v", got, tc.want)
			}
			if bodyMatches(t, rec, body("label-b"), noiseFor(got)) {
				t.Fatalf("%s label-a -> label-b passes with %v: the entries reach a field that does not qualify", tc.field, got)
			}
		})
	}
}

// isLeafTime reports whether the document holds the test's time value at p.
func isLeafTime(t *testing.T, body, p string) bool {
	t.Helper()
	v := parseIDKeyDoc(t, body)
	for _, seg := range strings.Split(p, ".") {
		m, ok := v.(map[string]interface{})
		if !ok {
			return false
		}
		v = m[seg]
	}
	s, ok := v.(string)
	return ok && s == "2026-11-19T10:00:00Z"
}

// A node the flattened leaves do not show still counts: an empty object, an
// empty array or a null under another id is silenced by the one entry and by
// no per-id one, so the field stays per id.
func TestCollapseIDKeyedPathsCountsEmptyAndNullNodes(t *testing.T) {
	u0, u1 := testUUID(1), testUUID(2)
	for _, other := range []string{`{}`, `[]`, `null`, `[{}]`} {
		t.Run(other, func(t *testing.T) {
			rec := `{"options":{"` + u0 + `":{"hold":"t"},"` + u1 + `":{"hold":` + other + `}}}`
			got := CollapseIDKeyedPaths(parseIDKeyDoc(t, rec), func(p string) bool { return p == "options."+u0+".hold" })
			if want := []string{"options." + u0 + ".hold"}; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("CollapseIDKeyedPaths = %v, want %v", got, want)
			}
		})
	}
}

// What a per-id path silences already is no reason to refuse: "options.<u0>.hold"
// reaches "options.<u0>.holdReason" by itself, and, as a substring, the same
// id's field under "wrap.options", so the one entry adds nothing there, and
// the field is written once.
func TestCollapseIDKeyedPathsAllowsWhatMembersReach(t *testing.T) {
	u0, u1 := testUUID(1), testUUID(2)
	for _, rec := range []string{
		`{"options":{"` + u0 + `":{"hold":"t","holdReason":"r"},"` + u1 + `":{"hold":"t"}}}`,
		`{"options":{"` + u0 + `":{"hold":"t"},"` + u1 + `":{"hold":"t"}},"wrap":{"options":{"` + u0 + `":{"holdReason":"r"}}}}`,
		// Compared without case, as the matcher compares.
		`{"options":{"` + u0 + `":{"hold":"t","HoldReason":"r"},"` + u1 + `":{"hold":"t"}}}`,
	} {
		got := CollapseIDKeyedPaths(parseIDKeyDoc(t, rec), func(p string) bool { return strings.HasSuffix(p, ".hold") && !strings.HasPrefix(p, "wrap.") })
		if want := []string{"options.{id}.hold"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: CollapseIDKeyedPaths = %v, want %v", rec, got, want)
		}
	}
}

// A dot-free path is a global field name, matched at any depth: an id key that
// is the whole path is returned as it is, never as a bare "{id}", which would
// name every id key anywhere.
func TestCollapseIDKeyedPathsNeverCollapsesADotFreeKey(t *testing.T) {
	a, b := testUUID(1), testUUID(2)
	got := CollapseIDKeyedPaths(parseIDKeyDoc(t, `{"`+a+`":"t","`+b+`":"t"}`), func(string) bool { return true })
	if want := []string{a, b}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("CollapseIDKeyedPaths = %v, want %v", got, want)
	}
}

// idKeyFill spells U1..U4, A1, C1, C2 as UUIDs.
var idKeyFill = strings.NewReplacer("U1", testUUID(11), "U2", testUUID(12), "U3", testUUID(13), "U4", testUUID(14),
	"A1", testUUID(15), "C1", testUUID(16), "C2", testUUID(17))

// perIDNoise is the entry naming id keys spelled once per node of the recorded
// document exp that it reaches: the per-id entries a producer writes for the
// same fields (exp's own paths, so one for each id it has).
func perIDNoise(t *testing.T, exp string, entry string, patterns []string) map[string][]string {
	t.Helper()
	var doc interface{}
	if err := json.Unmarshal([]byte(exp), &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	var walk func(p string, v interface{})
	walk = func(p string, v interface{}) {
		if p != "" && NoiseKeyCovers(p, entry) {
			out[p] = patterns
		}
		switch v := v.(type) {
		case map[string]interface{}:
			for k, c := range v {
				walk(childNoisePath(p, k), c)
			}
		case []interface{}:
			for _, c := range v {
				walk(p, c)
			}
		}
	}
	walk("", doc)
	return out
}

// An entry naming id keys excuses, of the keys the recording has, what the
// per-id entries for them would: a value that moved, a member a replay dropped.
// A key the recording lacks it never excuses: a new id, a field appearing under
// an id that did not carry it, under another id map its path also reaches, or
// under an id map nested in another. Each row is judged with the entry and
// with its per-id spelling, and must get the same verdict from both.
func TestIDKeyEntryExcusesNoKeyTheRecordingLacks(t *testing.T) {
	nested := `{"U1":{"ts":"a","children":{"C1":{"ts":"a","name":"x"}}},"U2":{"ts":"a"}}`
	for _, r := range []struct {
		name, entry, rec, rep string
		exact                 bool
	}{
		{"values move", "options.{id}.hold", `{"options":{"U1":{"hold":"a"},"U2":{"hold":"a"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"hold":"b"},"U2":{"hold":"c"},"U3":{"price":5}}}`, true},
		{"a new id", "lastSeen.{id}", `{"lastSeen":{"U1":"a","U2":"a"}}`, `{"lastSeen":{"U1":"b","U2":"b","U3":"b"}}`, false},
		{"a new id holding an object", "options.{id}", `{"options":{"U1":{"x":1}}}`, `{"options":{"U1":{"x":2},"U2":{"x":1}}}`, false},
		{"the field under an id that lacked it", "options.{id}.hold", `{"options":{"U1":{"hold":"a"},"U2":{"hold":"a"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"hold":"b"},"U2":{"hold":"b"},"U3":{"price":5,"hold":"b"}}}`, false},
		{"the field under another id map", "options.{id}.ts", `{"options":{"U1":{"ts":"a"}},"addonOptions":{"A1":{"label":"x"}}}`,
			`{"options":{"U1":{"ts":"b"}},"addonOptions":{"A1":{"label":"x","ts":"b"}}}`, false},
		{"root id map: a nested id map gains the field", "{id}.ts", `{"U1":{"ts":"a","children":{"C1":{"name":"x"}}},"U2":{"ts":"a"}}`,
			`{"U1":{"ts":"b","children":{"C1":{"name":"x","ts":"b"}}},"U2":{"ts":"b"}}`, false},
		{"a dropped id", "lastSeen.{id}", `{"lastSeen":{"U1":"a","U2":"a"}}`, `{"lastSeen":{"U1":"b"}}`, true},
		{"the field dropped under an id", "options.{id}.hold", `{"options":{"U1":{"hold":"a"},"U2":{"hold":"a"}}}`,
			`{"options":{"U1":{"hold":"b"},"U2":{}}}`, true},
		{"a member present at record, changed whole", "options.{id}", `{"options":{"U1":{"x":1}}}`, `{"options":{"U1":{"x":2,"y":3}}}`, true},
		{"nested id maps: values move", "{id}.ts", nested, `{"U1":{"ts":"b","children":{"C1":{"ts":"b","name":"x"}}},"U2":{"ts":"b"}}`, true},
		{"nested id maps: the field dropped under the nested id", "{id}.ts", nested,
			`{"U1":{"ts":"b","children":{"C1":{"name":"x"}}},"U2":{"ts":"b"}}`, true},
		{"nested id maps: a new nested id", "{id}.ts", nested,
			`{"U1":{"ts":"b","children":{"C1":{"ts":"b","name":"x"},"C2":{"ts":"b"}}},"U2":{"ts":"b"}}`, false},
		{"nested id maps: an outer id gains a nested map", "{id}.ts", nested,
			`{"U1":{"ts":"b","children":{"C1":{"ts":"b","name":"x"}}},"U2":{"ts":"b","children":{"C2":{"ts":"b"}}}}`, false},
		{"nested id maps: a new outer id", "{id}.ts", nested,
			`{"U1":{"ts":"b","children":{"C1":{"ts":"b","name":"x"}}},"U2":{"ts":"b"},"U3":{"ts":"b"}}`, false},
	} {
		t.Run(r.name, func(t *testing.T) {
			rec, rep := idKeyFill.Replace(r.rec), idKeyFill.Replace(r.rep)
			if got := jsonDiffExact(t, rec, rep, map[string][]string{r.entry: {}}); got != r.exact {
				t.Errorf("exact=%v with %s, want %v", got, r.entry, r.exact)
			}
			perID := perIDNoise(t, rec, r.entry, []string{})
			if got := jsonDiffExact(t, rec, rep, perID); got != r.exact {
				t.Errorf("exact=%v with the per-id entries %v, want %v", got, perID, r.exact)
			}
		})
	}
}

// Only an entry naming id keys is passed over for an added key: an entry
// without the segment, beside it or alone, excuses the key as it always has,
// and so does a global one. Containment allows an added member whatever the
// noise.
func TestIDKeyEntryLeavesOtherEntriesOnAnAddedKey(t *testing.T) {
	rec := idKeyFill.Replace(`{"lastSeen":{"U1":"a","U2":"a"}}`)
	rep := idKeyFill.Replace(`{"lastSeen":{"U1":"b","U2":"b","U3":"b"}}`)
	for _, noise := range []map[string][]string{
		{"lastSeen.{id}": {}, "lastSeen." + testUUID(13): {}},
		{"lastSeen.{id}": {}, "lastSeen": {}},
		{"lastSeen.{id}": {}, testUUID(13): {}},
	} {
		if !jsonDiffExact(t, rec, rep, noise) {
			t.Errorf("an entry without the segment no longer excuses the added key under %v", noise)
		}
	}
	vj, err := ValidateAndMarshalJSON(zap.NewNop(), &rec, &rep)
	if err != nil {
		t.Fatal(err)
	}
	if contains, decided, _ := JSONContainsWithNoiseControl(vj, map[string][]string{"lastSeen.{id}": {}}, false, nil); !decided || !contains {
		t.Errorf("containment refuses an added member: contains=%v decided=%v", contains, decided)
	}
}

// Property: whatever a random pair of documents differs by, an entry naming
// id keys finds them exact only if its per-id spelling over the recorded
// document does: it excuses nothing an entry per recorded id would not. The
// documents hold maps keyed by ids at several depths, and the changes add and
// drop ids and fields under them. The run must see enough exact pairs that
// differ in their keys to mean something.
func TestIDKeyEntryPropertyExcusesNoMoreThanPerIDEntries(t *testing.T) {
	r := rand.New(rand.NewSource(20261119))
	ids := []string{testUUID(21), testUUID(22), testUUID(23), testUUID(24)}
	names := []string{"ts", "hold", "label", "options"}
	var gen func(depth int) interface{}
	gen = func(depth int) interface{} {
		if depth <= 0 || r.Intn(5) == 0 {
			return fmt.Sprintf("v%d", r.Intn(2))
		}
		m := map[string]interface{}{}
		keys := names
		if r.Intn(2) == 0 {
			keys = ids
		}
		for i := r.Intn(3); i >= 0; i-- {
			m[keys[r.Intn(len(keys))]] = gen(depth - 1)
		}
		return m
	}
	var mutate func(v interface{}) interface{}
	mutate = func(v interface{}) interface{} {
		m, ok := v.(map[string]interface{})
		if !ok {
			return fmt.Sprintf("v%d", r.Intn(3))
		}
		out := map[string]interface{}{}
		var keys []string
		for k, e := range m {
			out[k] = e
			keys = append(keys, k)
		}
		sort.Strings(keys)
		switch {
		case len(keys) == 0 || r.Intn(4) == 0:
			pool := names
			if r.Intn(2) == 0 {
				pool = ids
			}
			out[pool[r.Intn(len(pool))]] = gen(1)
		case r.Intn(5) == 0:
			delete(out, keys[r.Intn(len(keys))])
		default:
			k := keys[r.Intn(len(keys))]
			out[k] = mutate(out[k])
		}
		return out
	}
	exactDifferentKeys := 0
	for i := 0; i < 20000; i++ {
		exp := gen(4)
		act := exp
		for n := r.Intn(2); n >= 0; n-- {
			act = mutate(act)
		}
		// The entry: a node of exp under an id key, spelled with its id keys
		// collapsed, sometimes from a later segment on.
		var under []string
		var walk func(p string, v interface{})
		walk = func(p string, v interface{}) {
			if c := CollapseIDKeys(p); strings.Contains(c, NoiseIDKey) && strings.Contains(c, ".") {
				under = append(under, c)
			}
			if m, ok := v.(map[string]interface{}); ok {
				for k, e := range m {
					walk(childNoisePath(p, k), e)
				}
			}
		}
		walk("", exp)
		if len(under) == 0 {
			continue
		}
		sort.Strings(under)
		entry := under[r.Intn(len(under))]
		if segs := strings.Split(entry, "."); len(segs) > 2 && r.Intn(3) == 0 {
			if later := strings.Join(segs[1:], "."); namesIDKeys(later) && strings.Contains(later, ".") {
				entry = later
			}
		}
		var patterns []string
		if r.Intn(3) == 0 {
			patterns = []string{"^v[01]$"}
		}
		eb, _ := json.Marshal(exp)
		ab, _ := json.Marshal(act)
		es, as := string(eb), string(ab)
		if !jsonDiffExact(t, es, as, map[string][]string{entry: patterns}) {
			continue
		}
		perID := perIDNoise(t, es, entry, patterns)
		if !jsonDiffExact(t, es, as, perID) {
			t.Fatalf("exact under %s %v, not under its per-id entries %v:\n exp %s\n act %s", entry, patterns, perID, es, as)
		}
		if keysOf(t, es) != keysOf(t, as) {
			exactDifferentKeys++
		}
	}
	if exactDifferentKeys < 200 {
		t.Fatalf("only %d exact pairs differed in their keys; the property went untested", exactDifferentKeys)
	}
	t.Logf("%d exact pairs differed in their keys", exactDifferentKeys)
}

// keysOf is every member path of a document, sorted and joined.
func keysOf(t *testing.T, doc string) string {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	eachNodePath(v, "", func(p string) { out = append(out, p) })
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// Deriving noise reads the case's existing entries as the matcher does: one
// naming id keys does not excuse a key the replay added, so that key is a
// structural refusal, not something already ignored.
func TestBodyNoiseFromJSONDiff_IDKeyEntryDoesNotExcuseAnAddedKey(t *testing.T) {
	rec := idKeyFill.Replace(`{"lastSeen":{"U1":"a","U2":"a"},"trace":"t1"}`)
	rep := idKeyFill.Replace(`{"lastSeen":{"U1":"b","U2":"b","U3":"b"},"trace":"t2"}`)
	paths, skipped := BodyNoiseFromJSONDiff(rec, rep, map[string][]string{"lastseen.{id}": nil}, false)
	want := NoiseSkip{Path: "lastSeen." + testUUID(13), Reason: NoiseSkipStructural}
	found := false
	for _, s := range skipped {
		found = found || s == want
	}
	if !found {
		t.Fatalf("paths %v, skipped %v: want %v", paths, skipped, want)
	}
	// Nor is that key already ignored when another candidate's entry would
	// reach it: "hol" drifted, and an entry for it would also excuse the
	// "hold" the replay added, which nothing excuses now. So it is refused.
	rec = idKeyFill.Replace(`{"opts":{"U1":{"hol":"a"}}}`)
	rep = idKeyFill.Replace(`{"opts":{"U1":{"hol":"b","hold":"x"}}}`)
	paths, skipped = BodyNoiseFromJSONDiff(rec, rep, map[string][]string{"opts.{id}.hold": nil}, false)
	if len(paths) != 0 {
		t.Fatalf("emitted %v (skipped %v): an entry that also excuses the added hold", paths, skipped)
	}
}

// An entry derived from one replay, whose values all moved, excuses what the
// next replay moves there and nothing it adds: a new id, or the field under
// an id that lacked it.
func TestBodyNoiseFromJSONDiff_DerivedIDKeyEntryAndALaterReplay(t *testing.T) {
	for _, r := range []struct {
		name, rec, first, later string
		exact                   bool
	}{
		{"values move", `{"lastSeen":{"U1":"a1","U2":"a2"}}`, `{"lastSeen":{"U1":"b1","U2":"b2"}}`, `{"lastSeen":{"U1":"c1","U2":"c2"}}`, true},
		{"a new id", `{"lastSeen":{"U1":"a1","U2":"a2"}}`, `{"lastSeen":{"U1":"b1","U2":"b2"}}`, `{"lastSeen":{"U1":"c1","U2":"c2","U3":"c3"}}`, false},
		{"the field under an id that lacked it", `{"options":{"U1":{"trace":"a1"},"U2":{"trace":"a2"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"trace":"b1"},"U2":{"trace":"b2"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"trace":"c1"},"U2":{"trace":"c2"},"U3":{"price":5,"trace":"c3"}}}`, false},
		{"a dropped id", `{"lastSeen":{"U1":"a1","U2":"a2"}}`, `{"lastSeen":{"U1":"b1","U2":"b2"}}`, `{"lastSeen":{"U1":"c1"}}`, true},
	} {
		t.Run(r.name, func(t *testing.T) {
			rec := idKeyFill.Replace(r.rec)
			paths, skipped := BodyNoiseFromJSONDiff(rec, idKeyFill.Replace(r.first), nil, false)
			if len(skipped) != 0 || len(paths) != 1 || !strings.Contains(paths[0], NoiseIDKey) {
				t.Fatalf("derived %v, skipped %v: want one entry naming id keys", paths, skipped)
			}
			if got := jsonDiffExact(t, rec, idKeyFill.Replace(r.later), map[string][]string{paths[0]: {}}); got != r.exact {
				t.Fatalf("a later replay is exact=%v under %v, want %v", got, paths, r.exact)
			}
		})
	}
}

// The failure assessment reads the live document as the matcher does: a key
// the recording lacks, under an entry naming id keys, is an added field, as
// it is under the per-id entries. So a response that only gained an id is
// graded as new fields (low risk), not as "identical".
func TestComputeFailureAssessmentJSON_IDKeyEntryReportsAnAddedKey(t *testing.T) {
	rec := idKeyFill.Replace(`{"lastSeen":{"U1":"a","U2":"a"},"options":{"U1":{"hold":"a"},"U3":{"price":5}}}`)
	rep := idKeyFill.Replace(`{"lastSeen":{"U1":"b","U2":"b","U3":"b"},"options":{"U1":{"hold":"b"},"U3":{"price":5,"hold":"b"}}}`)
	want := []string{"lastSeen." + testUUID(13), "options." + testUUID(13) + ".hold"}
	for _, noise := range []map[string][]string{
		{"lastSeen.{id}": {}, "options.{id}.hold": {}},
		{"lastSeen." + testUUID(11): {}, "lastSeen." + testUUID(12): {}, "options." + testUUID(11) + ".hold": {}},
	} {
		a, err := ComputeFailureAssessmentJSON(rec, rep, noise, false)
		if err != nil || a == nil {
			t.Fatalf("assessment: %v %v", a, err)
		}
		if fmt.Sprint(a.AddedFields) != fmt.Sprint(want) || a.Risk != models.Low || len(a.ValueChanges)+len(a.RemovedFields)+len(a.TypeChanges) != 0 {
			t.Errorf("under %v: added %v risk %s values %v removed %v types %v, want added %v and low risk",
				noise, a.AddedFields, a.Risk, a.ValueChanges, a.RemovedFields, a.TypeChanges, want)
		}
	}
}

// The field diffs list a key the live document added under an entry naming id
// keys, and their count on prepared documents agrees.
func TestJSONFieldDiffs_IDKeyEntryListsAnAddedKey(t *testing.T) {
	rec := idKeyFill.Replace(`{"lastSeen":{"U1":"a","U2":"a"},"items":[{"U1":"a"}]}`)
	rep := idKeyFill.Replace(`{"lastSeen":{"U1":"b","U2":"b","U3":"b"},"items":[{"U1":"b","U2":"b"}]}`)
	// Field diffs read noise in the request matcher's dialect, an array's
	// elements under "[]".
	noise := map[string][]string{"lastSeen.{id}": {}, "items[].{id}": {}}
	diffs := JSONFieldDiffs(rec, rep, noise, "body.", 0)
	var got []string
	for _, d := range diffs {
		got = append(got, string(d.Kind)+" "+d.Path)
	}
	want := []string{string(models.DiffKindMissingInMock) + " body.items[]." + testUUID(12), string(models.DiffKindMissingInMock) + " body.lastSeen." + testUUID(13)}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("diffs %v, want %v", got, want)
	}
	e, a := parseDoc(t, rec), parseDoc(t, rep)
	if n := CountJSONFieldDiffs(CollectJSONFields(e, noise), CollectJSONFields(a, noise)); n != len(diffs) {
		t.Fatalf("count %d, JSONFieldDiffs %d", n, len(diffs))
	}
	if f := CollectJSONFields(e, noise); f.Bytes() <= 0 {
		t.Fatalf("a kept document takes %d bytes", f.Bytes())
	}
	// An array's elements the recording has are recorded nodes too: an
	// element value the entry names may move.
	rec = idKeyFill.Replace(`{"U1":{"tags":["a"]}}`)
	rep = idKeyFill.Replace(`{"U1":{"tags":["b"]}}`)
	if d := JSONFieldDiffs(rec, rep, map[string][]string{"{id}.tags[]": {}}, "body.", 0); len(d) != 0 {
		t.Fatalf("an element the entry names, which the recording has, is listed: %v", d)
	}
}

// The match key leaves out what an entry naming id keys excuses: two documents
// the matcher finds exact under it have equal keys, so a caller that compares
// only pairs with equal keys still compares them.
func TestJSONMatchKey_IDKeyEntry(t *testing.T) {
	rec := idKeyFill.Replace(`{"options":{"U1":{"hold":"a","price":1},"U2":{"hold":"a","price":2}}}`)
	rep := idKeyFill.Replace(`{"options":{"U1":{"hold":"b","price":1},"U2":{"hold":"c","price":2}}}`)
	noise := map[string][]string{"options.{id}.hold": {}}
	if !exactByStrings(rec, rep, noise) {
		t.Fatal("the pair is not exact under the entry")
	}
	if JSONMatchKey(parseDoc(t, rec), noise) != JSONMatchKey(parseDoc(t, rep), noise) {
		t.Fatal("exact under an entry naming id keys, but the keys differ")
	}
	changed := idKeyFill.Replace(`{"options":{"U1":{"hold":"b","price":9},"U2":{"hold":"c","price":2}}}`)
	if JSONMatchKey(parseDoc(t, rec), noise) == JSONMatchKey(parseDoc(t, changed), noise) {
		t.Fatal("a field the entry does not name changed, and the keys are equal")
	}
}

// The request lookup reads an entry naming id keys as the response matcher
// does: it covers a leaf both requests have under any id, and not one only
// the live request has.
func TestNoiseLookup_IDKeyEntryAndAnAddedLeaf(t *testing.T) {
	l := NewNoiseLookup(map[string][]string{"options.{id}.hold": {}, "plain.ts": {}})
	path := "options." + testUUID(1) + ".hold"
	if !l.Covers(path, "x", true) {
		t.Error("the entry does not cover a leaf both requests have")
	}
	if l.CoversAdded(path) {
		t.Error("the entry covers a leaf only the live request has")
	}
	if !l.CoversAdded("plain.ts") {
		t.Error("an entry without the segment no longer covers an added leaf")
	}
	if NewNoiseLookup(map[string][]string{"plain.ts": {"^x$"}}).CoversAdded("plain.ts") {
		t.Error("an entry with patterns covers an added leaf")
	}
}
