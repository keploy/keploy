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
	t.Run("an equal tie of different calls is request_changed", func(t *testing.T) {
		live := body(t, "k", `{"id":"z","x":1}`)
		r := Pair(live, []Candidate{cand(body(t, "k", `{"id":"a","x":1}`), true), cand(body(t, "k", `{"id":"b","x":1}`), true)})
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
	rec := body(t, "k", `{"order":"o-1","nonce":"n-1","ts":5,"tags":["t1","t2"],"empty":""}`)
	live := body(t, "k", `{"order":"o-9","nonce":"n-9","ts":6,"tags":["u1","t2"],"empty":"now"}`)
	resp := map[string]Leaf{}
	v, _ := DecodeJSON([]byte(`{"echo":"n-1","location":"/orders/x/n-1","count":5}`))
	JSONLeaves("body", v, resp)
	var respLeaves []Leaf
	for _, l := range resp {
		respLeaves = append(respLeaves, l)
	}
	earlier := []Earlier{
		{Mock: "mock-1", At: at.Add(-time.Second), Seen: []Seen{
			// Where the order came from: recorded o-1, and F sent o-9 there.
			{Side: Request, Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: "o-1"}, Live: "o-9"},
		}},
		{Mock: "mock-later", At: at.Add(time.Second), Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.ts", Noise: "body.ts", Type: Number, Value: "5"}, Live: "6"},
		}},
		{Mock: "mock-2", At: at.Add(-2 * time.Second), Seen: []Seen{
			// Holds the recorded 5, but F saw another value there: no flow.
			{Side: Response, Leaf: Leaf{Path: "body.count", Noise: "body.count", Type: Number, Value: "5"}, Live: "5"},
		}},
	}
	got := Classify([]string{"body.empty", "body.nonce", "body.order", "body.tags[0]", "body.ts"}, live, rec, at, earlier, respLeaves)
	kinds := map[string]Kind{}
	for _, l := range got {
		kinds[l.Recorded.Path] = l.Kind
	}
	want := map[string]Kind{
		"body.order": KindVariable,    // it flowed from mock-1's request
		"body.nonce": KindCorrelation, // the answer echoes it, whole and as a token
		// The answer's count holds the recorded 5. mock-2 holds it too, but F
		// saw 5 there and sent 6 here, so nothing flowed from it; and
		// mock-later, which F saw send 6, was recorded after this call.
		"body.ts":      KindCorrelation,
		"body.tags[0]": KindNoise, // every element shares its noise path: no correlation can name it
		"body.empty":   KindNoise, // an empty recorded value carries nothing to follow
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	for _, l := range got {
		switch l.Recorded.Path {
		case "body.order":
			if l.From != (Place{Mock: "mock-1", Side: Request, Path: "body.id", Noise: "body.id"}) {
				t.Fatalf("from %+v", l.From)
			}
		case "body.nonce":
			var paths []string
			for _, e := range l.Echo {
				paths = append(paths, e.Path)
			}
			if !reflect.DeepEqual(paths, []string{"body.echo", "body.location"}) && !reflect.DeepEqual(paths, []string{"body.location", "body.echo"}) {
				t.Fatalf("echo %v", paths)
			}
		}
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
	if got, ok := ReplaceTokens("/v1/orders/1?n=1&m=11", "1", "2"); !ok || got != "/v1/orders/2?n=2&m=11" {
		t.Fatalf("ReplaceTokens = %q %v", got, ok)
	}
}

func TestRenderJSON(t *testing.T) {
	in := "{\n  \"id\": \"n-1\",\n  \"other\": \"n-1\",\n  \"loc\": \"/o/n-1\",\n  \"count\": 7,\n  \"list\": [{\"id\": \"n-1\"}],\n  \"ok\": true\n}"
	out, ok := RenderJSON(in, map[string]bool{"body.id": true, "body.loc": true, "body.list[].id": true}, "n-1", "n-9")
	want := "{\n  \"id\": \"n-9\",\n  \"other\": \"n-1\",\n  \"loc\": \"/o/n-9\",\n  \"count\": 7,\n  \"list\": [{\"id\": \"n-9\"}],\n  \"ok\": true\n}"
	if !ok || out != want {
		t.Fatalf("got %v\n%s\nwant\n%s", ok, out, want)
	}
	// A number is rewritten as a number, only where it is the whole value.
	out, ok = RenderJSON(`{"count":7,"other":7}`, map[string]bool{"body.count": true}, "7", "8")
	if !ok || out != `{"count":8,"other":7}` {
		t.Fatalf("number: %v %s", ok, out)
	}
	// A string live value never turns a number leaf into a string.
	if out, ok := RenderJSON(`{"count":7}`, map[string]bool{"body.count": true}, "7", "x"); ok || out != `{"count":7}` {
		t.Fatalf("type kept: %v %s", ok, out)
	}
	// A root scalar, and a body that is not JSON.
	if out, ok := RenderJSON(`"n-1"`, map[string]bool{"body": true}, "n-1", "n-2"); !ok || out != `"n-2"` {
		t.Fatalf("root: %v %s", ok, out)
	}
	if out, ok := RenderJSON(`not json`, map[string]bool{"body": true}, "json", "x"); ok || out != `not json` {
		t.Fatalf("not json: %v %s", ok, out)
	}
	// HTML characters are not escaped into the answer.
	if out, ok := RenderJSON(`{"u":"<a>n-1</a>"}`, map[string]bool{"body.u": true}, "n-1", "n-2"); !ok || out != `{"u":"<a>n-2</a>"}` {
		t.Fatalf("escape: %v %s", ok, out)
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker(nil)
	n, earlier := tr.Test()
	if len(earlier) != 0 {
		t.Fatal("a new tracker holds nothing")
	}
	tr.Served(n, Earlier{Mock: "a"})
	if _, e := tr.Test(); len(e) != 1 {
		t.Fatal("served is earlier for the same test")
	}
	tr.Begin()
	tr.Served(n, Earlier{Mock: "late"}) // the previous test's request ended after the boundary
	if _, e := tr.Test(); len(e) != 0 {
		t.Fatalf("a new test starts with nothing earlier, and a previous test's request does not leak into it: %v", e)
	}
	if tr.Variables().Follow("m", Leaf{}, Place{}) {
		t.Fatal("without a variable runtime nothing is followed")
	}
	var nilTracker *Tracker
	nilTracker.Begin()
	nilTracker.Served(0, Earlier{})
	if n, e := nilTracker.Test(); n != 0 || e != nil {
		t.Fatal("a nil tracker is inert")
	}
}
