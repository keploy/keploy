package http

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// Word-level learn at the HTTP parser (design r9): what the learn step
// learns of a leaf is the words of it that changed in F, and every serve
// site reads them through learn.WordsAdmit, so every other byte of the leaf
// is still compared.

// withWords is m with the words F learns of a leaf at place, recorded rec
// and seen f in record's full round.
func withWords(t *testing.T, m *models.Mock, place, rec, f string) *models.Mock {
	t.Helper()
	m.Spec.LearnedWords = models.MergeWords(m.Spec.LearnedWords, map[string]models.WordsEntry{place: *wordsOf(t, rec, f)})
	return m
}

// multipartNote is a multipart form whose field note holds notes, a part
// each.
func multipartNote(boundary string, notes ...string) (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.SetBoundary(boundary)
	for _, note := range notes {
		_ = w.WriteField("note", note)
	}
	_ = w.Close()
	return "multipart/form-data; boundary=" + boundary, b.String()
}

// Every strict serve site reads a leaf's words (r9 §1, SF1): the path, a
// query value, a header, a form field, a multipart field and a JSON body
// field each let only the learned words change. The founder's case at each:
// a fresh ref is no difference, "paid 999" is. Words admit one value: where
// a call can send the field twice, the fresh ref with one more value beside
// it is refused, as with no words.
func TestEachStrictSiteReadsTheWords(t *testing.T) {
	const rec, f, fresh, regressed = "ORD-1 paid 100 ref=a1b2c3", "ORD-1 paid 100 ref=d4e5f6", "ORD-1 paid 100 ref=0f9e8d", "ORD-1 paid 999 ref=0f9e8d"
	seg := func(s string) string { return strings.NewReplacer(" ", "-", "=", "-").Replace(s) }
	form := func(s string) string { return "note=" + url.QueryEscape(s) }
	jsonOf := func(s string) string { return `{"note":"` + s + `"}` }
	for _, tc := range []struct {
		name  string
		place string
		mock  func(v string) *models.Mock
		call  func(v string) *req
		value func(v string) string
		// twice is the call sending the field twice, a then b (nil where
		// a call cannot).
		twice func(a, b string) *req
		// recTwice is the mock recorded with the field twice, a then b
		// (nil where the site compares a repeated field whole).
		recTwice func(a, b string) *models.Mock
	}{
		{"a path segment", "url.path.2",
			func(v string) *models.Mock { return recorded("m", "/v1/orders/"+seg(v), nil, "") },
			func(v string) *req { return sent("/v1/orders/"+seg(v), nil, "") }, seg, nil, nil},
		{"a query value", "url.query.note",
			func(v string) *models.Mock { return recorded("m", "/v1/orders?note="+url.QueryEscape(v), nil, "") },
			func(v string) *req { return sent("/v1/orders?note="+url.QueryEscape(v), nil, "") }, func(v string) string { return v },
			func(a, b string) *req {
				return sent("/v1/orders?note="+url.QueryEscape(a)+"&note="+url.QueryEscape(b), nil, "")
			},
			func(a, b string) *models.Mock {
				return recorded("m", "/v1/orders?note="+url.QueryEscape(a)+"&note="+url.QueryEscape(b), nil, "")
			}},
		{"a header", "header.x-order-ref",
			func(v string) *models.Mock {
				return recorded("m", "/v1/orders", map[string]string{"X-Order-Ref": v}, "")
			},
			func(v string) *req { return sent("/v1/orders", map[string]string{"X-Order-Ref": v}, "") }, func(v string) string { return v },
			func(a, b string) *req {
				in := sent("/v1/orders", map[string]string{"X-Order-Ref": a}, "")
				in.header.Add("X-Order-Ref", b)
				return in
			}, nil},
		{"a form field", "body.note",
			func(v string) *models.Mock {
				return recorded("m", "/v1/orders", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, form(v))
			},
			func(v string) *req {
				return sent("/v1/orders", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, form(v))
			}, func(v string) string { return v },
			func(a, b string) *req {
				return sent("/v1/orders", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, form(a)+"&"+form(b))
			},
			func(a, b string) *models.Mock {
				return recorded("m", "/v1/orders", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, form(a)+"&"+form(b))
			}},
		{"a multipart field", "body.note",
			func(v string) *models.Mock {
				ct, body := multipartNote("AAA", v)
				return recorded("m", "/v1/orders", map[string]string{"Content-Type": ct}, body)
			},
			func(v string) *req {
				ct, body := multipartNote("BBB", v)
				return sent("/v1/orders", map[string]string{"Content-Type": ct}, body)
			}, func(v string) string { return v },
			func(a, b string) *req {
				ct, body := multipartNote("BBB", a, b)
				return sent("/v1/orders", map[string]string{"Content-Type": ct}, body)
			}, nil},
		{"a JSON body field", "body.note",
			func(v string) *models.Mock {
				return recorded("m", "/v1/orders", map[string]string{"Content-Type": "application/json"}, jsonOf(v))
			},
			func(v string) *req {
				return sent("/v1/orders", map[string]string{"Content-Type": "application/json"}, jsonOf(v))
			},
			func(v string) string { return v }, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withWords(t, tc.mock(rec), tc.place, tc.value(rec), tc.value(f))
			if served, diag := strictMatch(t, tc.call(fresh), nil, nil, nil, m); served == nil {
				t.Fatalf("a fresh ref at %s is refused: %+v", tc.place, diag)
			}
			if served, _ := strictMatch(t, tc.call(regressed), nil, nil, nil, withWords(t, tc.mock(rec), tc.place, tc.value(rec), tc.value(f))); served != nil {
				t.Fatalf("paid 999 at %s is served: a word F did not see change changed", tc.place)
			}
			if tc.twice != nil {
				// The first value is one the words admit; the second is
				// one more value the recording never held.
				for _, extra := range []string{"EXTRA", tc.value(fresh)} {
					if served, _ := strictMatch(t, tc.twice(fresh, extra), nil, nil, nil, withWords(t, tc.mock(rec), tc.place, tc.value(rec), tc.value(f))); served != nil {
						t.Fatalf("a fresh ref at %s sent with %q beside it is served: words admit one value", tc.place, extra)
					}
				}
			}
			if tc.recTwice != nil {
				// Recorded twice, the field is two values, and words (here
				// of its first) admit none of them sent once.
				if served, _ := strictMatch(t, tc.call(fresh), nil, nil, nil, withWords(t, tc.recTwice(rec, "EXTRA"), tc.place, tc.value(rec), tc.value(f))); served != nil {
					t.Fatalf("a fresh ref at %s, recorded with one more value beside it, is served: words admit one value", tc.place)
				}
			}
			// The words are read only under the switch (knownOf): against
			// the recording's own destination, so only the words decide, a
			// fresh ref is no difference with the switch on and one with it
			// off.
			dest := recordedDestination(m)
			if diffs := strictDiff(tc.call(fresh), dest, m, strictNoise{}); len(diffs) != 0 {
				t.Fatalf("with the switch on, a fresh ref at %s differs: %+v", tc.place, fieldDiffs(diffs))
			}
			t.Setenv(provenswitch.Env, "")
			if diffs := strictDiff(tc.call(fresh), dest, m, strictNoise{}); len(diffs) == 0 {
				t.Fatalf("the switch off read the words at %s", tc.place)
			}
		})
	}
}

