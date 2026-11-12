package http

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

const valuesForm = "application/x-www-form-urlencoded"

// valuesKey is a key sent more than once, in a query or a form.
type valuesKey struct {
	name, ct  string
	place     string // the learn step's place
	field     string // the place a miss names
	target    func(vs ...string) string
	body      func(vs ...string) string
	reqMethod string
}

func valuesKeys() []valuesKey {
	enc := func(vs []string) string {
		q := url.Values{"note": vs, "item": {"book"}}
		return q.Encode()
	}
	return []valuesKey{
		{name: "a query parameter", place: "url.query.note", field: "query.note", reqMethod: "GET",
			target: func(vs ...string) string { return "/v1/leads?" + enc(vs) }, body: func(...string) string { return "" }},
		{name: "a form field", ct: valuesForm, place: "body.note", field: "body.note", reqMethod: "POST",
			target: func(...string) string { return "/v1/leads" }, body: func(vs ...string) string { return enc(vs) }},
	}
}

func (k valuesKey) header() map[string]string {
	if k.ct == "" {
		return nil
	}
	return map[string]string{"Content-Type": k.ct}
}

func (k valuesKey) recorded(name string, at time.Duration, vs ...string) *models.Mock {
	return learnRecorded(name, at, k.reqMethod, k.target(vs...), k.body(vs...), k.header(), `{"ok":true}`)
}

func (k valuesKey) call(vs ...string) *req {
	return live(k.reqMethod, k.target(vs...), k.body(vs...), k.header())
}

// A key sent more than once (a query parameter, a form field) one word of
// one value of which drifted between the recording and record's full round
// is learned by that word alone, its values read value by value in the order
// sent: the replay lets that word change, and compares every other word of
// each value, how many values are sent, and their order where one changed.
// The recorded values in another order are the same values. Under either
// switch at the learning; the replay with the switch off matches by shape,
// so there the row asks only that the calls the words admit are answered.
func TestLearnMatchARepeatedKeysDriftedWordIsLearnedValueByValue(t *testing.T) {
	const tokRec, tokF, tokThird = "q7Zk2Lm9Xw", "R4tY8uIo3P", "Zz9xAb3Cd4"
	lead := func(w string) string { return "Lead " + w + " for ci" }
	for _, k := range valuesKeys() {
		for _, proven := range []string{"", "1"} {
			t.Run(k.name+"/proven="+proven, func(t *testing.T) {
				recs := []*models.Mock{k.recorded("mock-0", 1130*time.Millisecond, lead(tokRec), "Open deal")}
				db := driftLearn(t, proven, false, recs, []*req{k.call(lead(tokF), "Open deal")})
				want := map[string][]string{k.place: {learn.NoiseWordsPattern([]int{1})}}
				if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, want) {
					t.Fatalf("learned %v; want %v: the changed word of the first value alone", got, want)
				}
				mocks := learnedOnto(recs, db.consumed)
				replays := []string{"1"}
				if proven != "1" {
					replays = append(replays, proven)
				}
				for _, sw := range replays {
					t.Setenv(provenswitch.Env, sw)
					for _, r := range []struct {
						name string
						vs   []string
						want bool
					}{
						{"as at record's full round", []string{lead(tokF), "Open deal"}, true},
						{"as recorded", []string{lead(tokRec), "Open deal"}, true},
						{"a third token", []string{lead(tokThird), "Open deal"}, true},
						{"the recorded values in another order", []string{"Open deal", lead(tokRec)}, true},
						{"another word of the changed value", []string{"Evil " + tokF + " for ci", "Open deal"}, false},
						{"a word of the other value", []string{lead(tokF), "Open evil"}, false},
						{"the token's word in the other value", []string{lead(tokF), "Open " + tokF}, false},
						{"the values in another order, one changed", []string{"Open deal", lead(tokF)}, false},
						{"a value fewer", []string{lead(tokF)}, false},
						{"a value more", []string{lead(tokF), "Open deal", "Open deal"}, false},
						{"one value of both texts", []string{lead(tokF) + "&Open deal"}, false},
					} {
						if !r.want && sw != "1" {
							continue
						}
						vdb := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
						ok, _, diag := matchVar(t, newHTTP(), vdb, k.call(r.vs...))
						if ok != r.want || !ok && !hasField(differs(diag), k.field) {
							t.Fatalf("switch %q, %s %q: answered %v (differs %v); want %v, naming %s", sw, r.name, r.vs, ok, differs(diag), r.want, k.field)
						}
					}
				}
			})
		}
	}
}

