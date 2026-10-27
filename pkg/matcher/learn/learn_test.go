package learn

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
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
	// at F's lead). No element equals any recorded one, so a permutation
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
	rec := body(t, "k", `{"order":"o-1","n":12345,"tok":"Bearer tok-A","nonce":"nc-41f2","ts":"t-5","tags":["t1","t2"],"empty":"","db":"u-77","sess":"sess s-71a","at":"2026-10-14 09:59:30","seq":"q-1","file":"7c9e6679.json"}`)
	live := body(t, "k", `{"order":"o-9","n":54321,"tok":"Bearer tok-B","nonce":"nc-9c3e","ts":"t-6","tags":["u1","t2"],"empty":"now","db":"u-88","sess":"sess s-82b","at":"2026-10-14 10:00:31","seq":"q-9","file":"1b4e28ba.json"}`)
	resp := map[string]Leaf{}
	v, _ := DecodeJSON([]byte(`{"echo":"nc-41f2","location":"/orders/x/nc-41f2","items":[{"tag":"t1"}]}`))
	JSONLeaves("body", v, resp)
	var respLeaves []Leaf
	for _, l := range resp {
		respLeaves = append(respLeaves, l)
	}
	earlier := []Earlier{
		{Mock: "mock-1", At: at.Add(-time.Second), Payload: payloadOf("mock-1", at.Add(-time.Second)), Own: true, Seen: []Seen{
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
			// A minted id sent whole, then inside a file name: the part
			// drifted with it.
			{Side: Request, Leaf: Leaf{Path: "body.doc", Noise: "body.doc", Type: String, Value: "7c9e6679"}, Live: "1b4e28ba", Place: "body.doc"},
			// A clock read of its own, a second earlier: F's lead moved
			// it, and body.at, so what they share is no source.
			{Side: Request, Leaf: Leaf{Path: "body.sent", Noise: "body.sent", Type: String, Value: "2026-10-14 09:59:29"}, Live: "2026-10-14 10:00:30", Place: "body.sent"},
		}},
	}
	raw := []Raw{
		// A database row the test inserted before: the id came from there.
		{rRead: true, mock: "pg-3", at: at.Add(-2 * time.Second), rText: `{"query":"INSERT INTO users VALUES ($1)","params":["u-77"]}`},
		// Recorded after the paired call, and not served before it: not
		// earlier.
		{rRead: true, mock: "pg-9", at: at.Add(time.Second), rText: `{"params":["t-5"]}`},
		// A run of a drifted value that drifted counts in a payload as a
		// token there: "71a" of "sess s-71a" is one, a row's minutes are
		// not body.at's run.
		RawOf(&models.Mock{Name: "redis-2", Spec: models.MockSpec{ReqTimestampMock: at.Add(-3 * time.Second), Metadata: map[string]string{"args": "SET sid s-71a"}, GenericRequests: []models.Payload{{Message: []models.OutputBinary{{Data: "s-71a"}}}}}}, false),
		RawOf(&models.Mock{Name: "pg-4", Spec: models.MockSpec{ReqTimestampMock: at.Add(-3 * time.Second), Metadata: map[string]string{"row": "2026-10-14 09:59"}}}, false),
	}
	// The application writes its wall clock of no zone in UTC+10, where the
	// call at midnight UTC is 10:00 and body.at's read just before it.
	dc := clockedDrifts()
	dc.zone = time.FixedZone("AEST", 10*3600)
	got := Classify(dc, []string{"body.db", "body.empty", "body.n", "body.nonce", "body.order", "body.tags[0]", "body.tok", "body.ts", "body.sess", "body.at", "body.seq", "body.file"}, live, rec, at, earlier, raw, respLeaves)
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
		// Only a mock recorded after it holds "t-5", but its changed word
		// "5" appears earlier (mock-1's answer), where it did not drift with
		// it: keploy cannot tell, and refuses rather than learn noise.
		"body.ts":    KindUnaccounted,
		"body.empty": KindNoise,     // an empty recorded value carries nothing to follow
		"body.sess":  KindVariable,  // its run is a token of an earlier store call
		"body.at":    KindClockRead, // a clock read F's lead moved, as it did body.sent
		// Of a payload F saw no live value of, a run is searched alone,
		// however short: "1" of "q-1" is the token of pg-3's "$1". It errs
		// to refusing.
		"body.seq":  KindVariable,
		"body.file": KindVariable, // the id of body.doc, inside a file name
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
	if f := from["body.file"]; f.Whole || f.Path != "body.doc" {
		t.Fatalf("body.file from %+v: a part of a longer value, found where it drifted together", f)
	}
	// A raw mock's place has no leaf: it names the mock, and what was found
	// in which field of its payload; neither mock was served before the
	// call (a reason says so: correction).
	leafless := func(p Place) Place { p.Field, p.Held = "", ""; return p }
	if leafless(from["body.sess"]) != (Place{Mock: "redis-2", unserved: true}) || leafless(from["body.seq"]) != (Place{Mock: "pg-3", unserved: true}) {
		t.Fatalf("body.sess from %+v, body.seq from %+v", from["body.sess"], from["body.seq"])
	}
	if f := from["body.sess"]; f.Field != "spec.genericrequests[0].message[0].data" || f.Held != "71a" {
		t.Fatalf("body.sess from %+v: want its word 71a found in spec.genericrequests[0].message[0].data", f)
	}
	if leafless(from["body.db"]) != (Place{Mock: "pg-3", unserved: true}) {
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
	if !ContainsToken(r.Text(), "4242") || ContainsToken(r.Text(), "77777") || strings.Contains(r.Text(), "2026") {
		t.Fatalf("raw text %s: it holds the payload only", r.Text())
	}
	if !r.Served() || r.Mock() != "m" {
		t.Fatalf("%+v", r)
	}
}

// A Postgres recording's invocation id says which statement, on which
// connection, when and in what order it was recorded: where and when, as the
// spec's timestamps do, never a value that flowed. It is left out of the
// recording's text, and the recording itself is not changed.
func TestRawOfLeavesOutAPostgresInvocationsWhereAndWhen(t *testing.T) {
	inv := "sha256:71318920f30ed480b5cfcb2615d5a23f3d3d0c0104366db7828f0e830c15755d:0:2026-10-21T04:05:09.291827863Z:2"
	m := &models.Mock{Name: "mock-2", Kind: models.PostgresV3, Spec: models.MockSpec{PostgresV3: &models.PostgresV3Spec{
		Type: models.PostgresV3TypeQuery,
		Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "CREATE TABLE IF NOT EXISTS twins (name TEXT PRIMARY KEY)",
			InvocationID:  inv,
		},
	}}}
	r := RawOf(m, false)
	if strings.Contains(r.Text(), "2026") || strings.Contains(r.Text(), "71318920f30e") || !strings.Contains(r.Text(), "CREATE TABLE IF NOT EXISTS twins") {
		t.Fatalf("raw text %q: it holds the payload only", r.Text())
	}
	if m.Spec.PostgresV3.Query.InvocationID != inv {
		t.Fatalf("RawOf changed the recording's invocation id to %q", m.Spec.PostgresV3.Query.InvocationID)
	}
}

// F runs each test FullRoundLead past its recorded instant from when it
// begins, so an app that reads its clock late in F (a loaded box) binds a
// value the lead may not account for. It flowed from nowhere, and with no
// call window it is a read of the clock, compared at replay, however late it
// was read: never refused for the recording beside it, never left noise
// unchecked. Read as payload, a recording's invocation id made it a value
// that flowed from that recording (its date and time words), and the call
// was refused as changed (flaky/request_changed).
func TestALateClockReadIsAReadOfTheClockBesideAPostgresRecordingOfTheSameMinute(t *testing.T) {
	recAt := time.Date(2026, 10, 21, 4, 5, 9, 989222587, time.UTC)
	earlier := &models.Mock{Name: "mock-2", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: recAt.Add(-700 * time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "CREATE TABLE IF NOT EXISTS twins (name TEXT PRIMARY KEY)",
			InvocationID:  "sha256:71318920f30ed480b5cfcb2615d5a23f3d3d0c0104366db7828f0e830c15755d:0:" + recAt.Add(-700*time.Millisecond).Format(time.RFC3339Nano) + ":2",
		}},
	}}
	const lay = "2006-01-02 15:04:05.999999999Z07:00"
	rec := Shape{Leaves: map[string]Leaf{"bind.$3": {Path: "bind.$3", Value: recAt.Format(lay)}}}
	for _, late := range []time.Duration{10 * time.Millisecond, 600 * time.Millisecond, 2500 * time.Millisecond} {
		live := Shape{Leaves: map[string]Leaf{"bind.$3": {Path: "bind.$3", Value: recAt.Add(FullRoundLead + late).Format(lay)}}}
		out := Classify(clockedDrifts(), []string{"bind.$3"}, live, rec, recAt.Add(2*time.Millisecond), nil, []Raw{RawOf(earlier, false)}, nil)
		if len(out) != 1 || out[0].Kind != KindClockRead || out[0].Read == nil {
			t.Fatalf("a clock read %v late: %+v, want a read of the clock, compared at replay", late, out)
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
	n, earlier, mocks, _ := tr.Test()
	if len(earlier) != 0 || len(mocks) != 0 {
		t.Fatal("a new tracker holds nothing")
	}
	tr.Served(n, Earlier{Mock: "a"})
	tr.ServedMock(&models.Mock{Name: "pg-1"})
	if _, e, m, _ := tr.Test(); len(e) != 1 || len(m) != 1 {
		t.Fatal("served is earlier for the same test")
	}
	tr.End()
	tr.ServedMock(&models.Mock{Name: "between"}) // served between two tests
	if tr.Open() {
		t.Fatal("no test is open once it ends")
	}
	tr.Begin()
	tr.Served(n, Earlier{Mock: "late"}) // the previous test's request ended after the boundary
	if _, e, m, before := tr.Test(); len(e) != 0 || len(m) != 0 || len(before) != 2 || before[0].Name != "pg-1" || before[1].Name != "between" {
		t.Fatalf("a new test starts with nothing served in it, a previous test's request does not leak into it, and what was served before it began is kept apart: %v %v %v", e, m, before)
	}
	// Variables are named per test, by where each is bound.
	n, _, _, _ = tr.Test()
	id, ok := tr.Name(n, "mock-1", "body.id", nil)
	if !ok || id != "id" {
		t.Fatalf("Name = %q %v", id, ok)
	}
	if again, _ := tr.Name(n, "mock-1", "body.id", nil); again != id {
		t.Fatal("a variable has one name in its test")
	}
	if other, _ := tr.Name(n, "mock-2", "body.order.id", nil); other != "id-2" {
		t.Fatalf("another variable gets another name: %q", other)
	}
	if seg, _ := tr.Name(n, "mock-2", "url.path[1]", nil); seg != "path1" {
		t.Fatalf("a URL segment's variable: %q", seg)
	}
	if res, _ := tr.Name(n, "mock-3", "body.user.id", map[string]bool{"id-3": true}); res != "id-4" {
		t.Fatalf("a template key's name is taken by no other variable: %q", res)
	}
	if _, ok := tr.Name(n-1, "mock-3", "body.id", nil); ok {
		t.Fatal("a test that ended names nothing")
	}
	tr.Begin()
	n, _, _, _ = tr.Test()
	if fresh, _ := tr.Name(n, "mock-2", "body.order.id", nil); fresh != "id" {
		t.Fatalf("names are per test: %q", fresh)
	}
	var nilTracker *Tracker
	nilTracker.Begin()
	nilTracker.End()
	nilTracker.Served(0, Earlier{})
	nilTracker.ServedMock(&models.Mock{})
	if name, ok := nilTracker.Name(0, "m", "header.x-request-id", nil); !ok || name != "x-request-id" {
		t.Fatalf("a nil tracker names a variable by its place: %q %v", name, ok)
	}
	if n, e, m, b := nilTracker.Test(); n != 0 || e != nil || m != nil || b != nil || !nilTracker.Open() {
		t.Fatal("a nil tracker is inert, and knows of no test boundary")
	}
}

// A drifted value's runs are its maximal stretches of changed words that no
// unchanged word interrupts; values of different makes are one stretch,
// widened to whole words.
func TestRuns(t *testing.T) {
	for _, tc := range []struct {
		rec, live string
		want      []run
	}{
		{"7c9e6679-7425.json", "1b4e28ba-2fa1.json", []run{{"7c9e6679-7425", "1b4e28ba-2fa1"}}},
		{"doc:7c9e6679", "doc:1b4e28ba", []run{{"7c9e6679", "1b4e28ba"}}},
		{"doc-7c9e6679", "doc-1b4e28ba", []run{{"7c9e6679", "1b4e28ba"}}},
		{"a-111-b-222", "a-333-b-444", []run{{"111", "333"}, {"222", "444"}}},
		{"2026-10-14T10:00:41Z", "2026-10-14T10:01:42Z", []run{{"00:41Z", "01:42Z"}}},
		{"1760436041", "1760436102", []run{{"1760436041", "1760436102"}}},
		// Different makes: between the common start and end, with the
		// words it touches.
		{"v1.5", "v15", []run{{"v1.5", "v15"}}},
		{"Bearer abc", "Bearer abc-def", []run{{"abc", "abc-def"}}},
		{"10:00:41Z", "10:00:41.5Z", []run{{"41Z", "41.5Z"}}},
		{"same", "same", nil},
	} {
		if got := runsBy(tc.rec, tc.live, wordByte); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("runs(%q, %q) = %q, want %q", tc.rec, tc.live, got, tc.want)
		}
	}
}