// A form key sent once is one value, and one sent twice is two: the words of
// a key recorded once ("ORD-1 paid, ref=a1b2c3") admit only one value, by
// the matcher's strict serve and by the learn step's own pass, so the same
// text sent as two values ("ORD-1 paid" and "ref=..."), its ref fresh or as
// recorded, is refused, as with no words, and so is a value they admit sent
// with one more beside it. A key recorded twice is compared whole: the learn
// step learns no words of it.
func TestAFormKeySentTwiceIsNotOneValue(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	const note, noteF = "ORD-1 paid, ref=a1b2c3", "ORD-1 paid, ref=d4e5f6"
	one := func(v string) string { return "note=" + url.QueryEscape(v) }
	two := func(a, b string) string { return one(a) + "&" + one(b) }
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"one value, a fresh ref", one("ORD-1 paid, ref=0f9e8d"), true},
		{"two values, a fresh ref", two("ORD-1 paid", "ref=0f9e8d"), false},
		{"two values, as recorded", two("ORD-1 paid", "ref=a1b2c3"), false},
		{"a fresh ref, one more value beside it", two("ORD-1 paid, ref=0f9e8d", "EXTRA"), false},
		{"as recorded, then a fresh ref", two(note, "ORD-1 paid, ref=0f9e8d"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withWords(t, recorded("m", "/v1/orders", ct, one(note)), "body.note", note, noteF)
			if served, diag := strictMatch(t, sent("/v1/orders", ct, tc.body), nil, nil, nil, m); (served != nil) != tc.ok {
				t.Errorf("strict serve: served %v (%+v), want %v", served != nil, diag, tc.ok)
			}
			h, n, _ := learnOn(t)
			lm := withWords(t, learnRecorded("mock-0", 0, "POST", "/v1/orders", one(note), ct, `{"ok":true}`), "body.note", note, noteF)
			served, err := h.learnedMatch(context.Background(), live("POST", "/v1/orders", tc.body, ct), &learnDb{perTest: []*models.Mock{lm}}, n, models.OutgoingOptions{})
			if err != nil || (served != nil) != tc.ok {
				t.Errorf("the learn step's pass: served %v (%v), want %v", served != nil, err, tc.ok)
			}
		})
	}
	// Recorded twice and drifted in F: compared whole, so refused, and no
	// words are learned of it.
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/orders", two("ORD-1", "ref-a1"), ct, `{"ok":true}`)}}
	if served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders", two("ORD-1", "ref-b2"), ct), db, n, opts); err != nil || served != nil || miss == nil {
		t.Fatalf("a key recorded twice: served %v miss %+v err %v, want refused", served, miss, err)
	}
}

// A key recorded twice holds a set of values: sent in another order, the
// call is the recorded one, to the strict serve (which reads them as
// members) and to the learn step alike (its shape reads them sorted), for a
// form key and a query parameter; another set of values is refused by both.
func TestAKeySentTwiceInAnotherOrderIsTheRecordedCall(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for _, tc := range []struct {
		name, method, target, body string
		header                     map[string]string
		// sent is the call with its values as given.
		sent func(a, b string) (target, body string)
	}{
		{"a form key", "POST", "/v1/orders", "note=ORD-1&note=ref-a1", ct,
			func(a, b string) (string, string) { return "/v1/orders", "note=" + a + "&note=" + b }},
		{"a query parameter", "GET", "/v1/orders?note=ORD-1&note=ref-a1", "", nil,
			func(a, b string) (string, string) { return "/v1/orders?note=" + a + "&note=" + b, "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []struct {
				a, b string
				ok   bool
			}{
				{"ref-a1", "ORD-1", true},
				{"ORD-1", "ref-a1", true},
				{"ref-b2", "ORD-1", false},
			} {
				target, body := tc.sent(c.a, c.b)
				h, n, opts := learnOn(t)
				db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, tc.method, tc.target, tc.body, tc.header, `{"ok":true}`)}}
				served, miss, err := h.learnMatch(context.Background(), live(tc.method, target, body, tc.header), db, n, opts)
				if err != nil || (served != nil) != c.ok {
					t.Errorf("%s, %s: the learn step served %v (miss %+v, err %v), want %v", c.a, c.b, served != nil, miss, err, c.ok)
				}
				rec := recorded("m", tc.target, tc.header, tc.body)
				in := sent(target, tc.header, body)
				in.method = tc.method
				rec.Spec.HTTPReq.Method = models.Method(tc.method)
				if served, diag := strictMatch(t, in, nil, nil, nil, rec); (served != nil) != c.ok {
					t.Errorf("%s, %s: the strict serve served %v (%+v), want %v", c.a, c.b, served != nil, diag, c.ok)
				}
			}
		})
	}
}