// Record's full round reads a key sent more than once value by value, in the
// order sent. A run that sends another number of values is refused, naming
// how many values the key sends; one whose values came in another order as
// well as one changing is refused, naming the key, since no value can be
// paired with the one it changed from: neither leaves a value unchecked. The
// same values in another order are the same, and learn nothing.
func TestLearnMatchRefusesARepeatedKeysValuesThatCameOrWentOrMoved(t *testing.T) {
	for _, k := range valuesKeys() {
		for _, c := range []struct {
			name     string
			rec, f   []string
			why      string // "" for served, nothing learned
			wantMiss bool
		}{
			{"a value more", []string{"a1", "b"}, []string{"a1", "b", "c"}, "the number of values of " + k.place, true},
			{"a value fewer", []string{"a1", "b", "c"}, []string{"a1", "b"}, "the number of values of " + k.place, true},
			{"one value, then two", []string{"a1"}, []string{"a1", "b"}, "the number of values of " + k.place, true},
			{"another order, a value changed", []string{"a1", "b"}, []string{"b", "a2"}, k.place + " sent its values in another order", true},
			{"another order, a value of three changed", []string{"a1", "b", "c"}, []string{"a1", "c", "x"}, k.place + " sent its values in another order", true},
			{"another order alone", []string{"a1", "b"}, []string{"b", "a1"}, "", false},
		} {
			t.Run(k.name+"/"+c.name, func(t *testing.T) {
				h, n, opts := learnOn(t)
				tr := learn.NewTracker()
				tr.Begin()
				ctx := learn.WithTracker(context.Background(), tr)
				db := &learnDb{perTest: []*models.Mock{k.recorded("mock-0", 0, c.rec...)}, observe: tr.ServedMock}
				served, miss, err := h.learnMatch(ctx, k.call(c.f...), db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if !c.wantMiss {
					if served == nil || len(db.consumed) != 1 || len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
						t.Fatalf("served %v, consumed %+v, miss %+v; want served, nothing learned", served != nil, db.consumed, miss)
					}
					return
				}
				if served != nil || miss == nil || !strings.Contains(miss.res.Why, c.why) {
					why := ""
					if miss != nil {
						why = miss.res.Why
					}
					t.Fatalf("served %v (consumed %+v), why %q; want refused, saying %q", served != nil, db.consumed, why, c.why)
				}
			})
		}
	}
}

// keploy.yml's entry with patterns is the user's word on a field: at record's
// full round a words pattern learned there leaves no word of it unchecked, so
// a call whose only change is a learned word, to a value outside the
// patterns, is refused as the patterns say, and one inside them is served.
func TestLearnMatchAWordsPatternUnchecksNoWordOfAFieldKeployYmlGuards(t *testing.T) {
	for _, c := range []struct {
		note   string
		served bool
	}{{"lead r-9 created", true}, {"lead x-9 created", false}} {
		t.Run(c.note, func(t *testing.T) {
			h, _, opts := learnOn(t)
			n := h.newLearnNoise(opts, map[string][]string{"note": {`^lead r-\d+ created$`}}, nil)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			m := learnRecorded("mock-0", 0, "POST", "/v1/leads", `{"note":"lead r-1 created"}`, nil, `{"ok":true}`)
			m.Spec.ReqBodyNoise = map[string][]string{"body.note": {learn.NoiseWordsPattern([]int{1})}}
			db := &learnDb{perTest: []*models.Mock{m}, observe: tr.ServedMock}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/leads", `{"note":"`+c.note+`"}`, nil), db, n, opts)
			if err != nil || (served != nil) != c.served || !c.served && (miss == nil || !strings.Contains(miss.res.Why, "body.note")) {
				why := ""
				if miss != nil {
					why = miss.res.Why
				}
				t.Fatalf("served %v (why %q, err %v); want served %v, a refusal naming body.note", served != nil, why, err, c.served)
			}
		})
	}
}

// A text of one value holding '&' is read by its words as any text: two of
// its words that changed are learned so, never refused as values that moved.
func TestLearnMatchAOneValueTextHoldingAnAmpersandIsReadByItsWords(t *testing.T) {
	recs := []*models.Mock{learnRecorded("mock-0", 1130*time.Millisecond, "PUT", driftPath, driftNote("a1&b"), nil, `{"data":{"version":1}}`)}
	db := driftLearn(t, "1", false, recs, []*req{live("PUT", driftPath, driftNote("b&a2"), nil)})
	want := map[string][]string{"body.data.label": {learn.NoiseWordsPattern([]int{0, 1})}}
	if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, want) {
		t.Fatalf("learned %v; want %v", got, want)
	}
}

// Noise an older keploy learned whole at a key sent more than once leaves its
// values unchecked no longer, though each value is one word: its values are
// one text of more than one word, so a change in them is refused, saying to
// record the test again. The recorded values in another order are served.
func TestReplayReadsAnOlderKeploysWholeNoiseAtARepeatedKeysValues(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, k := range valuesKeys() {
		for _, c := range []struct {
			vs   []string
			want bool
		}{{[]string{"b", "a"}, true}, {[]string{"a", "c"}, false}} {
			m := k.recorded("mock-0", 0, "a", "b")
			m.Spec.ReqBodyNoise = map[string][]string{k.place: {}}
			ok, _, diag := matchVar(t, newHTTP(), &varDb{perTest: []*models.Mock{m}}, k.call(c.vs...))
			if ok != c.want || !ok && !hasField(differs(diag), k.field) {
				t.Fatalf("%s %q: answered %v (differs %v); want %v", k.name, c.vs, ok, differs(diag), c.want)
			}
			if want := []models.FieldReason{{Field: k.field, Why: staleWholeWhy(k.field)}}; !ok && !reflect.DeepEqual(reasonsOf(diag.differs), want) {
				t.Fatalf("%s %q: reasons %+v; want %+v: record the test again", k.name, c.vs, reasonsOf(diag.differs), want)
			}
		}
	}
}