// uuid7 is a version 7 UUID of the instant at, with a random part.
func uuid7(at time.Time, random string) string {
	ms := at.UnixMilli()
	h := fmt.Sprintf("%012x", ms)
	return h[:8] + "-" + h[8:] + "-7" + random[:3] + "-8" + random[3:6] + "-" + random[6:18]
}

// An earlier place is a value's source whole when it held the recorded value
// whole, and in part where the two drifted together: one's run is the
// other's, or they share a changed word, however short. What drifted only
// because F's clock runs FullRoundLead past the recording is no source.
func TestSources(t *testing.T) {
	T1, T2 := "4bf92f3577b34da6a3ce929d0e0e4736", "0af7651916cd43dd8448eb211c80319c"
	at := time.Date(2026, 10, 14, 10, 0, 41, 456e6, time.UTC)
	f := at.Add(FullRoundLead)
	for _, tc := range []struct {
		name                     string
		rec, live, recorded, now string
		found, whole             bool
	}{
		{"whole", "o-1", "o-7", "o-1", "o-9", true, true},
		{"an id, then inside a file name", "7c9e6679", "1b4e28ba", "7c9e6679.json", "1b4e28ba.json", true, false},
		{"an id, then inside a key", "7c9e6679", "1b4e28ba", "doc-7c9e6679", "doc-1b4e28ba", true, false},
		{"a short id, then inside a longer value", "41", "42", "o-41", "o-42", true, false},
		{"inside a header, then whole", "00-" + T1 + "-00f067aa0ba902b7-01", "00-" + T2 + "-b7ad6b7169203331-01", T1, T2, true, false},
		{"whole, then next to a fresh part", T1, T2, "00-" + T1 + "-00f067aa0ba902b7-01", "00-" + T2 + "-b7ad6b7169203331-01", true, false},
		{"a trace id hop to hop, next to a fresh span in both", T1 + ":00f067aa0ba902b7:1", T2 + ":b7ad6b7169203331:1", T1 + ":11f067aa0ba902b8:1", T2 + ":c7ad6b7169203332:1", true, false},
		{"a short id next to a fresh part in both", "o-7c9e6679:1760436041", "o-1b4e28ba:1760436102", "7c9e6679|a1b2", "1b4e28ba|c3d4", true, false},
		{"the recorded run alone is no source", "tok-1", "tok-2", "q-1", "q-9", false, false},
		// Ids of the URL-safe alphabets ('-' and '_' inside) split into
		// another number of words run to run: read as ids too.
		{"a nanoid whole, then next to a timestamp", "V1StGXR8_Z5jdHi6B-myT", "Uakgb7ZzQf3xJvP2mLwRt", "V1StGXR8_Z5jdHi6B-myT:1760436041123", "Uakgb7ZzQf3xJvP2mLwRt:1760436102623", true, false},
		{"a nanoid in a cookie next to a fresh token, then whole", "sid=V1StGXR8_Z5jdHi6B-myT; csrf=Rk2-yq_8Jc1vXo3pQ4mN7w", "sid=Uakgb7ZzQf3xJvP2mLwRt; csrf=aZ9bY8cX7dW6eV5fU4gT3h", "V1StGXR8_Z5jdHi6B-myT", "Uakgb7ZzQf3xJvP2mLwRt", true, false},
		{"a nanoid whole, then in a callback URL next to a timestamp", "V1StGXR8_Z5jdHi6B-myT", "Uakgb7ZzQf3xJvP2mLwRt", "https://app.example/cb?ref=V1StGXR8_Z5jdHi6B-myT&ts=1760436041", "https://app.example/cb?ref=Uakgb7ZzQf3xJvP2mLwRt&ts=1760436102", true, false},
		// A ref next to a fresh standard-base64 part in both ('+', '/' and
		// '=' read in no alphabet): the words both lost and gained.
		{"a ref next to a fresh base64 signature in both", "https://app.example/cb?ref=3f2b8c1e-9a4d-4e7b-8c21-5d6e7f8a9b0c&sig=q3+/Zx9Lk2mN8pQ4rS7tU1vW5xY0zA6bC+dE/fG3hI8=", "https://app.example/cb?ref=a7c4e2d1-0b3f-4a8e-9d12-6f5e4d3c2b1a&sig=Zz9/Yy8+Xx7Ww6Vv5Uu4Tt3Ss2Rr1Qq0Pp/Oo+Nn9Mm=",
			"https://app.example/cb?ref=3f2b8c1e-9a4d-4e7b-8c21-5d6e7f8a9b0c&sig=Hj2Kl/9Mn4+Op6Qr8St0Uv/Wx1Yz3Ab5Cd7Ef+Gh9Ij=", "https://app.example/cb?ref=a7c4e2d1-0b3f-4a8e-9d12-6f5e4d3c2b1a&sig=Kk1Ll2Mm3Nn4Oo5Pp6Qq7Rr8Ss9Tt0Uu/Vv1Ww2Xx3Y=", true, false},
		// An id right after a percent escape: read decoded too.
		{"an id after a percent escape", "uploads/3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c.png", "uploads/a7c4e2d10b3f4a8e9d126f5e4d3c2b1a.png", "https://storage.googleapis.com/storage/v1/b/bkt/o/uploads%2F3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c.png?alt=media", "https://storage.googleapis.com/storage/v1/b/bkt/o/uploads%2Fa7c4e2d10b3f4a8e9d126f5e4d3c2b1a.png?alt=media", true, false},
		{"an id after a backslash escape", "A7K29QXM", "Z3M81XPD", `line one\nA7K29QXM`, `line one\nZ3M81XPD`, true, false},
		{"an id after a percent escape encoded twice", "A7K29QXM", "Z3M81XPD", "next%3D%252Finvites%252FA7K29QXM", "next%3D%252Finvites%252FZ3M81XPD", true, false},
		{"an id after a percent escape beside a stray percent", "A7K29QXM", "Z3M81XPD", "Save 20% now: uploads%2FA7K29QXM", "Save 20% now: uploads%2FZ3M81XPD", true, false},
		{"an id after a backslash escape escaped again", "A7K29QXM", "Z3M81XPD", `{\"a\":\"x\\nA7K29QXM\"}`, `{\"a\":\"x\\nZ3M81XPD\"}`, true, false},
		// An id in upper case in one place and lower case in another.
		{"an id in another case", "3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c", "a7c4e2d10b3f4a8e9d126f5e4d3c2b1a", "REF-3F2B8C1E9A4D4E7B8C215D6E7F8A9B0C", "REF-A7C4E2D10B3F4A8E9D126F5E4D3C2B1A", true, false},
		// An id inside base64 text (a JWT's claims, an OAuth state).
		{"an id inside base64 JSON", "A7K29QXM", "Zq81Lm0X", "state=" + base64.RawURLEncoding.EncodeToString([]byte(`{"ref":"A7K29QXM","n":"x1"}`)), "state=" + base64.RawURLEncoding.EncodeToString([]byte(`{"ref":"Zq81Lm0X","n":"y2"}`)), true, false},
		// An ASCII id inside text of another script with no spaces.
		{"an order number inside CJK text", "您的订单A7K29Q已创建", "您的订单Z3M81X已创建", "订单A7K29Q已发货", "订单Z3M81X已发货", true, false},
		{"an order number inside Japanese text", "ご注文番号A7K29Qを受け付けました", "ご注文番号Z3M81Xを受け付けました", "注文A7K29Qを発送しました", "注文Z3M81Xを発送しました", true, false},
		// A part counts only where the earlier place drifted there too: a
		// list URL whose page and limit stayed, a traceparent's version and
		// flags.
		{"a word the earlier place held unchanged", "https://api.example/items?page=3&limit=7&ts=1760436041", "https://api.example/items?page=3&limit=7&ts=1760436102", "shard-3", "shard-7", false, false},
		{"a traceparent's version and flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "bucket-00", "bucket-01", false, false},
		// Words beyond ASCII drift as words.
		{"an emoji whole", "🦊", "🐼", "🦊", "🐼", true, true},
		{"an emoji inside a longer value", "🦊", "🐼", "hi 🦊", "hi 🐼", true, false},
		{"a Cyrillic word inside a longer value", "лиса", "панда", "to:лиса", "to:панда", true, false},
		// A shared counter word is a value that flowed, however short: it
		// errs to refusing.
		{"a counter and a float", "12.7", "13.8", "seq-7", "seq-8", true, false},

		// Clock reads F's lead moved.
		{"a clock's minutes moved too, not as one run", at.Add(-time.Second).Format(time.RFC1123), f.Add(-time.Second).Format(time.RFC1123), at.Format(time.RFC3339), f.Format(time.RFC3339), false, false},
		{"one second in another precision", at.Format(time.RFC1123), f.Format(time.RFC1123), at.Format(time.RFC3339Nano), f.Format(time.RFC3339Nano), false, false},
		// The same clock value whole, case-folded: a whole repeated value,
		// followed (founder decision), where it cannot be bound refused.
		{"an HTTP date and a lower-case copy of it, across midnight", "Wed, 14 Oct 2026 23:59:41 GMT", "Thu, 15 Oct 2026 00:00:42 GMT", "wed, 14 oct 2026 23:59:41 gmt", "thu, 15 oct 2026 00:00:42 gmt", true, false},
		{"an HTTP date and a lower-case log line holding it, across midnight", "Wed, 14 Oct 2026 23:59:41 GMT", "Thu, 15 Oct 2026 00:00:42 GMT", "at wed, 14 oct 2026 23:59:41 gmt: ok", "at thu, 15 oct 2026 00:00:42 gmt: ok", false, false},
		{"one second, a SQL datetime and an HTTP date", at.Format("2006-01-02 15:04:05"), f.Format("2006-01-02 15:04:05"), at.Format(time.RFC1123), f.Format(time.RFC1123), false, false},
		{"an HH:MM read and a clock read of its minute", at.Format("15:04"), f.Format("15:04"), at.Format(time.RFC3339), f.Format(time.RFC3339), false, false},
		{"an epoch second and a float of it", strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(f.Unix(), 10), fmt.Sprintf("%d.456", at.Unix()), fmt.Sprintf("%d.956", f.Unix()), false, false},
		{"epoch milliseconds and seconds", strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(f.Unix(), 10), strconv.FormatInt(at.UnixMilli(), 10), strconv.FormatInt(f.UnixMilli()+3, 10), false, false},
		// The same value whole appeared earlier: a variable (design §7.2
		// step 6), a clock read or not (an id from Date.now() sent twice).
		{"one clock read whole, twice", at.Format(time.RFC3339), f.Format(time.RFC3339), at.Format(time.RFC3339), f.Format(time.RFC3339), true, true},
		{"UUID v7s of one minute, both moved by the lead", uuid7(at, "abc123456789abcdef"), uuid7(f, "def0123456789abcde"), uuid7(at.Add(200*time.Millisecond), "123456789abcdef012"), uuid7(f.Add(203*time.Millisecond), "fedcba987654321012"), false, false},
		// Clock reads that moved by another amount than the lead are no
		// drift F's clock explains: a shared word is a value that flowed.
		{"clock reads moved by another amount", at.Format(time.RFC1123), at.Add(5 * time.Minute).Format(time.RFC1123), at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339), true, false},
		{"UUID v7s whose time moved by another amount", uuid7(at, "abc123456789abcdef"), uuid7(at.Add(81*time.Second), "def0123456789abcde"), uuid7(at.Add(200*time.Millisecond), "123456789abcdef012"), uuid7(at.Add(81*time.Second+200*time.Millisecond), "fedcba987654321012"), true, false},
		// At an earlier leaf F saw drift, a value that is one instant whole
		// co-drifts by its words as any value does (both lost the minute,
		// both gained the next), and also as the moment it names, both
		// ways, in another layout, or as its whole text (instants.go, rules
		// 1 and 3), but not one way only.
		{"instants of one minute moved by another amount", at.Format(time.RFC1123), at.Add(5 * time.Minute).Format(time.RFC1123), at.Add(7 * time.Second).Format(time.RFC3339), at.Add(5*time.Minute + 7*time.Second).Format(time.RFC3339), true, false},
		{"one instant moved by another amount, whole", at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339), at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339), true, true},
		{"one instant moved by another amount, inside a longer value", "at " + at.Format(time.RFC3339) + " ok", "at " + at.Add(5*time.Minute).Format(time.RFC3339) + " ok", at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339), true, false},
		{"one instant moved by another amount, its milliseconds", strconv.FormatInt(at.UnixMilli(), 10), strconv.FormatInt(at.Add(5*time.Minute).UnixMilli(), 10), at.Format("2006-01-02T15:04:05.000Z"), at.Add(5 * time.Minute).Format("2006-01-02T15:04:05.000Z"), true, false},
		// The same moment both ways in another layout, the instant inside
		// a longer value: rule 1 reads every value, not only one that is an
		// instant whole.
		{"one instant moved by another amount, its milliseconds, inside a longer value", strconv.FormatInt(at.UnixMilli(), 10), strconv.FormatInt(at.Add(5*time.Minute).UnixMilli(), 10), "booked at " + at.Format("2006-01-02T15:04:05.000Z") + " by w-1", "booked at " + at.Add(5*time.Minute).Format("2006-01-02T15:04:05.000Z") + " by w-1", true, false},
		{"the moment held one way only", at.Format(time.RFC1123), at.Add(9 * time.Minute).Format(time.RFC1123), at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339), false, false},
		{"one instant, its second against its millisecond", strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(at.Add(5*time.Minute).Unix(), 10), at.Add(473 * time.Millisecond).Format("2006-01-02T15:04:05.000Z"), at.Add(5*time.Minute + 473*time.Millisecond).Format("2006-01-02T15:04:05.000Z"), false, false},
		// What only stands inside a longer value is no unit: read word by
		// word, as every value is. Both lost the minute's word and both
		// gained A7K29QXM, a word that flowed.
		{"instants of one minute, then one word gained in both", at.Format(time.RFC3339), at.Add(5*time.Minute).Format(time.RFC3339) + " A7K29QXM", at.Add(7 * time.Second).Format(time.RFC3339), at.Add(5*time.Minute+7*time.Second).Format(time.RFC3339) + " A7K29QXM", true, false},
		{"one UUID v7 whose time moved by another amount, inside a longer value", uuid7(at, "abc123456789abcdef"), uuid7(at.Add(81*time.Second), "def0123456789abcde"), "x-" + uuid7(at, "abc123456789abcdef"), "y-" + uuid7(at.Add(81*time.Second), "def0123456789abcde"), true, false},
		// An id whose digits read as a date is no unit, nor is a value it
		// stands inside of: it is followed wherever it went, whatever its
		// length (a rand.Int31 id, a Snowflake id).
		{"a ten-digit id inside a longer value", "1298498081", "1427131847", "user-1298498081", "user-1427131847", true, false},
		{"a nineteen-digit id inside a longer value", "1541815603606036480", "1541815603606036999", "ref:1541815603606036480", "ref:1541815603606036999", true, false},
		{"a ten-digit id, whole", "1298498081", "1427131847", "1298498081", "1427131847", true, true},
		// One UUID v7 next to a fresh part in both: its random groups are
		// shared, a value that flowed.
		{"one UUID v7 next to a fresh part in both", uuid7(at, "abc123456789abcdef") + ":n1", uuid7(f, "def0123456789abcde") + ":n2", "x-" + uuid7(at, "abc123456789abcdef"), "y-" + uuid7(f, "def0123456789abcde"), true, false},
	} {
		dc := clockedDrifts()
		found, whole := sources(dc.of(tc.rec, tc.live), dc.of(tc.recorded, tc.now))
		if found != tc.found || whole != tc.whole {
			t.Errorf("%s: found %v whole %v, want %v %v (%q→%q at %q→%q)", tc.name, found, whole, tc.found, tc.whole, tc.recorded, tc.now, tc.rec, tc.live)
		}
	}
}