// A leaf whose type changed (the string "100" sent as the number 300) is
// refused at a words place by every serve site, whatever its words admit:
// the learn step's own pass (learn.Compare reads the type first), the
// variable pass beside a variable (fitVariables' words loop), and the strict
// serve (jsonDiffs). The same value as a string is served by each.
func TestATypeChangeAtAWordsPlaceIsRefused(t *testing.T) {
	for _, tc := range []struct {
		amt string
		ok  bool
	}{
		{`"300"`, true},
		{`300`, false},
	} {
		h, n, _ := learnOn(t)
		lm := withWords(t, learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"amt":"100","note":"x"}`, nil, `{"ok":true}`), "body.amt", "100", "200")
		served, err := h.learnedMatch(context.Background(), live("POST", "/v1/charges", `{"amt":`+tc.amt+`,"note":"x"}`, nil), &learnDb{perTest: []*models.Mock{lm}}, n, models.OutgoingOptions{})
		if err != nil || (served != nil) != tc.ok {
			t.Errorf("amt %s, the learn step's pass: served %v (%v), want %v", tc.amt, served != nil, err, tc.ok)
		}

		vm := varMock("m", "POST", "/v1/orders", `{"id":"k-1","amt":"100"}`, 200, `{"id":"k-1"}`,
			models.MockVariables{"id": {Recorded: "k-1", Bind: "body.id", Echo: true, Answer: []string{"body.id"}, Words: wordsOf(t, "k-1", "k-2")}})
		vm = withWords(t, vm, "body.amt", "100", "200")
		if ok, _, diag := matchVar(t, newHTTP(), &varDb{perTest: []*models.Mock{vm}}, live("POST", "/v1/orders", `{"id":"k-7","amt":`+tc.amt+`}`, map[string]string{"Host": ""})); ok != tc.ok {
			t.Errorf("amt %s, the variable pass: served %v (%+v), want %v", tc.amt, ok, diag, tc.ok)
		}

		sm := withWords(t, recorded("m", "/v1/charges", map[string]string{"Content-Type": "application/json"}, `{"amt":"100","note":"x"}`), "body.amt", "100", "200")
		if served, diag := strictMatch(t, sent("/v1/charges", map[string]string{"Content-Type": "application/json"}, `{"amt":`+tc.amt+`,"note":"x"}`), nil, nil, nil, sm); (served != nil) != tc.ok {
			t.Errorf("amt %s, the strict serve: served %v (%+v), want %v", tc.amt, served != nil, diag, tc.ok)
		}
	}
}

// tiePlaces refuses two places of a call that hold one value, recorded and
// now, but were learned apart (noise beside an echo, a variable beside an
// echo): it cannot follow them as one value. learnPlan gives it no such pair
// today: Classify reads a leaf by its two values alone, so every place of
// one recorded and one live value is learned alike, and an echo or a
// variable of one value takes one name. The refusal is tiePlaces' own
// contract, for whatever learnPlan gives it.
func TestTiePlacesRefusesTwinsLearnedApart(t *testing.T) {
	leaf := func(path string) learn.Learned {
		return learn.Learned{Recorded: learn.Leaf{Path: path, Type: learn.String, Value: "o-1111"}, Live: learn.Leaf{Path: path, Type: learn.String, Value: "o-9999"}}
	}
	for _, tc := range []struct {
		name string
		a, b slotPlace
		ok   bool
	}{
		{"noise and an echo", slotPlace{l: leaf("body.a"), kind: learn.KindNoise}, slotPlace{l: leaf("body.b"), kind: learn.KindEcho, name: "b"}, false},
		{"a variable and an echo", slotPlace{l: leaf("body.a"), kind: learn.KindVariable, name: "a"}, slotPlace{l: leaf("body.b"), kind: learn.KindEcho, name: "b"}, false},
		{"one echo at both", slotPlace{l: leaf("body.a"), kind: learn.KindEcho, name: "a"}, slotPlace{l: leaf("body.b"), kind: learn.KindEcho, name: "a"}, true},
	} {
		p := pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{}}
		why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{tc.a, tc.b}, nil, true, map[string]bool{}, nil)
		if tc.ok {
			if why != "" {
				t.Errorf("%s: refused: %s", tc.name, why)
			}
			continue
		}
		if !strings.Contains(why, "learned them apart") || p.refused != models.RefusedTie {
			t.Errorf("%s: %q (refused %q), want refused as two values learned apart", tc.name, why, p.refused)
		}
	}
}

// Two groups of twins whose binds name one variable name each are named in
// the places' sorted order, so a run names them alike every time: the
// first in sorted order takes the name, the other the next free one.
func TestTwinGroupsAreNamedInTheirPlacesOrder(t *testing.T) {
	leaf := func(path, rec, live string) learn.Learned {
		return learn.Learned{Kind: learn.KindNoise, Recorded: learn.Leaf{Path: path, Type: learn.String, Value: rec}, Live: learn.Leaf{Path: path, Type: learn.String, Value: live}}
	}
	slots := []slotPlace{
		{l: leaf("header.x-b", "q-7777", "q-3333"), kind: learn.KindNoise},
		{l: leaf("body.item.id", "q-7777", "q-3333"), kind: learn.KindNoise},
		{l: leaf("header.x-a", "o-1111", "o-9999"), kind: learn.KindNoise},
		{l: leaf("body.id", "o-1111", "o-9999"), kind: learn.KindNoise},
	}
	for i := 0; i < 20; i++ {
		p := pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{"mock-0": {}}}
		for _, s := range slots {
			p.words[s.l.Recorded.Path] = *wordsOf(t, s.l.Recorded.Value, s.l.Live.Value)
			p.f["mock-0"][s.l.Recorded.Path] = s.l.Live.Value
		}
		tr := learn.NewTracker()
		if why := tiePlaces(tr, 0, &models.Mock{Name: "mock-0"}, &p, append([]slotPlace(nil), slots...), nil, true, map[string]bool{}, nil); why != "" {
			t.Fatalf("refused: %s", why)
		}
		if len(p.words) != 0 || len(p.vars) != 2 || p.vars["id"].Bind != "body.id" || p.vars["id-2"].Bind != "body.item.id" {
			t.Fatalf("run %d: words %v variables %+v, want id bound at body.id and id-2 at body.item.id", i, p.words, p.vars)
		}
		if !reflect.DeepEqual(p.vars["id"].Use, []string{"header.x-a"}) || !reflect.DeepEqual(p.vars["id-2"].Use, []string{"header.x-b"}) {
			t.Fatalf("run %d: uses %v and %v", i, p.vars["id"].Use, p.vars["id-2"].Use)
		}
		if want := map[string]string{"body.id": "o-9999", "body.item.id": "q-3333"}; !reflect.DeepEqual(p.f["mock-0"], want) {
			t.Fatalf("run %d: F's values %v, want %v", i, p.f["mock-0"], want)
		}
	}
}

// A replay after F honours the words F learned before the matcher (the
// learn step's own pass, learnedMatch): a fresh ref is served, a regressed
// amount goes on to the matcher, which refuses it too.
func TestLearnedMatchServesByTheWords(t *testing.T) {
	h, n, _ := learnOn(t)
	rec := `{"note":"ORD-1 paid 100 ref=a1b2c3"}`
	mock := func() *models.Mock {
		m := learnRecorded("mock-0", 0, "POST", "/v1/charges", rec, nil, `{"ok":true}`)
		return withWords(t, m, "body.note", "ORD-1 paid 100 ref=a1b2c3", "ORD-1 paid 100 ref=d4e5f6")
	}
	db := &learnDb{perTest: []*models.Mock{mock()}}
	if served, err := h.learnedMatch(context.Background(), live("POST", "/v1/charges", `{"note":"ORD-1 paid 100 ref=0f9e8d"}`, nil), db, n, models.OutgoingOptions{}); err != nil || served == nil {
		t.Fatalf("a fresh ref: served %v err %v", served, err)
	}
	db = &learnDb{perTest: []*models.Mock{mock()}}
	if served, _ := h.learnedMatch(context.Background(), live("POST", "/v1/charges", `{"note":"ORD-1 paid 999 ref=0f9e8d"}`, nil), db, n, models.OutgoingOptions{}); served != nil {
		t.Fatal("paid 999 served by the learn step's pass")
	}
}

// A variable's words are learned in the window of the call that sent its
// value at its bind (learn.Place.Window), as that call learned its own
// there: a clock read made in the bind call's window is an instant slot of
// the variable, though the lead does not read it, nor does the later call's
// window (it was read before that call's previous one).
func TestAVariableLearnsItsWordsInItsBindCallsWindow(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	h, n, opts := learnOn(t)
	prev := learnRecorded("mock-p", 0, "GET", "/v1/health", "", nil, `{}`)
	recRead := learnAt.Add(time.Millisecond) // in the bind's recorded span, before the use's
	bind := learnRecorded("mock-0", 2*time.Millisecond, "POST", "/v1/orders", `{"placed_at":"`+iso(recRead)+`"}`, nil, `{}`)
	use := learnRecorded("mock-1", 4*time.Millisecond, "POST", "/v1/receipts", `{"order_at":"`+iso(recRead)+`"}`, nil, `{}`)
	db := &learnDb{perTest: []*models.Mock{bind, use}, staged: []*models.Mock{prev}}
	tr := learn.NewTracker()
	tr.Begin()
	offset := learn.FullRoundLead + 2*time.Second - 40*time.Second // F's clock: not moved by the lead
	tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
	servedPrev := learnAt.Add(40 * time.Second)
	tr.ServedMockAt(prev, servedPrev)
	fRead := servedPrev.Add(offset + time.Millisecond) // in the bind's F span, before the use's
	ctx := learn.WithTracker(context.Background(), tr)

	first := live("POST", "/v1/orders", `{"placed_at":"`+iso(fRead)+`"}`, nil)
	first.arrived = servedPrev.Add(2 * time.Millisecond)
	if served, miss, err := h.learnMatch(ctx, first, db, n, opts); err != nil || served == nil {
		t.Fatalf("the bind's call: served %v miss %+v err %v", served, miss, err)
	}
	tr.ServedMockAt(bind, first.arrived.Add(time.Millisecond))
	then := live("POST", "/v1/receipts", `{"order_at":"`+iso(fRead)+`"}`, nil)
	then.arrived = first.arrived.Add(3 * time.Millisecond)
	served, miss, err := h.learnMatch(ctx, then, db, n, opts)
	if err != nil || served == nil {
		t.Fatalf("the call that uses it: served %v miss %+v err %v", served, miss, err)
	}
	var got *models.WordsEntry
	for i := range db.consumed {
		if db.consumed[i].Name != "mock-1" {
			continue
		}
		for _, v := range db.consumed[i].Spec.LearnedBinds()["mock-0"] {
			got = v.Words
		}
	}
	if got == nil || len(got.Slots) != 1 || got.Slots[0].Kind != models.WordSlotInstant {
		t.Fatalf("the variable's words %+v, want one instant slot", got)
	}
}

// Excused never reports a words place: Excused sees one value, so a place
// whose words were learned is decided by ExcusedLive alone, and prune (which
// reads Excused) never prunes it.
func TestExcusedNeverSeesAWordsPlace(t *testing.T) {
	e := wordsOf(t, "ORD-1 ref=a1", "ORD-1 ref=b2")
	k := httpKnown{words: map[string]models.WordsEntry{"body.note": *e}}
	if excused, _ := k.Excused(learn.Leaf{Path: "body.note", Noise: "body.note", Type: learn.String, Value: "ORD-1 ref=a1"}); excused {
		t.Fatal("Excused reported a words place")
	}
	rec := learn.Leaf{Path: "body.note", Noise: "body.note", Type: learn.String, Value: "ORD-1 ref=a1"}
	if ok, typed := k.ExcusedLive(rec, learn.Leaf{Path: "body.note", Type: learn.String, Value: "ORD-1 ref=c3"}); !ok || !typed {
		t.Fatal("ExcusedLive refused a changed slot")
	}
	if ok, _ := k.ExcusedLive(rec, learn.Leaf{Path: "body.note", Type: learn.String, Value: "ORD-2 ref=c3"}); ok {
		t.Fatal("ExcusedLive admitted a changed fixed word")
	}
	if ok, _ := k.ExcusedLive(learn.Leaf{Path: "body.other", Value: "a"}, learn.Leaf{Path: "body.other", Value: "b"}); ok {
		t.Fatal("ExcusedLive excused a place with no words")
	}
	// Compare reads it: one drift, the regressed word.
	shape := func(v string) learn.Shape {
		return learn.Shape{Key: "k", Leaves: map[string]learn.Leaf{"body.note": {Path: "body.note", Noise: "body.note", Type: learn.String, Value: v}}}
	}
	if d := learn.Compare(shape("ORD-1 ref=z9"), shape("ORD-1 ref=a1"), k); len(d.Drift) != 0 {
		t.Fatalf("Compare: %+v", d)
	}
	if d := learn.Compare(shape("ORD-9 ref=z9"), shape("ORD-1 ref=a1"), k); len(d.Drift) != 1 {
		t.Fatalf("Compare: %+v", d)
	}
}

// A mock that learned the words of a path segment (or of the target) is keyed
// by its other parts in the not-owned index (r9 §1): a call of another path
// asks by the learned key, which the recording is under.
func TestAPathWordMockIsKeyedByItsOtherParts(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := recorded("m", "/v1/orders/ORD-1-a1", nil, "")
	if keys := recordedRequestKeys(m); !reflect.DeepEqual(keys, []string{requestKeyShape("POST", 3), requestKeyExact("POST", []string{"v1", "orders", "ORD-1-a1"}, "")}) {
		t.Fatalf("a mock with no words: %v", keys)
	}
	withWords(t, m, "url.path.2", "ORD-1-a1", "ORD-1-b2")
	keys := recordedRequestKeys(m)
	if !reflect.DeepEqual(keys, []string{requestKeyShape("POST", 3), requestKeyLearned("POST", 3)}) {
		t.Fatalf("a path-word mock: %v", keys)
	}
	live := RequestKeysOf("POST", "/v1/orders/ORD-1-c3", "", false)
	found := false
	for _, k := range live {
		for _, r := range keys {
			found = found || k == r
		}
	}
	if !found {
		t.Fatalf("a call of a fresh segment %v asks by no key the recording is under %v", live, keys)
	}
}

// The variable pass admits a bind only through the words learned there (r9
// §4), in both of holdVariables' branches: an echo's, and a variable bound
// here first. A variable learned before words binds any value (N6).
func TestAVariableBindsOnlyAValueItsWordsAdmit(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec, f := "ord-1-a1b2", "ord-1-c3d4"
	for _, echo := range []bool{true, false} {
		mock := func(words bool) *models.Mock {
			v := models.MockVariable{Recorded: rec, Bind: "body.id", Echo: echo, Answer: []string{"body.id"}}
			if words {
				v.Words = wordsOf(t, rec, f)
			}
			return varMock("m", "POST", "/v1/orders", `{"id":"`+rec+`"}`, 200, `{"id":"`+rec+`"}`, models.MockVariables{"id": v})
		}
		call := func(v string) *req {
			return live("POST", "/v1/orders", `{"id":"`+v+`"}`, map[string]string{"Host": ""})
		}
		for _, tc := range []struct {
			live  string
			words bool
			ok    bool
		}{
			{"ord-1-e5f6", true, true},
			{"ord-2-e5f6", true, false},
			{"xyz", true, false},
			{"ord-2-e5f6", false, true}, // learned before words: any value binds (N6)
		} {
			db := &varDb{perTest: []*models.Mock{mock(tc.words)}}
			ok, stub, diag := matchVar(t, newHTTP(), db, call(tc.live))
			if ok != tc.ok {
				t.Errorf("echo %v, words %v: %q served %v (%+v), want %v", echo, tc.words, tc.live, ok, diag, tc.ok)
				continue
			}
			if ok && !strings.Contains(stub.Spec.HTTPResp.Body, tc.live) {
				t.Errorf("echo %v: the answer %s does not hold the bound value", echo, stub.Spec.HTTPResp.Body)
			}
			if !ok && tc.words {
				if p := wantProblem(t, ok, diag, "changed only where record's full round saw it change"); p.place != "body.id" {
					t.Errorf("echo %v: the problem names %q", echo, p.place)
				}
			}
		}
	}
}

// formCT is a form body's content type.
var formCT = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

// The learn step's ties between the places it learned words at (r9 §2, §8):
// twins are one call-local variable, instants are exempt, two values equal
// only when recorded are two values, and a partial tie, a glued piece, an
// in-leaf repeat and a test case's value inside a place are refused (N8).
func TestTheLearnPlansTies(t *testing.T) {
	later := learnAt.Add(learn.FullRoundLead)
	v7 := func(at time.Time, r string) string {
		h := fmt.Sprintf("%012x", at.UnixMilli())
		return h[:8] + "-" + h[8:12] + "-7" + r[:3] + "-a" + r[3:6] + "-" + r[6:18]
	}
	rv7, fv7 := v7(learnAt, "abc123456789abcdef"), v7(later, "def0123456789abcde")
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		rec, f   string
		fpath    string
		given    []variables.Binding
		ct       map[string]string
		refused  string
		class    string
		words    []string
		variable *models.MockVariable
		// varName is the name of the one call-local variable learned.
		varName string
	}{
		{name: "a partial tie", method: "POST", path: "/a", rec: `{"note":"ORD-123","id":"123"}`, f: `{"note":"ORD-456","id":"456"}`, refused: "(N8)", class: models.RefusedTie},
		{name: "a piece glued to a word", method: "POST", path: "/a", rec: `{"note":"ORD123","id":"123"}`, f: `{"note":"ORD456","id":"456"}`, refused: "(N8)", class: models.RefusedContainment},
		{name: "a piece in two places", method: "POST", path: "/a", rec: `{"inv":"INV1042","ord":"ORD1042"}`, f: `{"inv":"INV2077","ord":"ORD2077"}`, refused: "(N8)", class: models.RefusedContainment},
		{name: "an end-aligned piece", method: "POST", path: "/a", rec: `{"p":"p12x1042","id":"1042"}`, f: `{"p":"p9x5678","id":"5678"}`, refused: "(N8)", class: models.RefusedContainment},
		{name: "glued in one leaf", method: "POST", path: "/a", rec: `{"note":"Invoice INV1042 for order 1042"}`, f: `{"note":"Invoice INV2077 for order 2077"}`, refused: "glued inside another", class: models.RefusedContainment},
		{name: "a repeat in one leaf", method: "POST", path: "/a", rec: `{"note":"id=x1 ref=x1"}`, f: `{"note":"id=y2 ref=y2"}`, refused: "repeats", class: models.RefusedTie},
		{name: "a test case's value inside a place", method: "POST", path: "/a", rec: `{"note":"note ORD1042"}`, f: `{"note":"note ORD2077"}`,
			given: []variables.Binding{{Name: "inv", Recorded: "INV1042", Value: "INV2077", Given: true}}, refused: "(N8)", class: models.RefusedContainment},
		{name: "a test case's value as a word of a place", method: "POST", path: "/a", rec: `{"note":"note INV1042 paid"}`, f: `{"note":"note INV2077 paid"}`,
			given: []variables.Binding{{Name: "inv", Recorded: "INV1042", Value: "INV2077", Given: true}}, refused: "(N8)", class: models.RefusedTie},
		// Twins are one variable of the call, which binds only at a place a
		// variable can stand: two form fields that held one value are
		// refused, never learned apart (each place's own words would let
		// the two take two values).
		{name: "twins in a form", method: "POST", path: "/a", rec: "a=k1x9q2&b=k1x9q2", f: "a=z7m3w8&b=z7m3w8", ct: formCT, refused: "at a place keploy cannot bind it (a form or multipart field)", class: models.RefusedTie},
		{name: "twins, a JSON field and one of an empty key", method: "POST", path: "/a", rec: `{"a":"o-1111","b":{"":"o-1111"}}`, f: `{"a":"o-9999","b":{"":"o-9999"}}`, refused: "cannot follow a variable to", class: models.RefusedTie},
		{name: "no tie rule: a separator changed", method: "POST", path: "/a", rec: `{"note":"a, b"}`, f: `{"note":"a ,b"}`, refused: "no word in it is new", class: ""},
		{name: "createdAt = updatedAt", method: "POST", path: "/a",
			rec:   `{"createdAt":"` + learnAt.Format(time.RFC3339) + `","updatedAt":"` + learnAt.Format(time.RFC3339) + `"}`,
			f:     `{"createdAt":"` + later.Format(time.RFC3339) + `","updatedAt":"` + later.Format(time.RFC3339) + `"}`,
			words: []string{"body.createdAt", "body.updatedAt"}},
		{name: "equal only when recorded", method: "POST", path: "/a", rec: `{"a":"x-1","b":"x-1"}`, f: `{"a":"x-2","b":"x-3"}`, words: []string{"body.a", "body.b"}},
		// Two values that were two when recorded and one in F are two
		// values (neither carries the other): each is learned apart.
		{name: "equal only in F", method: "POST", path: "/a", rec: `{"a":"a1x","b":"b2y"}`, f: `{"a":"z9q","b":"z9q"}`, words: []string{"body.a", "body.b"}},
		{name: "seq beside a fresh hex", method: "POST", path: "/a", rec: `{"note":"seq=17","id":"3f2b8c1e9a4d70a1"}`, f: `{"note":"seq=18","id":"a7c4e2d10b3f4c2e"}`, words: []string{"body.id", "body.note"}},
		// Twins are named by their first place, past the test case's keys
		// (here "id", which the twins' value does not carry).
		{name: "twins named past a test case's key", method: "POST", path: "/a", rec: `{"id":"o-1111","ref":"o-1111"}`, f: `{"id":"o-9999","ref":"o-9999"}`,
			given: []variables.Binding{{Name: "id", Recorded: "q-77", Value: "q-88", Given: true}}, varName: "id-2"},
		{name: "a UUID v7 in the path and the body", method: "PUT", path: "/items/" + rv7, fpath: "/items/" + fv7, rec: `{"id":"` + rv7 + `"}`, f: `{"id":"` + fv7 + `"}`,
			variable: &models.MockVariable{Recorded: rv7, Bind: "body.id", Use: []string{"url.path[1]"}, Echo: true, Words: wordsOf(t, rv7, fv7)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			m := learnRecorded("mock-0", 0, tc.method, tc.path, tc.rec, tc.ct, `{"ok":true}`)
			fpath := tc.fpath
			if fpath == "" {
				fpath = tc.path
			}
			db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{m}}}
			if tc.given != nil {
				db.table.Start(tc.given)
			}
			served, miss, err := h.learnMatch(context.Background(), live(tc.method, fpath, tc.f, tc.ct), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refused != "" {
				if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, tc.refused) {
					t.Fatalf("served %v miss %+v, want refused saying %q", served, miss, tc.refused)
				}
				// The rule that refused it, on the miss's report
				// (models.UnmatchedCall.Refused): what E-measure counts.
				var report models.MockMismatchReport
				miss.explain(&report)
				if miss.res.Refused != tc.class || report.Refused != tc.class {
					t.Fatalf("refused as %q (report %q), want %q", miss.res.Refused, report.Refused, tc.class)
				}
				return
			}
			if served == nil || miss != nil {
				t.Fatalf("refused: %+v", miss)
			}
			got := db.consumed[0].Spec
			places := make([]string, 0, len(got.LearnedWords))
			for p := range got.LearnedWords {
				places = append(places, p)
			}
			if tc.words != nil && !reflect.DeepEqual(sortedCopyOf(places), tc.words) {
				t.Fatalf("words at %v, want %v", places, tc.words)
			}
			if tc.varName != "" {
				if _, ok := got.Learned()[tc.varName]; !ok || len(got.Learned()) != 1 {
					t.Fatalf("variables %+v, want one named %s", got.Learned(), tc.varName)
				}
			}
			if tc.variable != nil {
				if len(got.LearnedWords) != 0 || len(got.Learned()) != 1 {
					t.Fatalf("words %v variables %+v, want one call-local variable", got.LearnedWords, got.Learned())
				}
				for _, v := range got.Learned() {
					if !reflect.DeepEqual(v, *tc.variable) {
						t.Fatalf("variable %+v, want %+v", v, *tc.variable)
					}
				}
				// F's value stays at the variable's bind alone (its words'
				// leaf in learned.local.yaml); a place it is used at holds
				// none of its own.
				if want := map[string]map[string]string{"mock-0": {"body.id": fv7}}; !reflect.DeepEqual(got.LearnedF(), want) {
					t.Fatalf("F's values %v, want %v", got.LearnedF(), want)
				}
				// Replayed: one fresh v7 at both places is served; two, or a
				// value that is no v7, is not.
				mocks := learnedOnto([]*models.Mock{learnRecorded("mock-0", 0, tc.method, tc.path, tc.rec, nil, `{"ok":true}`)}, db.consumed)
				for _, c := range []struct {
					path, id string
					ok       bool
				}{
					{v7(later.Add(time.Hour), "0123456789abcdef01"), v7(later.Add(time.Hour), "0123456789abcdef01"), true},
					{v7(later.Add(time.Hour), "0123456789abcdef01"), v7(later.Add(2*time.Hour), "0123456789abcdef01"), false},
					{"1b4e28ba-2fa1-41d2-883f-0016d3cca427", "1b4e28ba-2fa1-41d2-883f-0016d3cca427", false},
				} {
					vdb := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
					if ok, _, diag := matchVar(t, newHTTP(), vdb, live(tc.method, "/items/"+c.path, `{"id":"`+c.id+`"}`, nil)); ok != c.ok {
						t.Errorf("%s / %s: served %v (%+v), want %v", c.path, c.id, ok, diag, c.ok)
					}
				}
			}
		})
	}
}

// A value the recording's obfuscator redacted (Mock.Noise) is excused at
// learn as at serve: no words are learned of it.
func TestAnObfuscatedValueIsExcusedAtLearn(t *testing.T) {
	h, n, opts := learnOn(t)
	m := learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"token":"secret-a1","note":"ORD-1 ref=a1"}`, nil, `{"ok":true}`)
	m.Noise = []string{"^secret-[a-z0-9]+$"}
	db := &learnDb{perTest: []*models.Mock{m}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"token":"secret-b2","note":"ORD-1 ref=b2"}`, nil), db, n, opts)
	if err != nil || served == nil || miss != nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if got := db.consumed[0].Spec.LearnedWords; len(got) != 1 || got["body.note"].Digest == "" {
		t.Fatalf("words at %v, want body.note's alone", got)
	}
}

// Instants read within the call's window (its clock not moved by the lead)
// are clock reads wherever the call's words are learned: two places that
// hold one such instant are exempt (createdAt = updatedAt), each learned
// apart, and an echoed value beside one learns the instant as an instant
// slot.
func TestTheCallsWindowReadsItsInstantsEverywhere(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	h, n, opts := learnOn(t)
	prev := learnRecorded("mock-p", 0, "GET", "/v1/health", "", nil, `{}`)
	recRead := learnAt.Add(time.Millisecond)
	twins := learnRecorded("mock-0", 2*time.Millisecond, "POST", "/v1/orders", `{"created":"`+iso(recRead)+`","updated":"`+iso(recRead)+`"}`, nil, `{}`)
	echo := learnRecorded("mock-1", 2*time.Millisecond, "POST", "/v1/notes", `{"note":"ord-77 at `+iso(recRead)+`"}`, nil, `{"note":"ord-77 at `+iso(recRead)+`"}`)
	for _, tc := range []struct {
		name, path, body string
		mock             *models.Mock
	}{
		{"twins", "/v1/orders", `{"created":"%s","updated":"%s"}`, twins},
		{"an echo", "/v1/notes", `{"note":"ord-88 at %s"}`, echo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &learnDb{perTest: []*models.Mock{tc.mock}, staged: []*models.Mock{prev}}
			tr := learn.NewTracker()
			tr.Begin()
			offset := learn.FullRoundLead + 2*time.Second - 40*time.Second // F's clock: not moved by the lead
			tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
			servedPrev := learnAt.Add(40 * time.Second)
			tr.ServedMockAt(prev, servedPrev)
			fRead := iso(servedPrev.Add(offset + time.Millisecond))
			body := strings.ReplaceAll(tc.body, "%s", fRead)
			call := live("POST", tc.path, body, nil)
			call.arrived = servedPrev.Add(2 * time.Millisecond)
			served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), call, db, n, opts)
			if err != nil || served == nil {
				t.Fatalf("served %v miss %+v err %v", served, miss, err)
			}
			got := db.consumed[0].Spec
			if tc.mock == twins {
				if len(got.Learned()) != 0 || len(got.LearnedWords) != 2 {
					t.Fatalf("words %v variables %+v, want each instant learned apart", got.LearnedWords, got.Learned())
				}
				return
			}
			var words *models.WordsEntry
			for _, v := range got.Learned() {
				words = v.Words
			}
			if words == nil || len(words.Slots) != 2 || words.Slots[1].Kind != models.WordSlotInstant {
				t.Fatalf("the echo's words %+v, want a run and an instant slot", words)
			}
		})
	}
}

// A value an earlier call sent whole, which a later call sends whole where
// a variable can stand, is refused where keploy cannot learn its words
// there, saying why and by which rule (a changed word that repeats: a tie).
func TestAVariableWhoseWordsCannotBeLearnedIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	inv := learnRecorded("mock-0", 0, "POST", "/v1/invoices", `{"id":"x1-x1"}`, nil, `{"ok":true}`)
	inv.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	db := &learnDb{perTest: []*models.Mock{inv, learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"ref":"x1-x1"}`, nil, `{}`)}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/invoices", `{"id":"y2-y2"}`, nil), db, n, opts); served == nil {
		t.Fatalf("precondition: %+v", miss)
	}
	served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"ref":"y2-y2"}`, nil), db, n, opts)
	if served != nil || miss == nil || !strings.Contains(miss.res.Why, "keploy cannot tell which of its words changed alone there: "+"the same changed word repeats in it") {
		t.Fatalf("served %v miss %+v, want refused as a variable whose words cannot be learned", served, miss)
	}
	if miss.res.Refused != models.RefusedTie {
		t.Fatalf("refused as %q, want %q", miss.res.Refused, models.RefusedTie)
	}
}

// A value an earlier call of the test sent, which a later call sends inside
// a longer value, is refused (a variable keploy cannot follow there), and the
// miss names the rule that tied the two: containment when only the glued
// test (B1) finds the earlier value inside it, a tie when it stands there as
// a word.
func TestACrossCallRefusalNamesItsRule(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live, class string
	}{
		{"glued to a word", "ORD1042", "ORD2077", models.RefusedContainment},
		{"as a word", "ORD-1042", "ORD-2077", models.RefusedTie},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/invoices", `{"id":"1042"}`, nil, `{"ok":true}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"order":"`+tc.rec+`"}`, nil, `{}`),
			}}
			if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/invoices", `{"id":"2077"}`, nil), db, n, opts); served == nil {
				t.Fatalf("precondition: %+v", miss)
			}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"`+tc.live+`"}`, nil), db, n, opts)
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "inside a longer value") {
				t.Fatalf("served %v miss %+v, want refused as inside a longer value", served, miss)
			}
			var report models.MockMismatchReport
			miss.explain(&report)
			if miss.res.Refused != tc.class || report.Refused != tc.class {
				t.Fatalf("refused as %q (report %q), want %q", miss.res.Refused, report.Refused, tc.class)
			}
		})
	}
}

