package learn

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// body builds a shape from a JSON body under key, with optional extra leaves.
func body(t *testing.T, key, js string) Shape {
	t.Helper()
	s := Shape{Key: key, Fixed: map[string]string{"body": "json"}, Leaves: map[string]Leaf{}}
	v, ok := DecodeJSON([]byte(js))
	if !ok {
		t.Fatalf("not JSON: %s", js)
	}
	s.Body = v
	JSONLeaves("body", v, s.Leaves)
	return s
}

// known excuses the leaves whose noise paths it lists, typed.
type known map[string]bool

func (k known) Excused(l Leaf) (bool, bool) { return k[l.Noise], true }

// declared excuses the leaves whose noise paths it lists, untyped.
type declared map[string]bool

func (k declared) Excused(l Leaf) (bool, bool) { return k[l.Noise], false }

func TestJSONLeavesDialects(t *testing.T) {
	s := body(t, "k", `{"a":{"b":1},"items":[{"id":"x"},{"id":"y"}],"e":{},"n":null,"big":12345678901234567890,"d.ot":1,"z":[]}`)
	want := map[string]Leaf{
		"body.a.b":         {Path: "body.a.b", Noise: "body.a.b", Type: Number, Value: "1"},
		"body.items[0].id": {Path: "body.items[0].id", Noise: "body.items[].id", Type: String, Value: "x"},
		"body.items[1].id": {Path: "body.items[1].id", Noise: "body.items[].id", Type: String, Value: "y"},
		"body.e":           {Path: "body.e", Noise: "body.e", Type: EmptyObj},
		"body.n":           {Path: "body.n", Noise: "body.n", Type: Null},
		"body.big":         {Path: "body.big", Noise: "body.big", Type: Number, Value: "12345678901234567890"},
		"body.d.ot":        {Path: "body.d.ot", Noise: "body.d.ot", Type: Number, Value: "1", Unnamed: true},
		"body.z":           {Path: "body.z", Noise: "body.z", Type: EmptyArray},
	}
	if !reflect.DeepEqual(s.Leaves, want) {
		t.Fatalf("leaves:\n got %+v\nwant %+v", s.Leaves, want)
	}
	root := body(t, "k", `[{"id":1}]`)
	if l := root.Leaves["body[0].id"]; l.Noise != "body.[].id" {
		t.Fatalf("a root array's element field is %q in the request matcher's dialect, want body.[].id", l.Noise)
	}
}

func TestCompare(t *testing.T) {
	rec := body(t, "k", `{"id":"a","n":1,"opt":"x"}`)
	for _, tc := range []struct {
		name       string
		live       string
		known      Known
		structural string
		drift      []string
	}{
		{"same", `{"id":"a","n":1,"opt":"x"}`, nil, "", nil},
		{"a value drifted", `{"id":"b","n":1,"opt":"x"}`, nil, "", []string{"body.id"}},
		{"a type changed", `{"id":"a","n":"1","opt":"x"}`, nil, "body.n changed type", nil},
		{"a field went missing", `{"id":"a","n":1}`, nil, "body.opt is missing", nil},
		{"a field was added", `{"id":"a","n":1,"opt":"x","new":2}`, nil, "body.new is not in the recording", nil},
		{"known noise is not a drift", `{"id":"b","n":1,"opt":"x"}`, known{"body.id": true}, "", nil},
		{"known noise keeps its type", `{"id":7,"n":1,"opt":"x"}`, known{"body.id": true}, "body.id changed type", nil},
		{"known noise must be present", `{"n":1,"opt":"x"}`, known{"body.id": true}, "body.id is missing", nil},
		{"declared noise may be absent", `{"n":1,"opt":"x"}`, declared{"body.id": true}, "", nil},
		{"declared noise may change type", `{"id":7,"n":1,"opt":"x"}`, declared{"body.id": true}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Compare(body(t, "k", tc.live), rec, tc.known)
			if d.Structural != tc.structural || !reflect.DeepEqual(d.Drift, tc.drift) {
				t.Fatalf("got structural %q drift %v, want %q %v", d.Structural, d.Drift, tc.structural, tc.drift)
			}
		})
	}
	t.Run("a fixed part differs", func(t *testing.T) {
		live := body(t, "k", `{"id":"a","n":1,"opt":"x"}`)
		live.Fixed["media type"] = "text/plain"
		if d := Compare(live, rec, nil); d.Structural != "media type differs" {
			t.Fatalf("structural %q", d.Structural)
		}
	})
	t.Run("an unnamed leaf cannot drift", func(t *testing.T) {
		d := Compare(body(t, "k", `{"a.b":2}`), body(t, "k", `{"a.b":1}`), nil)
		if !strings.Contains(d.Structural, "no path names it alone") {
			t.Fatalf("structural %q", d.Structural)
		}
	})
}