// clockOf reads the clock reads F's lead moved, in the layouts apps write,
// whole or inside a longer value, and only those.
func TestClockOf(t *testing.T) {
	at := time.Date(2026, 10, 14, 23, 59, 41, 456e6, time.UTC)
	f := at.Add(FullRoundLead)
	for _, tc := range []struct {
		name      string
		rec, live string
		whole     bool
	}{
		{"RFC 3339", at.Format(time.RFC3339), f.Format(time.RFC3339), true},
		{"RFC 3339 across midnight, with nanoseconds", at.Format(time.RFC3339Nano), f.Format(time.RFC3339Nano), true},
		{"an offset zone", at.In(time.FixedZone("", 5*3600+1800)).Format(time.RFC3339), f.In(time.FixedZone("", 5*3600+1800)).Format(time.RFC3339), true},
		{"an HTTP date", at.Format(time.RFC1123), f.Format(time.RFC1123), true},
		{"a SQL datetime", at.Format("2006-01-02 15:04:05.000"), f.Format("2006-01-02 15:04:05.000"), true},
		{"x-amz-date", at.Format("20060102T150405Z"), f.Format("20060102T150405Z"), true},
		{"a time of day across midnight", at.Format("15:04:05"), f.Format("15:04:05"), true},
		{"epoch seconds", strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(f.Unix(), 10), true},
		{"epoch nanoseconds", strconv.FormatInt(at.UnixNano(), 10), strconv.FormatInt(f.UnixNano()+7e6, 10), true},
		{"inside a longer value", "created=" + at.Format(time.RFC3339) + ";v=1", "created=" + f.Format(time.RFC3339) + ";v=1", true},
		{"a UUID v7", uuid7(at, "abc123456789abcdef"), uuid7(f, "def0123456789abcde"), false},
		{"moved by another amount", at.Format(time.RFC3339), at.Add(2 * time.Minute).Format(time.RFC3339), false},
		{"a counter is no clock", "41", "42", false},
		{"a minute read with an offset zone", "2026-10-14T10:00+05:30", "2026-10-14T10:01+05:30", true},
		{"no word changed", "a-b", "a_b", false},
		{"an emoji", "🦊", "🐼", false},
		{"ten digits outside the clock's years", "0000000041", "0000000102", false},
		{"ten digits that did not move by the lead", "2025551234", "2025559876", false},
	} {
		c := clockOf(tc.rec, tc.live)
		if c.whole != tc.whole {
			t.Errorf("%s: whole %v, want %v (%q→%q, clock words %v %v)", tc.name, c.whole, tc.whole, tc.rec, tc.live, c.rec, c.live)
		}
		// ClockOf is that evidence, for an adapter.
		if ClockOf(tc.rec, tc.live) != tc.whole {
			t.Errorf("%s: ClockOf %v, want %v", tc.name, !tc.whole, tc.whole)
		}
	}
	// ClockWordsOf names the changed words of a read the lead moved, whole
	// or beside words that changed otherwise, in the order they stand,
	// recorded first; none where no changed word is one. Many changed words
	// around the read do not hide it.
	sec, fsec := strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(f.Unix(), 10)
	many := func(n, base int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(strconv.Itoa(base+i) + "x ")
		}
		return b.String()
	}
	for _, tc := range []struct {
		name      string
		rec, live string
		want      []string
	}{
		{"epoch seconds whole", sec, fsec, []string{sec, fsec}},
		{"an event id: the read beside a random suffix", "evt_" + sec + "_a1b2", "evt_" + fsec + "_c3d4", []string{sec, fsec}},
		// FullRoundLead (61.545 s) moves 23:59:41.456 to 00:00:43.001.
		{"RFC 3339, its unchanged words left out", at.Format(time.RFC3339), f.Format(time.RFC3339), []string{"14T23", "59", "41Z", "15T00", "00", "43Z"}},
		{"a UUID v7: its time groups only", uuid7(at, "abc123456789abcdef"), uuid7(f, "def0123456789abcde"), clockWordsOfUUID7(at, f)},
		{"the read after forty changed words", many(40, 100) + sec, many(40, 500) + fsec, []string{sec, fsec}},
		{"a counter", "41", "42", nil},
		{"moved by another amount", at.Format(time.RFC3339), at.Add(2 * time.Minute).Format(time.RFC3339), nil},
		{"a random suffix alone", "evt_" + sec + "_a1b2", "evt_" + sec + "_c3d4", nil},
		{"no word changed", "a-b", "a_b", nil},
		{"one read written twice", sec + "/" + sec, fsec + "/" + fsec, []string{sec, sec, fsec, fsec}},
	} {
		if got := ClockWordsOf(tc.rec, tc.live); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("ClockWordsOf %s: %q, want %q (%q→%q)", tc.name, got, tc.want, tc.rec, tc.live)
		}
	}
	// A UUID v7's time groups moved with the clock; its random ones did not.
	c := clockOf(uuid7(at, "abc123456789abcdef"), uuid7(f, "def0123456789abcde"))
	r, l := segmentsBy(uuid7(at, "abc123456789abcdef"), isWord), segmentsBy(uuid7(f, "def0123456789abcde"), isWord)
	if !c.rec[r[0].text] || !c.live[l[0].text] || c.rec[r[4].text] || c.live[l[4].text] || c.rec[r[8].text] || c.live[l[8].text] {
		t.Fatalf("UUID v7 clock words %v %v", c.rec, c.live)
	}
	// Found in each value on its own: a fresh signature beside the read
	// changes the rest of the words.
	c = clockOf("t="+strconv.FormatInt(at.Unix(), 10)+",v1=q3+/Zx9Lk2mN8pQ4=", "t="+strconv.FormatInt(f.Unix(), 10)+",v1=Zz9Yy/8Xx7+Ww6Vv5Uu4=")
	if !c.rec[strconv.FormatInt(at.Unix(), 10)] || !c.live[strconv.FormatInt(f.Unix(), 10)] {
		t.Fatalf("an expiry beside a fresh signature: clock words %v %v", c.rec, c.live)
	}
}