func sortedCopyOf(ss []string) []string {
	out := append([]string(nil), ss...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// The variable pass puts a recorded value back at a place whose words were
// learned only where the live value is admitted (fitVariables): beside a
// variable, a query value's fresh ref replays, and its "paid 999" does not.
func TestTheVariablePassReadsTheWordsBesideAVariable(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const note, noteF = "ORD-1 paid 100 ref=a1b2", "ORD-1 paid 100 ref=c3d4"
	mock := func() *models.Mock {
		m := varMock("m", "POST", "/v1/orders?note="+url.QueryEscape(note), `{"id":"k-1"}`, 200, `{"id":"k-1"}`,
			models.MockVariables{"id": {Recorded: "k-1", Bind: "body.id", Echo: true, Answer: []string{"body.id"}, Words: wordsOf(t, "k-1", "k-2")}})
		m.Spec.HTTPReq.URLParams = map[string]string{"note": note}
		return withWords(t, m, "url.query.note", note, noteF)
	}
	for _, tc := range []struct {
		note string
		ok   bool
	}{
		{"ORD-1 paid 100 ref=e5f6", true},
		{"ORD-1 paid 999 ref=e5f6", false},
		{"ORD-1 paid 100 ref=e5 f6", false},
	} {
		db := &varDb{perTest: []*models.Mock{mock()}}
		in := live("POST", "/v1/orders?note="+url.QueryEscape(tc.note), `{"id":"k-7"}`, map[string]string{"Host": ""})
		if ok, _, diag := matchVar(t, newHTTP(), db, in); ok != tc.ok {
			t.Errorf("%q: served %v (%+v), want %v", tc.note, ok, diag, tc.ok)
		}
	}
}

// The learn step's log says what each drifted leaf became: noise when its
// words are, the echo or the variable when the plan made the place one (a
// leaf Classify found noise is an echo's place when another place of the
// call holds its value whole), and HTTP registers itself as a kind whose
// leaves are learned by their words, so a leaf an HTTP mock holds whole is
// one learned before keploy learned words.
func TestTheLearnLogSaysWhatEachLeafBecame(t *testing.T) {
	leaf := func(path string, k learn.Kind) learn.Learned {
		return learn.Learned{Kind: k, Recorded: learn.Leaf{Path: path}}
	}
	p := pairing{
		words: map[string]models.WordsEntry{"body.note": {}},
		vars: models.MockVariables{
			"order_ref": {Bind: "body.order_ref", Use: []string{"header.X-Order-Ref"}, Echo: true},
			"id":        {Bind: "body.from", Use: []string{"url.path[1]"}},
		},
		binds: map[string]models.MockVariables{"mock-0": {"other": {Bind: "body.elsewhere"}}},
	}
	for _, tc := range []struct {
		l    learn.Learned
		want string
	}{
		{leaf("body.note", learn.KindNoise), "noise"},
		{leaf("body.order_ref", learn.KindNoise), "echo"},
		{leaf("header.x-order-ref", learn.KindNoise), "echo"},
		{leaf("url.path.1", learn.KindNoise), "variable"},
		{leaf("body.elsewhere", learn.KindNoise), "noise"},
		{leaf("body.k", learn.KindEcho), "echo"},
	} {
		if got := p.learnedAs(tc.l); got != tc.want {
			t.Errorf("%s: logged as %q, want %q", tc.l.Recorded.Path, got, tc.want)
		}
	}
	// A mock on disk holding an HTTP leaf whole, read as learned.yaml reads
	// it (the mock store's set reader, the HTTP leaf reader).
	dir := t.TempDir()
	ys := mockdb.New(zap.NewNop(), dir, "")
	m := learnRecorded("", 0, "POST", "/v1/orders", `{"at":"x"}`, nil, `{"ok":true}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
	if err := ys.InsertMock(context.Background(), m, "test-set-0"); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	leaves, err := learn.Legible(filepath.Join(dir, "test-set-0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 1 || leaves[0].Basis[0] != models.BasisBeforeWords || leaves[0].Recorded != "x" {
		t.Errorf("an HTTP mock's whole noise: %+v", leaves)
	}
}

// An HTTP mock's leaves, as learn.Legible reads them through the reader HTTP
// registers: the recorded request's leaves by their exact place, none for a
// mock with no request.
func TestTheLeafReaderReadsTheRecordedRequest(t *testing.T) {
	if got := recordedLeaves(nil); got != nil {
		t.Errorf("no mock: %v", got)
	}
	if got := recordedLeaves(&models.Mock{Kind: models.HTTP}); got != nil {
		t.Errorf("no request: %v", got)
	}
	got := recordedLeaves(learnRecorded("mock-0", 0, "POST", "/v1/orders?q=1", `{"note":"ORD-1"}`, nil, `{}`))
	for _, p := range []string{"body.note", "url.query.q", "url.path.1"} {
		if _, ok := got[p]; !ok {
			t.Errorf("no leaf at %s: %v", p, got)
		}
	}
}

// tiePlaces' own contract, for whatever learnPlan gives it: an echo and noise
// that held one value are refused in either order; twins of one number are
// one variable when F sent the number twice in two spellings; their
// variable takes a name the test case's keys left free; a test that ended
// names none; and two places whose only change is an instant read within
// the call's window are exempt there.
func TestTiePlacesOwnContract(t *testing.T) {
	leaf := func(path string, typ learn.Type, rec, live string) learn.Learned {
		return learn.Learned{Kind: learn.KindNoise, Recorded: learn.Leaf{Path: path, Type: typ, Value: rec}, Live: learn.Leaf{Path: path, Type: typ, Value: live}}
	}
	plan := func(t *testing.T, slots ...slotPlace) pairing {
		p := pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{"mock-0": {}}}
		for _, s := range slots {
			if s.kind == learn.KindNoise {
				p.words[s.l.Recorded.Path] = *wordsOf(t, s.l.Recorded.Value, s.l.Live.Value)
				p.f["mock-0"][s.l.Recorded.Path] = s.l.Live.Value
			}
		}
		return p
	}
	t.Run("an echo, then noise", func(t *testing.T) {
		a := slotPlace{l: leaf("body.a", learn.String, "o-1111", "o-9999"), kind: learn.KindEcho, name: "a"}
		b := slotPlace{l: leaf("body.b", learn.String, "o-1111", "o-9999"), kind: learn.KindNoise}
		p := plan(t, a, b)
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{a, b}, nil, true, map[string]bool{}, nil); !strings.Contains(why, "learned them apart") || p.refused != models.RefusedTie {
			t.Errorf("%q (refused %q), want refused as two values learned apart", why, p.refused)
		}
	})
	t.Run("twins of one number, spelled two ways", func(t *testing.T) {
		a := slotPlace{l: leaf("body.a", learn.Number, "1042", "2077"), kind: learn.KindNoise}
		b := slotPlace{l: leaf("body.b", learn.Number, "1042", "2077.0"), kind: learn.KindNoise}
		p := plan(t, a, b)
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{a, b}, nil, true, map[string]bool{}, nil); why != "" {
			t.Fatalf("refused: %s", why)
		}
		if len(p.words) != 0 || len(p.vars) != 1 || p.vars["a"].Bind != "body.a" || !reflect.DeepEqual(p.vars["a"].Use, []string{"body.b"}) {
			t.Fatalf("words %v variables %+v, want one variable bound at body.a and used at body.b", p.words, p.vars)
		}
	})
	twins := func() (slotPlace, slotPlace) {
		return slotPlace{l: leaf("body.id", learn.String, "o-1111", "o-9999"), kind: learn.KindNoise},
			slotPlace{l: leaf("header.x-id", learn.String, "o-1111", "o-9999"), kind: learn.KindNoise}
	}
	t.Run("three places of one value", func(t *testing.T) {
		a := slotPlace{l: leaf("body.a", learn.String, "o-1111", "o-9999"), kind: learn.KindNoise}
		b := slotPlace{l: leaf("body.b", learn.String, "o-1111", "o-9999"), kind: learn.KindNoise}
		c := slotPlace{l: leaf("body.c", learn.String, "o-1111", "o-9999"), kind: learn.KindNoise}
		p := plan(t, a, b, c)
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{c, a, b}, nil, true, map[string]bool{}, nil); why != "" {
			t.Fatalf("refused: %s", why)
		}
		if len(p.words) != 0 || len(p.vars) != 1 || p.vars["a"].Bind != "body.a" || !reflect.DeepEqual(p.vars["a"].Use, []string{"body.b", "body.c"}) {
			t.Fatalf("words %v variables %+v, want one variable bound at body.a and used at body.b and body.c", p.words, p.vars)
		}
	})
	t.Run("a name the test case's keys left free", func(t *testing.T) {
		a, b := twins()
		p := plan(t, a, b)
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{a, b}, nil, true, map[string]bool{"id": true}, nil); why != "" {
			t.Fatalf("refused: %s", why)
		}
		if _, ok := p.vars["id"]; ok || p.vars["id-2"].Bind != "body.id" {
			t.Fatalf("variables %+v, want id-2 (the test case holds id)", p.vars)
		}
	})
	t.Run("a test that ended", func(t *testing.T) {
		a, b := twins()
		p := plan(t, a, b)
		tr := learn.NewTracker()
		if why := tiePlaces(tr, 1, &models.Mock{Name: "mock-0"}, &p, []slotPlace{a, b}, nil, true, map[string]bool{}, nil); why != "the test ended before body.id was learned" {
			t.Fatalf("%q, want the test ended", why)
		}
	})
	t.Run("instants read within the window", func(t *testing.T) {
		boot := learnAt.Add(4 * time.Minute)
		w := &learn.Window{Rec: learn.Span{From: learnAt.Add(-time.Second), To: learnAt.Add(time.Second)}, F: learn.Span{From: boot.Add(-time.Second), To: boot.Add(time.Second)}}
		a := slotPlace{l: leaf("body.at", learn.String, "at "+learnAt.Format(time.RFC3339), "at "+boot.Format(time.RFC3339)), kind: learn.KindNoise}
		b := slotPlace{l: leaf("body.created", learn.String, learnAt.Format(time.RFC3339), boot.Format(time.RFC3339)), kind: learn.KindNoise}
		p := pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{}}
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{a, b}, nil, true, map[string]bool{}, w); why != "" {
			t.Fatalf("refused: %s", why)
		}
		g := []variables.Binding{{Name: "created", Recorded: learnAt.Format(time.RFC3339), Value: boot.Format(time.RFC3339), Given: true}}
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &p, []slotPlace{a}, g, true, map[string]bool{}, w); why != "" {
			t.Fatalf("a test case's instant, refused: %s", why)
		}
		// Two places glued (INV1042, ORD1042), each beside an instant read
		// within the window: the rule that ties them is read in the window
		// too, where the instants are clock reads and only the glued test
		// ties them (containment), never their instants' words.
		inv := slotPlace{l: leaf("body.inv", learn.String, "at "+learnAt.Format(time.RFC3339)+" INV1042", "at "+boot.Format(time.RFC3339)+" INV2077"), kind: learn.KindNoise}
		ord := slotPlace{l: leaf("body.ord", learn.String, "on "+learnAt.Format(time.RFC3339)+" ORD1042", "on "+boot.Format(time.RFC3339)+" ORD2077"), kind: learn.KindNoise}
		q := pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{}}
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &q, []slotPlace{inv, ord}, nil, true, map[string]bool{}, w); !strings.Contains(why, "changed together") || q.refused != models.RefusedContainment {
			t.Fatalf("%q (refused %q), want refused by containment", why, q.refused)
		}
		gi := []variables.Binding{{Name: "inv", Recorded: "at " + learnAt.Format(time.RFC3339) + " INV1042", Value: "at " + boot.Format(time.RFC3339) + " INV2077", Given: true}}
		q = pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{}}
		if why := tiePlaces(learn.NewTracker(), 0, &models.Mock{Name: "mock-0"}, &q, []slotPlace{ord}, gi, true, map[string]bool{}, w); !strings.Contains(why, "test case's inv") || q.refused != models.RefusedContainment {
			t.Fatalf("a test case's value: %q (refused %q), want refused by containment", why, q.refused)
		}
	})
}

// What learnPlan learns across calls and of an echo, beyond the ties: a
// value an earlier call sent whole at a place no variable can stand (a form
// field) is refused; a variable keeps F's value at its bind for
// learned.local.yaml and the earlier call's answer places that echoed it;
// an echo that cannot learn its words says by which rule; and two places of
// one call that name one variable name each get a name of their own.
func TestTheLearnPlanAcrossCalls(t *testing.T) {
	t.Run("a value sent earlier at a place no variable can stand", func(t *testing.T) {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/invoices", "id=k1x9q2", formCT, `{"ok":true}`),
			learnRecorded("mock-1", time.Second, "POST", "/v1/orders", `{"order":"k1x9q2"}`, nil, `{}`),
		}}
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/invoices", "id=z7m3w8", formCT), db, n, opts); served == nil {
			t.Fatalf("precondition: %+v", miss)
		}
		served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"z7m3w8"}`, nil), db, n, opts)
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, "at a place keploy cannot bind it") {
			t.Fatalf("served %v miss %+v, want refused as at a place keploy cannot bind it", served, miss)
		}
	})
	t.Run("a variable's F at its bind, and the answers that echoed it", func(t *testing.T) {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"id":"k-1a2b"}`, nil, `{"id":"k-1a2b"}`),
			learnRecorded("mock-1", time.Second, "POST", "/v1/receipts", `{"order":"k-1a2b"}`, nil, `{}`),
		}}
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"id":"k-9z8y"}`, nil), db, n, opts); served == nil {
			t.Fatalf("precondition: %+v", miss)
		}
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/receipts", `{"order":"k-9z8y"}`, nil), db, n, opts); served == nil {
			t.Fatalf("the use: %+v", miss)
		}
		var use *models.Mock
		for i := range db.consumed {
			if db.consumed[i].Name == "mock-1" {
				use = &db.consumed[i]
			}
		}
		if use == nil {
			t.Fatal("mock-1 was not consumed")
		}
		binds := use.Spec.LearnedBinds()["mock-0"]
		if len(binds) != 1 {
			t.Fatalf("binds %+v, want one variable bound at mock-0", use.Spec.LearnedBinds())
		}
		for name, v := range binds {
			if v.Bind != "body.id" || !reflect.DeepEqual(v.Answer, []string{"body.id"}) || v.Words == nil {
				t.Fatalf("%s: %+v, want bound at body.id, answered at mock-0's body.id, with words", name, v)
			}
		}
		if want := map[string]map[string]string{"mock-0": {"body.id": "k-9z8y"}}; !reflect.DeepEqual(use.Spec.LearnedF(), want) {
			t.Fatalf("F's values %v, want %v", use.Spec.LearnedF(), want)
		}
	})
	t.Run("an echo whose words repeat", func(t *testing.T) {
		h, n, opts := learnOn(t)
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/a", `{"note":"id=x1 ref=x1"}`, nil, `{"note":"id=x1 ref=x1"}`)}}
		served, miss, _ := h.learnMatch(context.Background(), live("POST", "/a", `{"note":"id=y2 ref=y2"}`, nil), db, n, opts)
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, "is echoed in the answer") || miss.res.Refused != models.RefusedTie {
			t.Fatalf("served %v miss %+v, want refused by the tie rule as an echo", served, miss)
		}
	})
	t.Run("an echo and twins whose binds name one variable", func(t *testing.T) {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/a", `{"id":"k-1a","item":{"id":"q-7b"},"ref":"q-7b"}`, nil, `{"id":"k-1a"}`)}}
		served, miss, _ := h.learnMatch(ctx, live("POST", "/a", `{"id":"k-2c","item":{"id":"q-9d"},"ref":"q-9d"}`, nil), db, n, opts)
		if served == nil {
			t.Fatalf("refused: %+v", miss)
		}
		got := db.consumed[0].Spec.Learned()
		if len(got) != 2 || got["id"].Bind != "body.id" || got["id-2"].Bind != "body.item.id" || !reflect.DeepEqual(got["id-2"].Use, []string{"body.ref"}) {
			t.Fatalf("variables %+v, want the echo id and the twins' id-2", got)
		}
	})
}