func cand(s Shape, pairable bool) Candidate {
	return Candidate{Shape: s, Known: None, Pairable: pairable}
}

func TestPair(t *testing.T) {
	t.Run("exact first, whatever comes before it", func(t *testing.T) {
		live := body(t, "k", `{"id":"a"}`)
		r := Pair(live, []Candidate{cand(body(t, "k", `{"id":"b"}`), true), cand(body(t, "k", `{"id":"a"}`), false)})
		if r.Outcome != Exact || r.Index != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the unique closest is paired", func(t *testing.T) {
		live := body(t, "k", `{"id":"z","x":1,"y":2}`)
		r := Pair(live, []Candidate{
			cand(body(t, "k", `{"id":"a","x":9,"y":9}`), true),
			cand(body(t, "k", `{"id":"b","x":1,"y":2}`), true),
		})
		if r.Outcome != Paired || r.Index != 1 || !reflect.DeepEqual(r.Diff.Drift, []string{"body.id"}) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a call recorded twice pairs with the first", func(t *testing.T) {
		live := body(t, "k", `{"id":"z","x":1}`)
		r := Pair(live, []Candidate{cand(body(t, "k", `{"id":"a","x":1}`), true), cand(body(t, "k", `{"id":"a","x":1}`), true)})
		if r.Outcome != Paired || r.Index != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("calls that drift in the same place pair with the first, in recorded order", func(t *testing.T) {
		live := body(t, "k", `{"id":"z","x":1}`)
		r := Pair(live, []Candidate{cand(body(t, "k", `{"id":"a","x":1}`), true), cand(body(t, "k", `{"id":"b","x":1}`), true)})
		if r.Outcome != Paired || r.Index != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("an equal tie in different places is request_changed", func(t *testing.T) {
		live := body(t, "k", `{"id":"z","x":1}`)
		r := Pair(live, []Candidate{cand(body(t, "k", `{"id":"a","x":1}`), true), cand(body(t, "k", `{"id":"z","x":2}`), true)})
		if r.Outcome != Miss || r.Reason != models.MissRequestChanged {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("no candidate with the key is unmocked_call", func(t *testing.T) {
		r := Pair(body(t, "k", `{}`), []Candidate{cand(body(t, "other", `{}`), true)})
		if r.Outcome != Miss || r.Reason != models.MissUnmockedCall || r.Index != -1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("only a change of shape is request_changed, naming it", func(t *testing.T) {
		r := Pair(body(t, "k", `{"id":"a","new":1}`), []Candidate{cand(body(t, "k", `{"id":"a"}`), true)})
		if r.Outcome != Miss || r.Reason != models.MissRequestChanged || r.Index != 0 || !strings.Contains(r.Why, "body.new") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a candidate that is not pairable is only matched exactly", func(t *testing.T) {
		r := Pair(body(t, "k", `{"id":"z"}`), []Candidate{cand(body(t, "k", `{"id":"a"}`), false)})
		if r.Outcome != Miss || r.Reason != models.MissRequestChanged {
			t.Fatalf("%+v", r)
		}
	})
	// The golden of design v14 §7.2: a map-ordered list that came back in
	// another order, with one leaf of each element drifting (expires_in moved
	// at F's +61.5 s). No element equals any recorded one, so a permutation
	// test sees no reorder; the order rule does, and the request is refused.
	t.Run("golden: reorder plus one drifting leaf per element is unstable_order", func(t *testing.T) {
		rec := body(t, "k", `{"tokens":[{"id":"a","expires_in":100},{"id":"b","expires_in":200}]}`)
		live := body(t, "k", `{"tokens":[{"id":"b","expires_in":201},{"id":"a","expires_in":101}]}`)
		r := Pair(live, []Candidate{cand(rec, true)})
		if r.Outcome != Miss || r.Reason != models.MissUnstableOrder || !reflect.DeepEqual(r.Diff.Reordered, []string{"body.tokens"}) {
			t.Fatalf("%+v", r)
		}
		if !strings.Contains(r.Why, "body.tokens has the recorded elements in another order") {
			t.Fatalf("why %q", r.Why)
		}
	})
	t.Run("the same list in its order with one leaf drifting per element is paired", func(t *testing.T) {
		rec := body(t, "k", `{"tokens":[{"id":"a","expires_in":100},{"id":"b","expires_in":200}]}`)
		live := body(t, "k", `{"tokens":[{"id":"a","expires_in":101},{"id":"b","expires_in":201}]}`)
		r := Pair(live, []Candidate{cand(rec, true)})
		if r.Outcome != Paired || !reflect.DeepEqual(r.Diff.Drift, []string{"body.tokens[0].expires_in", "body.tokens[1].expires_in"}) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a leaf noise excuses casts no vote on the order", func(t *testing.T) {
		// The timestamps tie each element to its own index; excused, they
		// cannot hide that the ids came back reordered.
		rec := body(t, "k", `{"l":[{"id":"a","ts":1},{"id":"b","ts":2}]}`)
		live := body(t, "k", `{"l":[{"id":"b","ts":1},{"id":"a","ts":2}]}`)
		r := Pair(live, []Candidate{{Shape: rec, Known: known{"body.l[].ts": true}, Pairable: true}})
		if r.Outcome != Miss || r.Reason != models.MissUnstableOrder {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("an exact match reordered is unstable_order, not exact", func(t *testing.T) {
		r := Pair(body(t, "k", `{"l":[2,1]}`), []Candidate{cand(body(t, "k", `{"l":[1,2]}`), true)})
		if r.Outcome != Miss || r.Reason != models.MissUnstableOrder {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a closer candidate that is not pairable is a miss, not a farther pairing", func(t *testing.T) {
		live := body(t, "k", `{"user":"alice","ts":9}`)
		r := Pair(live, []Candidate{
			cand(body(t, "k", `{"user":"bob","ts":2}`), true),    // the test's own: user and ts drift
			cand(body(t, "k", `{"user":"alice","ts":1}`), false), // the pool's, in a partial record: only ts
		})
		if r.Outcome != Miss || r.Reason != models.MissRequestChanged || r.Index != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("recordings that drift alike pair the first pairable one", func(t *testing.T) {
		live := body(t, "k", `{"id":"z","x":1}`)
		r := Pair(live, []Candidate{cand(body(t, "k", `{"id":"a","x":1}`), false), cand(body(t, "k", `{"id":"b","x":1}`), true)})
		if r.Outcome != Paired || r.Index != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a reorder with other drift is request_changed, not unstable_order", func(t *testing.T) {
		r := Pair(body(t, "k", `{"l":[2,1],"amount":6}`), []Candidate{cand(body(t, "k", `{"l":[1,2],"amount":5}`), true)})
		if r.Outcome != Miss || r.Reason != models.MissRequestChanged || !strings.Contains(r.Why, "body.amount changed too") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the order rule is applied before pairing: the reordered candidate is the closest", func(t *testing.T) {
		live := body(t, "k", `{"l":[2,1],"a":"x","b":"y"}`)
		r := Pair(live, []Candidate{
			cand(body(t, "k", `{"l":[2,1],"a":"p","b":"q"}`), true), // two drifts
			cand(body(t, "k", `{"l":[1,2],"a":"x","b":"y"}`), true), // only the reorder
		})
		if r.Outcome != Miss || r.Reason != models.MissUnstableOrder || r.Index != 1 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestClassify(t *testing.T) {
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	rec := body(t, "k", `{"order":"o-1","n":12345,"tok":"Bearer tok-A","nonce":"n-1","ts":"t-5","tags":["t1","t2"],"empty":"","db":"u-77"}`)
	live := body(t, "k", `{"order":"o-9","n":54321,"tok":"Bearer tok-B","nonce":"n-9","ts":"t-6","tags":["u1","t2"],"empty":"now","db":"u-88"}`)
	resp := map[string]Leaf{}
	v, _ := DecodeJSON([]byte(`{"echo":"n-1","location":"/orders/x/n-1","items":[{"tag":"t1"}]}`))
	JSONLeaves("body", v, resp)
	var respLeaves []Leaf
	for _, l := range resp {
		respLeaves = append(respLeaves, l)
	}
	earlier := []Earlier{
		{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
			// A string here, a number at the drifted leaf: the value flowed
			// whatever its type.
			{Side: Request, Leaf: Leaf{Path: "body.ref", Noise: "body.ref", Type: String, Value: "12345"}, Live: "54321", Place: "body.ref"},
			// F saw this place hold its recorded value: nothing that drifted
			// came from it.
			{Side: Response, Leaf: Leaf{Path: "body.count", Noise: "body.count", Type: Number, Value: "5"}, Live: "5"},
			// F saw another value here than the request sends: the
			// recorded value still came from here, and is asserted.
			{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: "o-1"}, Live: "o-7", Place: "body.id"},
			{Side: Request, Leaf: Leaf{Path: "body.token", Noise: "body.token", Type: String, Value: "tok-A"}, Live: "tok-B", Place: "body.token"},
		}},
	}
	raw := []Raw{
		// A database row the test inserted before: the id came from there.
		{Mock: "pg-3", At: at.Add(-2 * time.Second), Text: `{"query":"INSERT INTO users VALUES ($1)","params":["u-77"]}`},
		// Recorded after the paired call, and not served before it: not
		// earlier.
		{Mock: "pg-9", At: at.Add(time.Second), Text: `{"params":["t-5"]}`},
	}
	got := Classify([]string{"body.db", "body.empty", "body.n", "body.nonce", "body.order", "body.tags[0]", "body.tok", "body.ts"}, live, rec, at, earlier, raw, respLeaves)
	kinds := map[string]Kind{}
	from := map[string]Place{}
	for _, l := range got {
		kinds[l.Recorded.Path] = l.Kind
		from[l.Recorded.Path] = l.From
	}
	want := map[string]Kind{
		"body.order":   KindVariable, // recorded earlier, whatever F saw there
		"body.n":       KindVariable, // a number that appeared earlier as a string
		"body.tok":     KindVariable, // tok-A appeared earlier, and is a token here
		"body.db":      KindVariable, // in an earlier mock of another kind
		"body.nonce":   KindEcho,     // the answer echoes it, whole and as a token
		"body.tags[0]": KindEcho,     // echoed by the answer's items[0].tag
		"body.ts":      KindNoise,    // only a mock recorded after it holds it
		"body.empty":   KindNoise,    // an empty recorded value carries nothing to follow
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	// The place says what binding it needs: the variable place, the value F
	// saw there, that the value was whole there, and that the mock is the
	// test's own.
	if from["body.order"] != (Place{Mock: "mock-1", Side: Request, Path: "body.id", Noise: "body.id", Var: "body.id", Live: "o-7", Whole: true, Own: true}) {
		t.Fatalf("body.order from %+v", from["body.order"])
	}
	if f := from["body.tok"]; f.Whole || f.Path != "body.token" {
		t.Fatalf("body.tok from %+v: a word of a longer value is not whole", f)
	}
	if from["body.db"] != (Place{Mock: "pg-3"}) {
		t.Fatalf("body.db from %+v: a raw mock's place has no leaf", from["body.db"])
	}
}

func TestRawOfLeavesOutWhereAndWhen(t *testing.T) {
	m := &models.Mock{Name: "m", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"connID": "77777"},
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/v1/items/4242", Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`},
		ReqTimestampMock: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}
	r := RawOf(m, true)
	if !ContainsToken(r.Text, "4242") || ContainsToken(r.Text, "77777") || strings.Contains(r.Text, "2026") {
		t.Fatalf("raw text %s: it holds the payload only", r.Text)
	}
	if !r.Served || r.Mock != "m" {
		t.Fatalf("%+v", r)
	}
}

func TestTokens(t *testing.T) {
	for _, tc := range []struct {
		s, v string
		want bool
	}{
		{"/orders/42", "42", true},
		{"/orders/421", "42", false},
		{"order-42x", "42", false},
		{"42", "42", true},
		{"a 42 b", "42", true},
		{"x-uuid-1-y", "uuid-1", true},
		{"", "42", false},
		{"42", "", false},
	} {
		if got := ContainsToken(tc.s, tc.v); got != tc.want {
			t.Errorf("ContainsToken(%q, %q) = %v", tc.s, tc.v, got)
		}
	}
}

func TestWholeEcho(t *testing.T) {
	r := Leaf{Type: String, Value: "42"}
	for _, tc := range []struct {
		a    Leaf
		want bool
	}{
		{Leaf{Type: String, Value: "42"}, true},
		{Leaf{Type: Number, Value: "42"}, true}, // an id sent as text, answered as a number
		{Leaf{Type: String, Value: "id-42"}, false},
		{Leaf{Type: Null}, false},
		{Leaf{Type: String, Value: "42", Unnamed: true}, false},
	} {
		if got := WholeEcho(r, tc.a); got != tc.want {
			t.Errorf("WholeEcho(%+v) = %v", tc.a, got)
		}
	}
	if WholeEcho(Leaf{Type: String}, Leaf{Type: String}) {
		t.Error("an empty value echoes nothing")
	}
}

func TestVarPlaces(t *testing.T) {
	for _, tc := range []struct {
		leaf  Leaf
		json  bool
		place string
	}{
		{Leaf{Path: "url.path.2"}, false, "url.path[2]"},
		{Leaf{Path: "url.query.page"}, false, "url.query.page"},
		{Leaf{Path: "header.x-request-id"}, false, "header.x-request-id"},
		{Leaf{Path: "body.items[0].id"}, true, "body.items[0].id"},
		{Leaf{Path: `body.a"b`}, true, `body["a\"b"]`},
		{Leaf{Path: "body.id"}, false, ""},                // a form or multipart field
		{Leaf{Path: "body"}, true, ""},                    // a whole body
		{Leaf{Path: "body.a.b", Unnamed: true}, true, ""}, // no path names it alone
	} {
		got, ok := VarPlace(tc.leaf, tc.json)
		if got != tc.place || ok != (tc.place != "") {
			t.Errorf("VarPlace(%+v, %v) = %q %v, want %q", tc.leaf, tc.json, got, ok, tc.place)
			continue
		}
		if !ok {
			continue
		}
		back, ok := LeafPath(got)
		if !ok || back != tc.leaf.Path {
			t.Errorf("LeafPath(%q) = %q %v, want %q", got, back, ok, tc.leaf.Path)
		}
	}
	if p, ok := LeafPath("header.X-Request-Id"); !ok || p != "header.x-request-id" {
		t.Errorf("a header place names the lowercased leaf: %q %v", p, ok)
	}
	if _, ok := LeafPath("nowhere"); ok {
		t.Error("a place that does not parse names no leaf")
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker()
	if tr.Open() {
		t.Fatal("no test is open before the first begins")
	}
	tr.Begin()
	if !tr.Open() {
		t.Fatal("a test is open once it begins")
	}
	n, earlier, mocks := tr.Test()
	if len(earlier) != 0 || len(mocks) != 0 {
		t.Fatal("a new tracker holds nothing")
	}
	tr.Served(n, Earlier{Mock: "a"})
	tr.ServedMock(&models.Mock{Name: "pg-1"})
	if _, e, m := tr.Test(); len(e) != 1 || len(m) != 1 {
		t.Fatal("served is earlier for the same test")
	}
	tr.End()
	if tr.Open() {
		t.Fatal("no test is open once it ends")
	}
	tr.Begin()
	tr.Served(n, Earlier{Mock: "late"}) // the previous test's request ended after the boundary
	if _, e, m := tr.Test(); len(e) != 0 || len(m) != 0 {
		t.Fatalf("a new test starts with nothing earlier, and a previous test's request does not leak into it: %v %v", e, m)
	}
	// Variables are named per test, by where each is bound.
	n, _, _ = tr.Test()
	id, ok := tr.Name(n, "mock-1", "body.id")
	if !ok || id != "id" {
		t.Fatalf("Name = %q %v", id, ok)
	}
	if again, _ := tr.Name(n, "mock-1", "body.id"); again != id {
		t.Fatal("a variable has one name in its test")
	}
	if other, _ := tr.Name(n, "mock-2", "body.order.id"); other != "id-2" {
		t.Fatalf("another variable gets another name: %q", other)
	}
	if seg, _ := tr.Name(n, "mock-2", "url.path[1]"); seg != "path1" {
		t.Fatalf("a URL segment's variable: %q", seg)
	}
	if _, ok := tr.Name(n-1, "mock-3", "body.id"); ok {
		t.Fatal("a test that ended names nothing")
	}
	tr.Begin()
	n, _, _ = tr.Test()
	if fresh, _ := tr.Name(n, "mock-2", "body.order.id"); fresh != "id" {
		t.Fatalf("names are per test: %q", fresh)
	}
	var nilTracker *Tracker
	nilTracker.Begin()
	nilTracker.End()
	nilTracker.Served(0, Earlier{})
	nilTracker.ServedMock(&models.Mock{})
	if name, ok := nilTracker.Name(0, "m", "header.x-request-id"); !ok || name != "x-request-id" {
		t.Fatalf("a nil tracker names a variable by its place: %q %v", name, ok)
	}
	if n, e, m := nilTracker.Test(); n != 0 || e != nil || m != nil || !nilTracker.Open() {
		t.Fatal("a nil tracker is inert, and knows of no test boundary")
	}
}