// ClockReads is ClockOf's evidence as the learn step records it on a
// variable: each read the recorded value holds that moved by the lead, the
// instant and resolution its recorded text says, in the order the reads stand
// in it, and none for a value that drifted otherwise (FD-3 compares them).
func TestClockReads(t *testing.T) {
	at := time.Date(2026, 10, 14, 23, 59, 41, 456e6, time.UTC)
	f := at.Add(FullRoundLead)
	sec, ms := at.Truncate(time.Second), at.Truncate(time.Millisecond)
	day := time.Date(0, 1, 1, 23, 59, 41, 456e6, time.UTC)
	for _, tc := range []struct {
		name      string
		rec, live string
		want      []models.ClockRead
	}{
		{"RFC 3339, to the second", at.Format(time.RFC3339), f.Format(time.RFC3339), []models.ClockRead{{At: sec, Res: time.Second}}},
		{"RFC 3339, to the millisecond", at.Format(time.RFC3339Nano), f.Format(time.RFC3339Nano), []models.ClockRead{{At: ms, Res: time.Millisecond}}},
		{"an offset zone, read in UTC", at.In(time.FixedZone("", 5*3600+1800)).Format(time.RFC3339), f.In(time.FixedZone("", 5*3600+1800)).Format(time.RFC3339), []models.ClockRead{{At: sec, Res: time.Second}}},
		{"a SQL datetime with milliseconds", at.Format("2006-01-02 15:04:05.000"), f.Format("2006-01-02 15:04:05.000"), []models.ClockRead{{At: ms, Res: time.Millisecond}}},
		{"an HTTP date", at.Format(time.RFC1123), f.Format(time.RFC1123), []models.ClockRead{{At: sec, Res: time.Second}}},
		{"a minute", at.Format("2006-01-02T15:04Z07:00"), f.Format("2006-01-02T15:04Z07:00"), []models.ClockRead{{At: at.Truncate(time.Minute), Res: time.Minute}}},
		{"epoch seconds", strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(f.Unix(), 10), []models.ClockRead{{At: sec, Res: time.Second}}},
		{"epoch milliseconds", strconv.FormatInt(at.UnixMilli(), 10), strconv.FormatInt(f.UnixMilli()+3, 10), []models.ClockRead{{At: ms, Res: time.Millisecond}}},
		{"epoch microseconds", strconv.FormatInt(at.UnixMicro(), 10), strconv.FormatInt(f.UnixMicro(), 10), []models.ClockRead{{At: at.Truncate(time.Microsecond), Res: time.Microsecond}}},
		{"epoch nanoseconds", strconv.FormatInt(at.UnixNano(), 10), strconv.FormatInt(f.UnixNano()+7e6, 10), []models.ClockRead{{At: at, Res: time.Nanosecond}}},
		{"a float of epoch seconds", fmt.Sprintf("%d.456", at.Unix()), fmt.Sprintf("%d.956", f.Unix()), []models.ClockRead{{At: ms, Res: time.Millisecond}}},
		{"a time of day", at.Format("15:04:05"), f.Format("15:04:05"), []models.ClockRead{{At: day.Truncate(time.Second), Res: time.Second, Day: true}}},
		{"a time of day with milliseconds", at.Format("15:04:05.000"), f.Format("15:04:05.000"), []models.ClockRead{{At: day, Res: time.Millisecond, Day: true}}},
		{"two reads, in the order they stand", "t=" + strconv.FormatInt(at.Unix(), 10) + " at=" + at.Format(time.RFC3339Nano), "t=" + strconv.FormatInt(f.Unix(), 10) + " at=" + f.Format(time.RFC3339Nano),
			[]models.ClockRead{{At: sec, Res: time.Second}, {At: ms, Res: time.Millisecond}}},
		{"moved by another amount", at.Format(time.RFC3339), at.Add(2 * time.Minute).Format(time.RFC3339), nil},
		{"a clock read beside another word that changed", "created=" + at.Format(time.RFC3339) + ";v=1", "created=" + f.Format(time.RFC3339) + ";v=2", nil},
		{"a counter is no clock", "41", "42", nil},
	} {
		got := ClockReads(tc.rec, tc.live)
		if len(got) != len(tc.want) {
			t.Errorf("%s: reads %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if !got[i].At.Equal(tc.want[i].At) || got[i].Res != tc.want[i].Res || got[i].Day != tc.want[i].Day {
				t.Errorf("%s: read %d is %+v, want %+v", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

// Each of the running test's own mocks answers one request: a second take
// fails, a give-back frees it, an ended test takes nothing, and the next
// test starts afresh.
func TestTrackerAnswer(t *testing.T) {
	m, x := &models.Mock{Name: "m"}, &models.Mock{Name: "x"}
	tr := NewTracker()
	tr.Begin()
	n, _, _, _ := tr.Test()
	if !tr.Answer(n, m) || tr.Answer(n, m) || !tr.Answered("m") {
		t.Fatal("a mock answers one request of the test")
	}
	if got := tr.AnsweredMocks(); len(got) != 1 || got[0] != m {
		t.Fatalf("answered mocks %v: the mock as it answered", got)
	}
	tr.Unanswer(n, "m")
	if tr.Answered("m") || !tr.Answer(n, m) {
		t.Fatal("a mock given back answers again")
	}
	tr.Begin()
	if tr.Answer(n, x) {
		t.Fatal("an ended test takes nothing")
	}
	if tr.Answered("m") || len(tr.AnsweredMocks()) != 0 {
		t.Fatal("the next test starts afresh")
	}
	var nilTracker *Tracker
	if !nilTracker.Answer(1, m) || nilTracker.Answered("m") || nilTracker.AnsweredMocks() != nil {
		t.Fatal("a nil tracker takes every mock")
	}
}

// A spent candidate (one of the test's own mocks that answered already)
// competes and is neither served nor paired: the next recording of the
// call answers, and a call only a spent one explains is one call more than
// recorded, a miss with no reason of the learn step's.
func TestPairSpent(t *testing.T) {
	spent := func(s Shape) Candidate { return Candidate{Shape: s, Known: None, Spent: true} }
	t.Run("the next exact recording answers", func(t *testing.T) {
		r := Pair(body(t, "k", `{"id":"a"}`), []Candidate{spent(body(t, "k", `{"id":"a"}`)), cand(body(t, "k", `{"id":"a"}`), true)})
		if r.Outcome != Exact || r.Index != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("only a spent recording matches exactly: one call more", func(t *testing.T) {
		r := Pair(body(t, "k", `{"id":"a"}`), []Candidate{spent(body(t, "k", `{"id":"a"}`)), cand(body(t, "k", `{"id":"b"}`), true)})
		if r.Outcome != Miss || r.Index != 0 || r.Reason != "" || !strings.Contains(r.Why, "one call more") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a spent recording that drifts alike is passed over", func(t *testing.T) {
		r := Pair(body(t, "k", `{"id":"z"}`), []Candidate{spent(body(t, "k", `{"id":"a"}`)), cand(body(t, "k", `{"id":"a"}`), true)})
		if r.Outcome != Paired || r.Index != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a spent recording closest of all: one call more", func(t *testing.T) {
		r := Pair(body(t, "k", `{"id":"z","x":1}`), []Candidate{spent(body(t, "k", `{"id":"a","x":1}`)), cand(body(t, "k", `{"id":"a","x":2}`), true)})
		if r.Outcome != Miss || r.Index != 0 || r.Reason != "" || !strings.Contains(r.Why, "answered a request of the test already") {
			t.Fatalf("%+v", r)
		}
	})
}

// A payload of another protocol has no live view: the value, or what of it
// drifted, is searched there as a token, but for what drifted with F's
// clock; a whole clock read the lead moved only where it stands whole.
func TestAppearedInRawPayloads(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	raw := func(text string) []Raw {
		return []Raw{{rRead: true, mock: "mock-db", at: at.Add(-time.Second), served: true, rText: text}}
	}
	for _, tc := range []struct {
		name, text, recorded, now string
		found                     bool
	}{
		{"an order number from an INSERT, inside CJK text", `{"query":"INSERT INTO orders(no) VALUES ($1)","params":["A7K29Q"]}`, "您的订单A7K29Q已发货", "您的订单Z3M81X已发货", true},
		{"an order number from an INSERT, inside text", `{"query":"INSERT INTO orders(no) VALUES ($1)","params":["A7K29Q"]}`, "Your order A7K29Q shipped", "Your order Z3M81X shipped", true},
		{"a clock read of one second in a row of more precision", `{"rows":[["42","2026-10-14 10:00:41.123456+00"]]}`, "2026-10-14 10:00:41", "2026-10-14 10:01:42", false},
		{"a clock read of one second in another form", `{"rows":[["42","2026-10-14 10:00:41.123456+00"]]}`, "2026-10-14T10:00:41", "2026-10-14T10:01:42", false},
		{"an epoch second and a float of it", `{"command":"ZADD jobs 1760436041.123 job"}`, "1760436041", "1760436102", false},
		// The same value whole: followed, so refused where it cannot be
		// bound, a next column or element beside it or not.
		{"one clock read whole", `{"rows":[["42","2026-10-14 10:00:41"]]}`, "2026-10-14 10:00:41", "2026-10-14 10:01:42", true},
		{"an epoch second whole, the next column beside it", `{"query":"INSERT INTO events (ts,user_id) VALUES (1760436041,42)"}`, "1760436041", "1760436102", true},
		{"an epoch second whole, in a list", `{"value":"[1760436041,3]"}`, "1760436041", "1760436102", true},
		{"an epoch second whole, a member after it", `{"cmd":"ZADD due 1760436041 42"}`, "1760436041", "1760436102", true},
		{"a datetime whole, the next field after a comma", `{"csv":"2026-10-14 10:00:41,42"}`, "2026-10-14 10:00:41", "2026-10-14 10:01:42", true},
		{"a datetime with a decimal comma of milliseconds (a log stamp)", "2026-10-14 10:00:41,123 INFO order placed", "2026-10-14 10:00:41", "2026-10-14 10:01:42", false},
		{"an encoded clock read, whole in a payload", "INSERT INTO t (ts) VALUES ('2026-10-14T10:00:41Z')", "2026-10-14T10%3A00%3A41Z", "2026-10-14T10%3A01%3A42Z", true},
		{"an id in upper case in a payload, lower case in the value", `{"cmd":"SET digest:42 3F2B8C1E9A4D4E7B8C215D6E7F8A9B0C"}`, "sha256=3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c", "sha256=a7c4e2d10b3f4a8e9d126f5e4d3c2b1a", true},
		{"an id in lower case in a payload, upper case in the value", `{"cmd":"SET digest:42 3f2b8c1e9a4d4e7b8c215d6e7f8a9b0c"}`, "REF-3F2B8C1E9A4D4E7B8C215D6E7F8A9B0C", "REF-A7C4E2D10B3F4A8E9D126F5E4D3C2B1A", true},
		{"an id after a percent escape in a payload", `{"query":"INSERT INTO uploads VALUES ('https://storage.googleapis.com/b/o/uploads%2FA7K29QXM.png')"}`, "A7K29QXM", "Zq81Lm0X", true},
		{"a percent-encoded clock read of one second", `{"url":"https://a.blob/x?st=2026-10-14T10%3A00%3A41Z"}`, "2026-10-14T10:00:41.123Z", "2026-10-14T10:01:42.623Z", false},
	} {
		_, found := appeared(clockedDrifts(), tc.recorded, tc.now, at, nil, raw(tc.text))
		if found != tc.found {
			t.Errorf("%s: found %v, want %v", tc.name, found, tc.found)
		}
	}
}

// A payload's text is its own, not its JSON: a value framed by control
// characters (a RESP or memcached frame, a TSV field) or markup is a token
// there.
func TestRawOfReadsThePayloadsOwnText(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	for _, data := range []string{
		"*3\r\n$3\r\nSET\r\n$12\r\nsess:user-42\r\n$8\r\nA7K29QXM\r\n",
		"set order:42 0 0 8\r\nA7K29QXM\r\n",
		"<order><id>A7K29QXM</id></order>",
		"id\tA7K29QXM\tshipped",
		"INSERT INTO t (id) VALUES ('A7K29QXM')",
	} {
		m := &models.Mock{Name: "raw-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Second),
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: data}}}}}}
		if _, ok := appeared(clockedDrifts(), "A7K29QXM", "Zq81Lm0X", at, nil, []Raw{RawOf(m, true)}); !ok {
			t.Errorf("a token in %q is no source", data)
		}
	}
}

// A drift no earlier place is the source of is noise only where the
// declared readings account for all it changed: a clock read the lead
// moved, or words found nowhere else in the test. Whatever they cannot read
// whole, or a changed word found elsewhere where it did not drift with it,
// is refused (the safe direction for an encoding outside the readings).
func TestUnaccounted(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	f := at.Add(FullRoundLead)
	earlier := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.kind", Type: String, Value: "batch-7"}, Live: "batch-7"},
	}}}
	raw := []Raw{{rRead: true, mock: "pg-0", at: at.Add(-2 * time.Second), served: true, rText: "INSERT INTO jobs (shard) VALUES ('s3')"}}
	for _, tc := range []struct {
		name, recorded, now string
		refused             bool
	}{
		{"a fresh id found nowhere else", "req-3f2b8c1e9a4d", "req-a7c4e2d10b3f", false},
		{"a clock read the lead moved", at.Format(time.RFC3339), f.Format(time.RFC3339), false},
		{"a changed word found earlier where it stayed", "page-7", "page-9", true},
		{"a changed word found in a payload", "s3-a", "s4-a", true},
		{"a stray percent", "Save 20% now: 3f2b8c1e9a4d", "Save 20% now: a7c4e2d10b3f", true},
		{"a backslash that is no escape", `C:\x\3f2b8c1e9a4d`, `C:\x\a7c4e2d10b3f`, true},
		{"a standard base64 run that does not decode", "sig=ab+cd", "sig=ef+gh", true},
		{"text of a script with no spaces", "您的订单已创建", "您的订单已发货", true},
	} {
		dc := clockedDrifts()
		why := unaccounted(dc, dc.of(tc.recorded, tc.now), at, earlier, raw, nil)
		if (why != "") != tc.refused {
			t.Errorf("%s: unaccounted %q, want refused %v", tc.name, why, tc.refused)
		}
	}
}