// A mock's AWS target (X-Amz-Target) whose words were learned, or that is
// obfuscated, may change: the not-owned index keys it by its other parts, as
// one learned whole. A target with no words is keyed exactly, and words for
// a target the mock does not hold key nothing.
func TestATargetWithWordsIsKeyedByItsOtherParts(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const target, targetF = "DynamoDB_20120810.GetItem", "DynamoDB_20120810.PutItem"
	shape, learned := requestKeyShape("POST", 2), requestKeyLearned("POST", 2)
	exact := func(target string) []string {
		return []string{shape, requestKeyExact("POST", []string{"v1", "items"}, target)}
	}
	m := recorded("m", "/v1/items", map[string]string{amzTargetHeader: target}, "")
	if keys := recordedRequestKeys(m); !reflect.DeepEqual(keys, exact(target)) {
		t.Fatalf("a target with no words: %v", keys)
	}
	withWords(t, m, "header."+strings.ToLower(amzTargetHeader), target, targetF)
	if keys := recordedRequestKeys(m); !reflect.DeepEqual(keys, []string{shape, learned}) {
		t.Fatalf("a target with words: %v", keys)
	}
	o := recorded("o", "/v1/items", map[string]string{amzTargetHeader: target}, "")
	o.Noise = []string{"^" + regexp.QuoteMeta(target) + "$"}
	if keys := recordedRequestKeys(o); !reflect.DeepEqual(keys, []string{shape, learned}) {
		t.Fatalf("an obfuscated target: %v", keys)
	}
	none := recorded("n", "/v1/items", nil, "")
	withWords(t, none, "header."+strings.ToLower(amzTargetHeader), target, targetF)
	if keys := recordedRequestKeys(none); !reflect.DeepEqual(keys, exact("")) {
		t.Fatalf("words for a target the mock does not hold: %v", keys)
	}
}

// A multipart field is excused by keploy.yml's body noise that names it
// whole, never by one whose regex would match an empty value, and a file
// part is compared whatever words its field learned.
func TestAMultipartPartsNoiseAndWords(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mp := func(note, file string) (string, string) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		_ = w.SetBoundary("b0undary")
		_ = w.WriteField("note", note)
		fw, _ := w.CreateFormFile("upload", "a.txt")
		_, _ = fw.Write([]byte(file))
		_ = w.Close()
		return "multipart/form-data; boundary=b0undary", b.String()
	}
	h := newHTTP()
	ct, rec := mp("ORD-1 ref=a1", "doc ref=a1")
	_, liveNote := mp("ORD-2 ref=b2", "doc ref=a1")
	_, liveFile := mp("ORD-1 ref=a1", "doc ref=b2")
	if d := (strictKnown{n: h.newStrictNoise(nil, map[string][]string{"note": {}}, nil)}).multipartDiffs(ct, rec, ct, []byte(liveNote)); len(d) != 0 {
		t.Errorf("a field keploy.yml's noise names whole: %v", d)
	}
	if d := (strictKnown{n: h.newStrictNoise(nil, map[string][]string{"note": {".*"}}, nil)}).multipartDiffs(ct, rec, ct, []byte(liveNote)); len(d) == 0 {
		t.Error("a field a value regex names: excused")
	}
	k := strictKnown{n: h.newStrictNoise(nil, nil, nil), words: map[string]models.WordsEntry{"body.upload": *wordsOf(t, "doc ref=a1", "doc ref=c3")}}
	if d := k.multipartDiffs(ct, rec, ct, []byte(liveFile)); len(d) == 0 {
		t.Error("a file part with words: excused")
	}
	// A field is excused, as a form field is, by the noise the learn step
	// learned at it, and where the recording's value is one the obfuscator
	// redacted; with neither, it is compared.
	m := recorded("m", "/v1/orders", map[string]string{"Content-Type": ct}, rec)
	if d := knownOf(h.newStrictNoise(nil, nil, nil), m).multipartDiffs(ct, rec, ct, []byte(liveNote)); len(d) != 1 {
		t.Errorf("a field with no noise: %v, want it to differ", d)
	}
	m.Spec.ReqBodyNoise = map[string][]string{"body.note": nil}
	if d := knownOf(h.newStrictNoise(nil, nil, nil), m).multipartDiffs(ct, rec, ct, []byte(liveNote)); len(d) != 0 {
		t.Errorf("a field the learn step learned as noise: %v", d)
	}
	m = recorded("m", "/v1/orders", map[string]string{"Content-Type": ct}, rec)
	m.Noise = []string{"^ORD-1 ref=a1$"}
	if d := knownOf(h.newStrictNoise(nil, nil, nil), m).multipartDiffs(ct, rec, ct, []byte(liveNote)); len(d) != 0 {
		t.Errorf("a field whose recorded value was redacted: %v", d)
	}
}