// classifyOne classifies one drifted query leaf, as the HTTP adapter does.
func classifyOne(rec, live string, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf) Learned {
	r := Shape{Leaves: map[string]Leaf{"url.query.state": {Path: "url.query.state", Noise: "url.query.state", Type: String, Value: rec}}}
	l := Shape{Leaves: map[string]Leaf{"url.query.state": {Path: "url.query.state", Noise: "url.query.state", Type: String, Value: live}}}
	return Classify(nil, []string{"url.query.state"}, l, r, at, earlier, raw, resp)[0]
}

// Every declared reading is read whole and accounted for: no value that flowed ends learned as noise, through base64
// text, an escape, an echo or a whole clock value in another reading; and a
// clock read the lead moved, with a '+' offset, is still noise.
func TestClassifyInEveryDeclaredReading(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	earlier := func(rec, live string) []Earlier {
		return []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.name", Noise: "body.name", Type: String, Value: rec}, Live: live, Place: "body.name"},
		}}}
	}
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	b64s := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	notNoise := []struct {
		name, rec, live string
		earlier         []Earlier
		raw             []Raw
		resp            []Leaf
	}{
		{name: "a Cyrillic name inside base64 JSON", rec: b64u(`{"name":"Иван Петров","v":1}`), live: b64u(`{"name":"Пётр Сидоров","v":1}`), earlier: earlier("Иван Петров", "Пётр Сидоров")},
		{name: "a CJK name inside base64 JSON", rec: b64u(`{"name":"山田 太郎","v":1}`), live: b64u(`{"name":"佐藤 花子","v":1}`), earlier: earlier("山田 太郎", "佐藤 花子")},
		{name: "an emoji inside base64 JSON", rec: b64u(`{"name":"🦊","v":1}`), live: b64u(`{"name":"🐼","v":1}`), earlier: earlier("🦊", "🐼")},
		{name: "a \\u-escaped name inside base64 JSON", rec: b64s(`{"name": "\u0418\u0432\u0430\u043d"}`), live: b64s(`{"name": "\u041f\u0451\u0442\u0440"}`), earlier: earlier("Иван", "Пётр")},
		{name: "an id after %2F inside base64 text", rec: b64u("next=%2Forders%2FA7K29QXM&lang=en"), live: b64u("next=%2Forders%2FZq81Lm0X&lang=en"), earlier: earlier("A7K29QXM", "Zq81Lm0X")},
		{name: "an id inside base64 JSON", rec: b64u(`{"ref":"A7K29QXM"}`), live: b64u(`{"ref":"Zq81Lm0X"}`), earlier: earlier("A7K29QXM", "Zq81Lm0X")},
		{name: "a whole clock value, base64-encoded", rec: b64s("2026-10-14T10:00:41Z"), live: b64s("2026-10-14T10:01:42Z"), earlier: earlier("2026-10-14T10:00:41Z", "2026-10-14T10:01:42Z")},
		{name: "a whole clock value, percent-encoded", rec: "2026-10-14T10%3A00%3A41Z", live: "2026-10-14T10%3A01%3A42Z", earlier: earlier("2026-10-14T10:00:41Z", "2026-10-14T10:01:42Z")},
		{name: "a JWT whose iat is the epoch second an earlier call sent whole",
			rec:     "Bearer " + b64u(`{"alg":"HS256"}`) + "." + b64u(`{"sub":"svc","iat":1760436042}`) + "." + b64u("\x01\x02\x03\xfe\xff"),
			live:    "Bearer " + b64u(`{"alg":"HS256"}`) + "." + b64u(`{"sub":"svc","iat":1760436103}`) + "." + b64u("\x07\x08\x09\xfa\xfb"),
			earlier: earlier("1760436042", "1760436103")},
		{name: "an id after a backslash, from a payload", rec: `{"path":"C:\\b3f2b8c1e9a4d"}`, live: `{"path":"C:\\f1e2d3c4b5a69"}`,
			raw: []Raw{{rRead: true, mock: "pg-0", at: at.Add(-2 * time.Second), served: true, rText: "INSERT INTO jobs (id) VALUES ('b3f2b8c1e9a4d')"}}},
		{name: "an echoed id picked from an earlier answer", rec: "ord-7Q2K", live: "ord-9MX4",
			earlier: []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
				{Side: Response, Leaf: Leaf{Path: "body.items[0].id", Type: String, Value: "ord-7Q2K"}, Live: "ord-7Q2K"},
				{Side: Response, Leaf: Leaf{Path: "body.items[1].id", Type: String, Value: "ord-9MX4"}, Live: "ord-9MX4"},
			}}},
			resp: []Leaf{{Path: "body.id", Noise: "body.id", Type: String, Value: "ord-7Q2K"}}},
	}
	for _, tc := range notNoise {
		got := classifyOne(tc.rec, tc.live, at, tc.earlier, tc.raw, tc.resp)
		if got.Kind == KindNoise || got.Kind == KindEcho {
			t.Errorf("%s: learned as %s", tc.name, got.Kind)
		}
	}
	ist := time.FixedZone("IST", 19800)
	for _, tc := range []struct {
		name, layout string
		zone         *time.Location
	}{
		{"python isoformat, +00:00, microseconds", "2006-01-02T15:04:05.000000-07:00", time.UTC},
		{"python isoformat, +00:00", "2006-01-02T15:04:05-07:00", time.UTC},
		{"java, +05:30, milliseconds", "2006-01-02T15:04:05.000Z07:00", ist},
		{"postgres text, +00, microseconds", "2006-01-02 15:04:05.000000-07", time.UTC},
	} {
		// Read in the second before its call, at.
		rec := at.Add(234567*time.Microsecond - time.Second).In(tc.zone).Format(tc.layout)
		live := at.Add(FullRoundLead).Add(123456*time.Microsecond - time.Second).In(tc.zone).Format(tc.layout)
		if got := classifyOne(rec, live, at, nil, nil, nil); !noiseOrRead(got) {
			t.Errorf("%s: a clock read the lead moved is %s (%q)", tc.name, got.Kind, got.Why)
		}
	}
	if got := classifyOne("qa+3f2b8c1e-9a4d-4e7b-8c21-5d6e7f8a9b0c@example.com", "qa+a7c4e2d1-0b3f-4c2a-9e8d-1f2e3d4c5b6a@example.com", at, nil, nil, nil); got.Kind != KindNoise {
		t.Errorf("a plus-addressed email with a fresh id is %s (%q)", got.Kind, got.Why)
	}
}

// The declared decodings: each %XX on its own and again, backslash escapes
// as their characters (\uXXXX, a surrogate pair) or as separators.
func TestUnescaped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"uploads%2FA7K29Q.png", "uploads/A7K29Q.png"},
		{"next%3D%252Finvites%252FA7K29Q", "next=/invites/A7K29Q"},
		{"Save 20% now: uploads%2FA7K29Q", "Save 20% now: uploads/A7K29Q"},
		{`line\nA7K29Q`, "line A7K29Q"},
		{esc("#u0418#u0432#u0430#u043d"), "Иван"},
		{esc("#ud83e#udd8a"), "🦊"},
		{`x\\nA7K29Q`, "x A7K29Q"},
	} {
		if got := unescaped(tc.in); got != tc.want {
			t.Errorf("unescaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// What flowed is accounted for in every form a payload or a value carries
// it: a payload's base64 text decoded, unescaped and case-folded; base64
// inside base64, a JWT inside a base64 state, hex; words held in another
// order or once more; a word a value gained, searched in payloads too.
func TestClassifyAccountsForWhatFlowedInEveryForm(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	b64s := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	payload := func(text string) []Raw {
		return []Raw{{rRead: true, mock: "pg-0", at: at.Add(-2 * time.Second), served: true, rText: text}}
	}
	sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.user", Noise: "body.user", Type: String, Value: "A7K29QXM"}, Live: "Zq81Lm0X", Place: "body.user"},
	}}}
	answered := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.items[0].id", Type: String, Value: "ord-7Q2K"}, Live: "ord-7Q2K"},
		{Side: Response, Leaf: Leaf{Path: "body.items[1].id", Type: String, Value: "ord-9MX4"}, Live: "ord-9MX4"},
	}}}
	jwt := func(claims string) string {
		return b64u(`{"alg":"HS256","typ":"JWT"}`) + "." + b64u(claims) + "." + b64u("\x01\x02\x03\xfe\xff")
	}
	recSec, liveSec := "1760436042", "1760436103"
	// A base64 value whose '/' a JSON writer escaped ("\/", PHP's default).
	escapedSlash := func(ref string) string {
		for pad := 0; pad < 64; pad++ {
			s := b64s(`{"p":"` + strings.Repeat("?", pad) + `","ref":"` + ref + `"}`)
			if strings.Contains(s[:len(s)/2], "/") {
				return `{"state":"` + strings.ReplaceAll(s, "/", `\/`) + `"}`
			}
		}
		t.Fatal("no base64 text with a '/' before the ref")
		return ""
	}
	for _, tc := range []struct {
		name, rec, live string
		earlier         []Earlier
		raw             []Raw
	}{
		{name: "a percent-encoded path in a base64url state in SQL", rec: "A7K29QXM", live: "Zq81Lm0X",
			raw: payload("INSERT INTO oauth_states (id, state) VALUES (7, '" + b64u("next=%2Forders%2FA7K29QXM&lang=en") + "')")},
		{name: "a percent-encoded path in a base64 state in Redis", rec: "A7K29QXM", live: "Zq81Lm0X",
			raw: payload("SET\nstate:7\n" + b64s("next=%2Forders%2FA7K29QXM&lang=en"))},
		{name: "a JSON \\n glued before the ref inside base64 JSON", rec: "A7K29QXM", live: "Zq81Lm0X",
			raw: payload("INSERT INTO notes (body) VALUES ('" + b64s(`{"note":"ref:\nA7K29QXM"}`) + "')")},
		{name: "a \\u003e glued before the ref inside base64 JSON", rec: "A7K29QXM", live: "Zq81Lm0X",
			raw: payload("INSERT INTO docs (body) VALUES ('" + b64s("{\"xml\":\"\\u003cref\\u003eA7K29QXM\\u003c/ref\\u003e\"}") + "')")},
		{name: "upper case inside base64, lower case later", rec: "a7k29qxm", live: "zq81lm0x",
			raw: payload("INSERT INTO s (state) VALUES ('" + b64u(`{"ref":"A7K29QXM"}`) + "')")},
		{name: "a whole epoch second as a stored JWT's iat", rec: recSec, live: liveSec,
			raw: payload("SET\nsession:u1\n" + jwt(`{"sub":"u1","iat":`+recSec+`}`))},
		{name: "a whole epoch second in a base64 JSON blob in SQL", rec: recSec, live: liveSec,
			raw: payload("INSERT INTO s (blob) VALUES ('" + b64s(`{"iat":`+recSec+`}`) + "')")},
		{name: "a whole epoch second in SQL", rec: recSec, live: liveSec, raw: payload("INSERT INTO s (iat) VALUES (" + recSec + ")")},
		{name: "ids of an answer, joined in another order", rec: "ord-7Q2K,ord-9MX4", live: "ord-9MX4,ord-7Q2K", earlier: answered},
		{name: "ids of an answer, space-joined in another order", rec: "ord-7Q2K ord-9MX4", live: "ord-9MX4 ord-7Q2K", earlier: answered},
		{name: "an id of an answer, once more", rec: "ord-7Q2K,ord-9MX4", live: "ord-7Q2K,ord-9MX4,ord-7Q2K", earlier: answered},
		{name: "an id of an answer, picked", rec: "ord-7Q2K", live: "ord-9MX4", earlier: answered},
		{name: "ids of a payload's rows, in another order", rec: "ord-7Q2K,ord-9MX4", live: "ord-9MX4,ord-7Q2K",
			raw: payload("SELECT id FROM orders\nord-7Q2K\nord-9MX4\n")},
		{name: "an id in a JWT inside a base64 state", rec: b64u(`{"id_token":"` + jwt(`{"sub":"A7K29QXM"}`) + `","v":1}`),
			live: b64u(`{"id_token":"` + jwt(`{"sub":"Zq81Lm0X"}`) + `","v":1}`), earlier: sent},
		{name: "an id in base64 inside base64", rec: b64s(b64s(`{"ref":"A7K29QXM"}`)), live: b64s(b64s(`{"ref":"Zq81Lm0X"}`)), earlier: sent},
		{name: "an id in hex", rec: hex.EncodeToString([]byte("A7K29QXM")), live: hex.EncodeToString([]byte("Zq81Lm0X")), earlier: sent},
		{name: "an id in base64 whose '/' is JSON-escaped", rec: escapedSlash("A7K29QXM"), live: escapedSlash("Zq81Lm0X"), earlier: sent},
		{name: "an empty value filled from a payload's answer", rec: "", live: "rt-9fK2pQ7x", raw: payload("SELECT refresh FROM tokens\nrt-9fK2pQ7x\n")},
	} {
		got := classifyOne(tc.rec, tc.live, at, tc.earlier, tc.raw, nil)
		if got.Kind == KindNoise || got.Kind == KindEcho {
			t.Errorf("%s: learned as %s", tc.name, got.Kind)
		}
		// Nor as an echo, when the answer holds it.
		resp := []Leaf{{Path: "body.ref", Noise: "body.ref", Type: String, Value: tc.rec}}
		if got := classifyOne(tc.rec, tc.live, at, tc.earlier, tc.raw, resp); got.Kind == KindNoise || got.Kind == KindEcho {
			t.Errorf("%s, answered: learned as %s", tc.name, got.Kind)
		}
	}
}