// A call is on the route of a mock with variables at a path segment whose
// words the learn step learned (noise.words), where its slots may change: its
// field diffs leave the variable's place out there, as at a segment a
// variable is declared at.
func TestARouteHoldsTheSegmentsItsWordsMayChange(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := at(varMock("mock-1", "POST", "http://store/orders/ORD-1", `{"id":"r1","item":"pen"}`, 201, `{}`,
		models.MockVariables{"id": bindV("r1", "body.id")}), 1)
	withWords(t, m, "url.path.1", "ORD-1", "ORD-7")
	r := makeReq("POST", "/orders/ORD-2")
	r.Header = http.Header{"Content-Type": []string{"application/json"}}
	if _, body := asRecordedForDiff(r, []byte(`{"id":"l1","item":"pen"}`), m); !strings.Contains(string(body), `"r1"`) {
		t.Fatalf("the call's variable was not put back on its route: %s", body)
	}
	other := makeReq("POST", "/orderz/ORD-2")
	other.Header = r.Header
	if _, body := asRecordedForDiff(other, []byte(`{"id":"l1","item":"pen"}`), m); strings.Contains(string(body), `"r1"`) {
		t.Fatalf("a call to another route had its variable put back: %s", body)
	}
}

// A pairing keeps what the recording carried: the words learned of its
// leaves before (here of a header this call does not send) and F's values
// beside them (a reusable mock paired before in the round), beside what it
// learns now.
func TestAPairingKeepsWhatTheMockCarried(t *testing.T) {
	h, n, opts := learnOn(t)
	m := learnRecorded("mock-0", 0, "POST", "/v1/charges", `{"note":"ORD-1 paid 100 ref=a1b2c3"}`, nil, `{"ok":true}`)
	m.Spec.LearnedWords = map[string]models.WordsEntry{"header.x-old": *wordsOf(t, "t-1", "t-2")}
	m.Spec.SetLearnedF(map[string]map[string]string{"mock-0": {"header.x-old": "t-2"}})
	db := &learnDb{perTest: []*models.Mock{m}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/charges", `{"note":"ORD-1 paid 100 ref=d4e5f6"}`, nil), db, n, opts)
	if err != nil || served == nil || miss != nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	got := db.consumed[0].Spec
	if _, ok := got.LearnedWords["header.x-old"]; !ok || len(got.LearnedWords) != 2 {
		t.Fatalf("words at %v, want the recording's and body.note", got.LearnedWords)
	}
	if want := map[string]map[string]string{"mock-0": {"header.x-old": "t-2", "body.note": "ORD-1 paid 100 ref=d4e5f6"}}; !reflect.DeepEqual(got.LearnedF(), want) {
		t.Fatalf("F's values %v, want %v", got.LearnedF(), want)
	}
}

// The learn step's own pass (learnedMatch) serves only a recording that
// learned something: one with no noise and no words is the matcher's, even
// for its own recorded call.
func TestLearnedMatchLeavesAnUnlearnedRecordingToTheMatcher(t *testing.T) {
	h, n, _ := learnOn(t)
	rec := `{"note":"ORD-1 paid 100 ref=a1b2c3"}`
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/charges", rec, nil, `{"ok":true}`)}}
	if served, err := h.learnedMatch(context.Background(), live("POST", "/v1/charges", rec, nil), db, n, models.OutgoingOptions{}); err != nil || served != nil {
		t.Fatalf("served %v err %v, want none", served, err)
	}
}