// What did not change is no word of what changed: two fresh tokens that share
// a JWT header and its claims' names, or two values that share a line, are
// each noise.
func TestClassifyLearnsFreshValuesThatShareWhatDidNotChange(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tok := func(jti string, iat int64) string {
		return "Bearer " + b64u(`{"alg":"HS256","typ":"JWT"}`) + "." + b64u(`{"jti":"`+jti+`","iat":`+strconv.FormatInt(iat, 10)+`}`) + "." + b64u("\x01\x02"+jti[:3]+"\xfe\xff")
	}
	seen := func(rec, live string) []Earlier {
		return []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Payload: payloadOf("mock-0", at.Add(-time.Second)), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "header.authorization", Noise: "header.authorization", Type: String, Value: rec}, Live: live, Place: "header.authorization"},
		}}}
	}
	t0, t1 := at.Unix()-1, at.Add(FullRoundLead).Unix()-1
	for _, tc := range []struct {
		name, rec, live string
		earlier         []Earlier
	}{
		{"two fresh JWTs", tok("Hq6Ys0Bc5D", t0+1), tok("Mw2Xr8Ue3F", t1+1), seen(tok("k3J9xQ2mW8", t0), tok("Lp4Zt7Vn1R", t1))},
		{"two messages that share a line", "Order created\nref: Hq6Ys0Bc5D", "Order created\nref: Mw2Xr8Ue3F",
			seen("Order created\nref: k3J9xQ2mW8", "Order created\nref: Lp4Zt7Vn1R")},
		{"two pretty-printed JSON texts", "{\n  \"ref\": \"Hq6Ys0Bc5D\"\n}", "{\n  \"ref\": \"Mw2Xr8Ue3F\"\n}",
			seen("{\n  \"id\": \"k3J9xQ2mW8\"\n}", "{\n  \"id\": \"Lp4Zt7Vn1R\"\n}")},
	} {
		if got := classifyOne(tc.rec, tc.live, at, tc.earlier, nil, nil); got.Kind != KindNoise {
			t.Errorf("%s: %s (%s)", tc.name, got.Kind, got.Why)
		}
	}
}

// A whole clock value an earlier call held inside a longer value: noise
// beside an HTTP leaf (a clock read inside a longer value is clock noise),
// refused beside a payload, where keploy cannot tell a longer value's bounds
// from a word's (the safe direction).
func TestClassifyAWholeClockValueAnEarlierCallHeldInsideALongerOne(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	rec, live := "2026-10-14T10:00:41Z", "2026-10-14T10:01:42Z"
	e := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Payload: payloadOf("mock-0", at.Add(-time.Second)), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.note", Noise: "body.note", Type: String, Value: "created at " + rec + " by u1"}, Live: "created at " + live + " by u1", Place: "body.note"},
	}}}
	if got := classifyOne(rec, live, at, e, nil, nil); !noiseOrRead(got) {
		t.Errorf("beside an HTTP leaf: %s (%s)", got.Kind, got.Why)
	}
	raw := []Raw{{rRead: true, mock: "pg-0", at: at.Add(-2 * time.Second), served: true, rText: "INSERT INTO n (note) VALUES ('created at " + rec + " by u1')"}}
	if got := classifyOne(rec, live, at, nil, raw, nil); got.Kind == KindNoise || got.Kind == KindEcho {
		t.Errorf("beside a payload: %s", got.Kind)
	}
}

// The encoded reading decodes runs that can carry an id (eight characters
// or more; hex and base32 to ASCII text, base32 in one case), and nothing
// shorter: a short fresh id's decode is garbage.
func TestB64TextReadsRunsThatCanCarryAnId(t *testing.T) {
	id := "A7K29QXM"
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(id))
	for _, enc := range []string{
		hex.EncodeToString([]byte(id)),
		b32,
		strings.ToLower(b32),
		base64.RawURLEncoding.EncodeToString([]byte(`{"ref":"` + id + `"}`)),
		base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString([]byte(id)))),
	} {
		if got := b64Text(enc, nil); !strings.Contains(got, id) {
			t.Errorf("b64Text(%q) = %q, want it to hold %q", enc, got, id)
		}
	}
	for _, word := range []string{"ddqs", "fldc", "QXMZTA", "diml"} {
		if got := b64Text(word, nil); got != "" {
			t.Errorf("b64Text(%q) = %q: a short word decoded", word, got)
		}
	}
	if isText([]byte("\u008ah")) {
		t.Error("a C1 control character is text")
	}
}

// An id that flowed is read through the encodings apps wrap it in: base64
// wrapped in lines (Ruby's Base64.encode64 at 60 columns, MIME at 76), base64
// glued to a digest ("<base64>--<digest>" of a Rails signed message), base32
// with its padding, hex with "0x"; in a value and in a payload.
func TestClassifyReadsWrappedGluedAndPaddedEncodings(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	r := rand.New(rand.NewSource(14))
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	id := func() string {
		b := make([]byte, 8)
		for i := range b {
			b[i] = alpha[r.Intn(len(alpha))]
		}
		return string(b)
	}
	b64s := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	wrapped := func(width int) func(pad int, id string) string {
		return func(pad int, id string) string {
			e := b64s(`{"note":"` + strings.Repeat("x", pad) + `","ref":"` + id + `","v":1}`)
			var sb strings.Builder
			for len(e) > width {
				sb.WriteString(e[:width] + "\n")
				e = e[width:]
			}
			sb.WriteString(e + "\n")
			return sb.String()
		}
	}
	signed := func(pad int, id string) string {
		data := b64s(`{"_rails":{"message":"` + id + `","exp":null,"pur":"x` + strings.Repeat("y", pad%30) + `"}}`)
		sum := sha1.Sum([]byte(data))
		return data + "--" + hex.EncodeToString(sum[:])
	}
	b32 := func(_ int, id string) string { return base32.StdEncoding.EncodeToString([]byte(id + "-" + id)) }
	hex0x := func(_ int, id string) string { return "0x" + hex.EncodeToString([]byte(id)) }
	wrap := func(e string, width int, nl string) string {
		var sb strings.Builder
		for len(e) > width {
			sb.WriteString(e[:width] + nl)
			e = e[width:]
		}
		sb.WriteString(e + nl)
		return sb.String()
	}
	doc := func(pad int, id string) string {
		return b64s(`{"note":"` + strings.Repeat("x", pad) + `","ref":"` + id + `","v":1}`)
	}
	escapedCRLF := func(pad int, id string) string { return `{"blob":"` + wrap(doc(pad, id), 76, `\r\n`) + `"}` }
	thenText := func(pad int, id string) string { return wrap(doc(pad, id), 60, "\n") + "sent by worker 3\n" }
	keyed := func(pad int, id string) string { return "data=" + wrap(doc(pad, id), 60, "\n") }
	indented := func(pad int, id string) string {
		return "<blob>\n    " + wrap(doc(pad, id), 64, "\n    ") + "</blob>"
	}
	gluedMiddle := func(pad int, id string) string {
		d := base64.RawURLEncoding.EncodeToString([]byte(`{"ref":"` + id + `","p":"` + strings.Repeat("y", pad) + `"}`))
		sum := sha1.Sum([]byte(d))
		return "v1_" + d + "--" + hex.EncodeToString(sum[:])
	}
	for _, tc := range []struct {
		name string
		enc  func(pad int, id string) string
	}{
		{"base64 wrapped at 60", wrapped(60)},
		{"base64 wrapped at 76", wrapped(76)},
		{"a signed message", signed},
		{"padded base32", b32},
		{"0x hex", hex0x},
		{"base64 wrapped with escaped CRLFs", escapedCRLF},
		{"wrapped base64 and a line of text", thenText},
		{"wrapped base64 after a key", keyed},
		{"indented wrapped base64", indented},
		{"base64 glued between a prefix and a digest", gluedMiddle},
	} {
		for pad := 0; pad < 60; pad++ {
			a, b := id(), id()
			sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
				{Side: Request, Leaf: Leaf{Path: "body.user", Noise: "body.user", Type: String, Value: a}, Live: b, Place: "body.user"},
			}}}
			if got := classifyOne(tc.enc(pad, a), tc.enc(pad, b), at, sent, nil, nil); got.Kind == KindNoise || got.Kind == KindEcho {
				t.Fatalf("%s (pad %d): an id that flowed into it learned as %s", tc.name, pad, got.Kind)
			}
			raw := []Raw{{rRead: true, mock: "pg-0", at: at.Add(-2 * time.Second), served: true, rText: "INSERT INTO s (blob) VALUES ('" + tc.enc(pad, a) + "')"}}
			if got := classifyOne(a, b, at, nil, raw, nil); got.Kind == KindNoise || got.Kind == KindEcho {
				t.Fatalf("%s (pad %d), in a payload: an id that flowed from it learned as %s", tc.name, pad, got.Kind)
			}
		}
	}
}

// What changed is the words outside a longest common subsequence, ties
// broken both ways: of two words that swapped both moved, a word held once
// more moved, and words that kept their order between fresh ones did not.
func TestMovedWords(t *testing.T) {
	for _, tc := range []struct {
		rec, live         []string
		wantRec, wantLive []string
	}{
		{[]string{"x", "F"}, []string{"F", "x"}, []string{"x", "F"}, []string{"F", "x"}},
		{[]string{"a", "b"}, []string{"a", "b", "a"}, nil, []string{"a"}},
		{[]string{"state", "n1", "user", "acct", "nonce", "n2"}, []string{"state", "n3", "user", "acct", "nonce", "n4"}, []string{"n1", "n2"}, []string{"n3", "n4"}},
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}, nil, nil},
	} {
		r, l := movedWords(tc.rec, tc.live)
		if fmt.Sprint(r) != fmt.Sprint(tc.wantRec) || fmt.Sprint(l) != fmt.Sprint(tc.wantLive) {
			t.Errorf("movedWords(%v, %v) = %v, %v; want %v, %v", tc.rec, tc.live, r, l, tc.wantRec, tc.wantLive)
		}
	}
}

// Fresh tokens around words that stayed in place are noise, though those
// words appear earlier in the test.
func TestClassifyLearnsFreshTokensAroundWordsThatStayed(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	e := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Payload: payloadOf("mock-0", at.Add(-time.Second)), Own: true, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "url.query.user", Noise: "url.query.user", Type: String, Value: "acct_7Q2K9"}, Live: "acct_7Q2K9", Place: "url.query.user"},
		{Side: Request, Leaf: Leaf{Path: "body.trace", Noise: "body.trace", Type: String, Value: "user42 trace"}, Live: "user42 trace", Place: "body.trace"},
	}}}
	for _, tc := range []struct{ rec, live string }{
		{"state=V1StGXR8_Z5jdHi6B-myT&user=acct_7Q2K9&nonce=Uakgb_J5m9g-0JDMbcJqLJ", "state=x9Kq2_LmP4t-Zr7Wn3BvC&user=acct_7Q2K9&nonce=Hy6_tR2pQ9w-Lm4Kz8XcVb"},
		{"req V1StGXR8_Z5jdHi6B-myT for user42 trace Uakgb_J5m9g-0JDMbcJqLJ", "req x9Kq2_LmP4t-Zr7Wn3BvC for user42 trace Hy6_tR2pQ9w-Lm4Kz8XcVb"},
	} {
		if got := classifyOne(tc.rec, tc.live, at, e, nil, nil); got.Kind != KindNoise {
			t.Errorf("%q: %s (%s)", tc.rec, got.Kind, got.Why)
		}
	}
}

// KeysLeaf is every key of a JSON body's objects, one per line in order.
func TestKeysLeaf(t *testing.T) {
	v, _ := DecodeJSON([]byte(`{"orders":{"ord-9MX4":{"qty":2},"ord-7Q2K":{"qty":1}},"list":[{"id":"x"}]}`))
	got, ok := KeysLeaf("body", v)
	if !ok || got.Type != Keys || !got.Unnamed || got.Value != "id\nlist\nord-7Q2K\nord-9MX4\norders\nqty" {
		t.Fatalf("KeysLeaf = %+v %v", got, ok)
	}
	if _, ok := KeysLeaf("body", []interface{}{"a"}); ok {
		t.Fatal("a body with no object has no keys")
	}
	r := Leaf{Path: "body.id", Type: String, Value: "ord-7Q2K"}
	if e := Echoes(nil, r, []Leaf{got}); len(e) != 1 || WholeEcho(r, got) {
		t.Fatalf("an id at a key is an echo keploy cannot answer: %+v", e)
	}
}

// A payload's wrapped base64, read as RawOf reads it (its last line next to
// the text that follows), carries an id that flowed.
func TestAppearedInAPayloadsWrappedBase64(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	for pad := 0; pad < 60; pad++ {
		a, b := fmt.Sprintf("Kq%06dXm", pad), fmt.Sprintf("Zr%06dWn", pad)
		e := base64.StdEncoding.EncodeToString([]byte(`{"note":"` + strings.Repeat("x", pad) + `","ref":"` + a + `","v":1}`))
		var sb strings.Builder
		for len(e) > 60 {
			sb.WriteString(e[:60] + "\n")
			e = e[60:]
		}
		sb.WriteString(e + "\n")
		m := &models.Mock{Name: "gen-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: at.Add(-2 * time.Second),
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: sb.String()}}}}}}
		if got := classifyOne(a, b, at, nil, []Raw{RawOf(m, true)}, nil); got.Kind == KindNoise || got.Kind == KindEcho {
			t.Fatalf("pad %d: an id from a payload's wrapped base64 learned as %s", pad, got.Kind)
		}
	}
}

// An answer that holds the value in another declared form (inside base64,
// percent-encoded, in another case) echoes it, at a place keploy cannot
// answer: never noise.
func TestEchoesInEveryForm(t *testing.T) {
	a := "A7K29QXMtq"
	r := Leaf{Path: "body.id", Type: String, Value: a}
	for _, answer := range []string{
		base64.StdEncoding.EncodeToString([]byte(`{"after":"` + a + `","n":20}`)),
		"https://api.test/v1/next?u=%2Fusers%2F" + a,
		"USER-" + strings.ToUpper(a),
		strings.ToLower(a),
		"user " + strings.ToLower(a),
		"user " + a,
	} {
		resp := []Leaf{{Path: "body.next", Noise: "body.next", Type: String, Value: answer}}
		e := Echoes(nil, r, resp)
		if len(e) != 1 || WholeEcho(r, e[0]) {
			t.Errorf("%q: echoes %+v", answer, e)
		}
	}
	// At a header name, which the answer's leaves hold in lower case.
	names := Leaf{Path: "header{names}", Type: Keys, Value: "content-type\nx-amz-meta-" + strings.ToLower(a), Unnamed: true}
	if e := Echoes(nil, r, []Leaf{names}); len(e) != 1 {
		t.Errorf("an echo at a header name: %+v", e)
	}
}

// Mail and wrapping shapes an id flows through: quoted-printable ("=3D"
// before it, a soft line break inside it), raw and inside base64 (SES's and
// Gmail's raw message) and in a payload (SMTP DATA); wrapped base64 after a
// header that ends in an id-like run and a blank line, escaped with an indent,
// or after a prefix glued by '_'; base64url glued by "--" to a digest when
// both hold many '-' and '_' of their own.
func TestClassifyReadsMailAndWrappingShapes(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	r := rand.New(rand.NewSource(16))
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	id := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.Intn(len(alpha))]
		}
		return string(b)
	}
	b64s := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	wrap := func(e string, width int, nl string) string {
		var sb strings.Builder
		for len(e) > width {
			sb.WriteString(e[:width] + nl)
			e = e[width:]
		}
		sb.WriteString(e + nl)
		return sb.String()
	}
	doc := func(pad int, id string) string {
		return b64s(`{"note":"` + strings.Repeat("x", pad) + `","order":"` + id + `","v":1}`)
	}
	mime := func(pad int, v string) string {
		return "From: shop@test\r\nSubject: Your order\r\nMessage-ID: <" + id(16) + "@test>\r\n" +
			"Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
			"<p>" + strings.Repeat("x", pad) + "</p><a href=3D\"https://shop.test/track?order=3D" + v + "&lang=3Den\">Track</a>\r\n"
	}
	softBreak := func(pad int, v string) string {
		line := strings.Repeat("x", pad) + " order " + v + " shipped today, thank you for shopping with us at the shop"
		var sb strings.Builder
		for len(line) > 75 {
			sb.WriteString(line[:75] + "=\r\n")
			line = line[75:]
		}
		sb.WriteString(line)
		return "Content-Transfer-Encoding: quoted-printable\r\n\r\n" + sb.String()
	}
	uuid := func() string {
		h := hex.EncodeToString([]byte(id(16)))[:32]
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	}
	headerBlank := func(pad int, v string) string {
		return "Content-Transfer-Encoding: base64\r\nX-Request-Id: " + uuid() + "\r\n\r\n" + wrap(doc(pad, v), 76, "\r\n")
	}
	hexBefore := func(pad int, v string) string {
		return "ref " + strings.ReplaceAll(uuid(), "-", "") + "\n" + wrap(doc(pad, v), 76, "\n")
	}
	escapedIndent := func(pad int, v string) string {
		return `{"cfg":"data: |\n    ` + wrap(doc(pad, v), 76, `\n    `) + `"}`
	}
	gluedPrefix := func(pad int, v string) string { return "v1_" + wrap(doc(pad, v), 60, "\n") }
	cart := func() string {
		var items []string
		for i := 0; i < 30; i++ {
			name := []rune{}
			for k := 0; k < 4; k++ {
				name = append(name, rune(0x4E00+r.Intn(0x5000)))
			}
			items = append(items, fmt.Sprintf(`{"sku":"%s","name":"%s","q":%d}`, id(8), string(name), r.Intn(9)+1))
		}
		return strings.Join(items, ",")
	}()
	qpSoftLine := func(pad int, v string) string {
		line := strings.Repeat("x", pad) + " Your order " + v + " has shipped. Thank you for shopping with us, see you soon at the shop."
		var sb strings.Builder
		for len(line) > 75 {
			sb.WriteString(line[:75] + "=\r\n")
			line = line[75:]
		}
		sb.WriteString(line)
		return "Content-Transfer-Encoding: quoted-printable\r\n\r\n" + sb.String() + "\r\n"
	}
	jsonString := func(pad int, v string) string {
		b, _ := json.Marshal(map[string]string{"raw": qpSoftLine(pad, v)})
		return string(b)
	}
	wrapWith := func(nl string) func(pad int, v string) string {
		return func(pad int, v string) string { return wrap(doc(pad, v), 76, nl) }
	}
	singleGlued := func(sep string) func(pad int, v string) string {
		return func(_ int, v string) string {
			d := base64.RawURLEncoding.EncodeToString([]byte(`{"order":"` + v + `","cart":[` + cart + `]}`))
			h := hmac.New(sha256.New, []byte("k"))
			h.Write([]byte(d))
			return d + sep + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
		}
	}
	gluedBlob := func(sep string, n int, before bool) func(pad int, v string) string {
		return func(_ int, v string) string {
			b := make([]byte, n)
			r.Read(b)
			blob := base64.RawURLEncoding.EncodeToString(b)
			d := base64.RawURLEncoding.EncodeToString([]byte(`{"order":"` + v + `","cart":[` + cart + `]}`))
			if before {
				return blob + sep + d
			}
			return d + sep + blob
		}
	}
	manyGroups := func(_ int, v string) string {
		d := base64.RawURLEncoding.EncodeToString([]byte(`{"order":"` + v + `","cart":[` + cart + `]}`))
		sum := sha256.Sum256([]byte(d))
		return "v1_" + d + "--" + base64.RawURLEncoding.EncodeToString(sum[:])
	}
	for _, tc := range []struct {
		name string
		enc  func(pad int, id string) string
		n    int
	}{
		{"quoted-printable mail", mime, 20},
		{"quoted-printable mail inside base64", func(pad int, v string) string { return b64s(mime(pad, v)) }, 20},
		{"a quoted-printable soft break", softBreak, 70},
		{"wrapped base64 after a header and a blank line", headerBlank, 40},
		{"wrapped base64 after a line ending in hex", hexBefore, 40},
		{"wrapped base64 escaped with an indent", escapedIndent, 40},
		{"wrapped base64 after a prefix glued by '_'", gluedPrefix, 40},
		{"base64url glued to a digest, both with many groups", manyGroups, 20},
		{"a quoted-printable mail as an escaped JSON string", jsonString, 40},
		{"a quoted-printable mail escaped twice (SNS in SQS)", func(pad int, v string) string {
			b, _ := json.Marshal(map[string]string{"Message": jsonString(pad, v)})
			return string(b)
		}, 75},
		{"wrapped base64 with blank lines", wrapWith("\n\n"), 40},
		{"wrapped base64 with a trailing space", wrapWith(" \n"), 40},
		{"wrapped base64 with CR CR LF", wrapWith("\r\r\n"), 40},
		{"wrapped base64 with tab CR LF", wrapWith("\t\r\n"), 40},
		{"base64url glued by '_' to an HMAC", singleGlued("_"), 20},
		{"base64url glued by '-' to an HMAC", singleGlued("-"), 20},
		{"base64url glued by '_' to a 160-character blob", gluedBlob("_", 120, false), 20},
		{"base64url glued by '-' to an RSA-size signature", gluedBlob("-", 256, false), 20},
		{"a 160-character blob glued by '_' to base64url", gluedBlob("_", 120, true), 20},
	} {
		for pad := 0; pad < tc.n; pad++ {
			a, b := id(10), id(10)
			sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
				{Side: Request, Leaf: Leaf{Path: "body.order", Noise: "body.order", Type: String, Value: a}, Live: b, Place: "body.order"},
			}}}
			if got := classifyOne(tc.enc(pad, a), tc.enc(pad, b), at, sent, nil, nil); got.Kind == KindNoise || got.Kind == KindEcho {
				t.Fatalf("%s (pad %d): an id that flowed into it learned as %s", tc.name, pad, got.Kind)
			}
			m := &models.Mock{Name: "smtp-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: at.Add(-2 * time.Second),
				GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: "DATA\r\n" + tc.enc(pad, a) + ".\r\n"}}}}}}
			if got := classifyOne(a, b, at, nil, []Raw{RawOf(m, true)}, nil); got.Kind == KindNoise || got.Kind == KindEcho {
				t.Fatalf("%s (pad %d), in a payload: an id that flowed from it learned as %s", tc.name, pad, got.Kind)
			}
		}
	}
}

// quoted-printable: soft breaks out, "=XX" runs that are text decoded, but
// never to '%' or '\'.
func TestQPDecoded(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://x/?order=3DA7K29Q&n=3D1", "https://x/?order=A7K29Q&n=1"},
		{"A7K=\r\n29Q", "A7K29Q"},
		{"caf=C3=A9 A7K", "café A7K"},
		{"limit=25&x=5C", "limit=25&x=5C"},
		{"k=AB12", "k=AB12"},
		{"plain", "plain"},
	} {
		if got := qpDecoded(tc.in); got != tc.want {
			t.Errorf("qpDecoded(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A long run of base64url with many groups of its own is cut at places
// whose spans add up to a constant times its length, whatever its length:
// reading it stays linear.
func TestGluedSpansAreLinear(t *testing.T) {
	r := rand.New(rand.NewSource(18))
	for _, n := range []int{4096, 32768, 98304} {
		b := make([]byte, n)
		r.Read(b)
		run := base64.RawURLEncoding.EncodeToString(b)
		total := 0
		for _, sp := range gluedSpans(run) {
			total += sp[1] - sp[0]
		}
		if total > 100*len(run) {
			t.Fatalf("a %d-character run is cut into spans %d characters long in all", len(run), total)
		}
	}
}

// ReadRaw reads a mock whose answer the agent parked on disk with its answer
// back, on a copy, once per drift cache; ok is false when it cannot be read
// back. RawOf reads it without.
func TestReadRawReadsAParkedAnswerBack(t *testing.T) {
	reads := 0
	m := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "/a"}}}
	m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		reads++
		return &models.HTTPResp{StatusCode: 200, Body: `{"token":"tok-7f3a9c"}`}, nil, nil
	})
	if r := RawOf(m, true); strings.Contains(r.Text(), "tok-7f3a9c") {
		t.Fatal("RawOf read the parked answer: the test's premise moved")
	}
	dc := clockedDrifts()
	for i := 0; i < 2; i++ {
		r, err := dc.ReadRaw(m, true)
		if err != nil || !strings.Contains(r.Text(), "tok-7f3a9c") {
			t.Fatalf("read %d: err %v, text %q", i, err, r.Text())
		}
	}
	if reads != 1 {
		t.Fatalf("read back %d times; want once for the cache", reads)
	}
	if m.Spec.HTTPResp != nil || !m.HasSpilledResponse() {
		t.Fatal("the pooled mock was written")
	}
	var nilCache *DriftCache
	if r, err := nilCache.ReadRaw(m, false); err != nil || !strings.Contains(r.Text(), "tok-7f3a9c") {
		t.Fatalf("a nil cache: err %v, text %q", err, r.Text())
	}
	broken := &models.Mock{Name: "mock-1", Kind: models.HTTP}
	broken.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("disk mocks: store closed")
	})
	if _, err := dc.ReadRaw(broken, false); err == nil || !strings.Contains(err.Error(), "store closed") {
		t.Fatalf("an answer that cannot be read back: err %v; want the store's", err)
	}
}

// One input gives one reason and one place, however often it is classified:
// where two words of a value each stand earlier, the word that stands first
// in the value is the one a reason names (inOrder), never the one a set's
// order gives first. Each row runs its input many times and expects one
// outcome, text for text.
func TestAReasonIsOneTextForOneInput(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 10, 0, 0, time.UTC)
	mk := func(name, body string, header map[string]string, off time.Duration) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "/x"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: header, Body: body},
			ReqTimestampMock: at.Add(off), ResTimestampMock: at.Add(off + time.Millisecond),
		}}
	}
	const p = "body.ref"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	for _, tc := range []struct {
		name, rec, live string
		mocks           []*models.Mock
		want            string
	}{
		// Two words the value lost stand in two fields of one payload: a
		// variable of the field of the first (appearedIn).
		{"a variable found in a payload", "x-aaa1-ccc3", "x-bbb2-ddd4",
			[]*models.Mock{mk("mock-1", `{}`, map[string]string{"X-A": "aaa1", "X-B": "ccc3"}, -3*time.Second)},
			"variable | mock-1 | spec.resp.header.X-A | a part of its recorded value (its characters 3 to 6)"},
		// Two words F saw stand in two payloads: refused naming the first
		// of them, in the payload that holds it (unaccounted).
		{"an unaccounted word in a payload", "x-aaa1", "x-bbb2-ccc3",
			[]*models.Mock{mk("mock-1", `{"a":"ccc3"}`, nil, -3*time.Second), mk("mock-2", `{"b":"bbb2"}`, nil, -2*time.Second)},
			"unaccounted |  |  | a word of what record's full round saw in it (its characters 3 to 6) appears in an earlier recording, in mock-2"},
	} {
		seen := map[string]int{}
		for i := 0; i < 200; i++ {
			dc := clockedDrifts()
			var raw []Raw
			for _, m := range tc.mocks {
				raw = append(raw, dc.RawOf(m, true))
			}
			ln := ClassifyIn(dc, []string{p}, shape(tc.live), shape(tc.rec), at, nil, raw, nil, nil)[0]
			seen[string(ln.Kind)+" | "+ln.From.Mock+" | "+ln.From.Field+" | "+ln.From.Held+" | "+ln.Why]++
			if i == 0 {
				got := string(ln.Kind) + " | " + ln.From.Mock + " | " + ln.From.Field + " | " + ln.Why
				if !strings.HasPrefix(got, tc.want) {
					t.Errorf("%s: %q; want it to begin %q", tc.name, got, tc.want)
				}
			}
		}
		if len(seen) != 1 {
			t.Errorf("%s: %d outcomes for one input: %v", tc.name, len(seen), seen)
		}
	}
}

// A Raw is read from its mock or not at all: one the classifier is handed
// that was not (the zero Raw, or ReadRaw's of an answer it could not read
// back) refuses every drift, naming it, rather than be searched for nothing.
func TestARawNotReadFromItsMockIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 10, 0, 0, time.UTC)
	const p = "body.ref"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	broken := &models.Mock{Name: "mock-7", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Second)}}
	broken.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("disk mocks: store closed")
	})
	unread, err := ReadRaw(broken, true)
	if err == nil || unread.Read() || unread.Mock() != "mock-7" || !unread.Served() || !unread.At().Equal(at.Add(-time.Second)) {
		t.Fatalf("ReadRaw of an answer it cannot read back: %+v, %v", unread, err)
	}
	good := RawOf(&models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "/x"}}}, false)
	if !good.Read() {
		t.Fatal("RawOf of a readable mock is not read")
	}
	for name, raw := range map[string][]Raw{"the zero Raw": {good, {}}, "ReadRaw's unread Raw": {unread, good}} {
		for _, live := range []string{"req-a7c4e2d10b3f", "2026-10-22T11:15:00Z"} {
			ln := ClassifyIn(clockedDrifts(), []string{p}, shape(live), shape("req-3f2b8c1e9a4d"), at, nil, raw, nil, nil)[0]
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "keploy could not read") || name != "the zero Raw" && !strings.Contains(ln.Why, "mock-7") {
				t.Errorf("%s, -> %q: %s (%s); want every drift refused, naming the recording", name, live, ln.Kind, ln.Why)
			}
		}
	}
}

// One input gives one reason wherever it is found: at a leaf of an exchange
// F served, at the test case's own request, and where F's moment of a typed
// instant stands. Each row runs its input 200 times and expects one text.
func TestAReasonIsOneTextAtEveryPlace(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const p = "body.ref"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	typed := func(t time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, t, time.Second)}}
	}
	leaves := []Earlier{{Mock: "mock-1", At: at.Add(-time.Second), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.a", Noise: "body.a", Type: String, Value: "ccc3"}, Live: "ccc3"},
		{Side: Response, Leaf: Leaf{Path: "body.b", Noise: "body.b", Type: String, Value: "aaa1"}, Live: "aaa1"},
	}}}
	request := []Earlier{GivenRequest(slotRequest("11:13", "11:14", "2026-10-22T11:14:00Z"))}
	slots := []Earlier{{Mock: "mock-2", At: at.Add(-time.Second), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.s[0]", Noise: "body.s", Type: String, Value: "2026-10-22T09:00:00Z"}, Live: "2026-10-22T11:15:00Z"},
		{Side: Response, Leaf: Leaf{Path: "body.s[1]", Noise: "body.s", Type: String, Value: "2026-10-22T09:30:00Z"}, Live: "2026-10-22T11:15:00Z"},
	}}}
	for _, tc := range []struct {
		name      string
		rec, live Shape
		earlier   []Earlier
		want      string
	}{
		{"served leaves", str("x-aaa1-ccc3"), str("x-bbb2-ddd4"), leaves, "in mock-1 at body.b"},
		{"the test case's request", str("2026-10-22T11:14:00Z"), str("none"), request, "test case test-1 at body.slots[1]"},
		{"F's moment of a typed instant", typed(time.Date(2026, 10, 22, 11, 14, 0, 0, time.UTC)), typed(time.Date(2026, 10, 22, 11, 15, 0, 0, time.UTC)), slots, "in mock-2 at body.s[0]"},
	} {
		seen := map[string]int{}
		for i := 0; i < 200; i++ {
			ln := ClassifyIn(clockedDrifts(), []string{p}, tc.live, tc.rec, at, tc.earlier, nil, nil, nil)[0]
			seen[string(ln.Kind)+" | "+ln.From.Mock+" | "+ln.From.Path+" | "+ln.Why]++
			if i == 0 && !strings.Contains(ln.Why, tc.want) {
				t.Errorf("%s: %s (%s); want it to name %q", tc.name, ln.Kind, ln.Why, tc.want)
			}
		}
		if len(seen) != 1 {
			t.Errorf("%s: %d outcomes for one input: %v", tc.name, len(seen), seen)
		}
	}
}

// clockWordsOfUUID7 is the time groups of uuid7(at, ...) then of uuid7(f,
// ...) that differ: the words ClockWordsOf names for a UUID v7 the lead moved.
func clockWordsOfUUID7(at, f time.Time) []string {
	r := strings.Split(uuid7(at, "abc123456789abcdef"), "-")[:2]
	l := strings.Split(uuid7(f, "def0123456789abcde"), "-")[:2]
	var out []string
	for _, side := range [][2][]string{{r, l}, {l, r}} {
		for i, w := range side[0] {
			if w != side[1][i] {
				out = append(out, w)
			}
		}
	}
	return out
}
