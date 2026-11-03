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

// A text one word of which changed between the recording and record's full
// round (a fresh id, a stamp the lead moved) is learned as noise of that word
// alone: the rest of the text is still compared at every replay (founder,
// 2026-10-22). Learned whole, "lead <stamp> created" would pass "deal <stamp>
// deleted" too. Each place the learn step reads a text at is checked so: a
// JSON string, a query value, a header, a path segment, a form field and a
// multipart field.
func TestLearnMatchATextsChangedWordIsTheOnlyWordLeftUnchecked(t *testing.T) {
	stampAt := learnAt.Add(-time.Hour + 17*time.Second)
	stamp := func(d time.Duration) string { return stampAt.Add(d).Format(time.RFC3339) }
	body := func(note string) string { return `{"note":"` + note + `","item":"book"}` }
	query := func(note string) string { return "/v1/leads?note=" + url.QueryEscape(note) }
	for _, c := range []struct {
		name, place     string
		field           string // the place as a miss names it
		clock           bool
		call            func(note string) *req
		rec, f, ok, bad string
	}{
		{"a fresh id word in a JSON text", "body.note", "body.note", false, func(n string) *req { return live("POST", "/v1/leads", body(n), nil) },
			"lead r-1 created", "lead r-2 created", "lead r-3 created", "deal r-3 deleted"},
		{"a stamp the lead moved in a JSON text", "body.note", "body.note", true, func(n string) *req { return live("POST", "/v1/leads", body(n), nil) },
			"lead " + stamp(0) + " created", "lead " + stamp(learn.FullRoundLead) + " created", "lead " + stamp(2*learn.FullRoundLead) + " created", "deal " + stamp(2*learn.FullRoundLead) + " deleted"},
		{"two words of a query value", "url.query.note", "query.note", false, func(n string) *req { return live("GET", query(n), "", nil) },
			"lead r-1 at s-1", "lead r-2 at s-2", "lead r-9 at s-9", "lead r-9 on s-9"},
		{"a header's token", "header.x-trace", "header.x-trace", false, func(n string) *req { return live("GET", "/v1/leads", "", map[string]string{"X-Trace": n}) },
			"tenant=acme; span=s-1", "tenant=acme; span=s-2", "tenant=acme; span=s-3", "tenant=evil; span=s-3"},
		{"an id word of a path segment", "url.path.2", "path", false, func(n string) *req { return live("GET", "/v1/leads/"+n, "", nil) },
			"r-1,v2", "r-2,v2", "r-3,v2", "r-3,v9"},
		{"a form field's words", "body.note", "body.note", false, func(n string) *req {
			return live("POST", "/v1/leads", "note="+url.QueryEscape(n)+"&item=book", map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		}, "lead r-1 created", "lead r-2 created", "lead r-3 created", "deal r-3 deleted"},
		{"a multipart field's words", "body.note", "body.note", false, func(n string) *req {
			return live("POST", "/v1/leads", multipartNote(n), map[string]string{"Content-Type": "multipart/form-data; boundary=" + noteBoundary})
		}, "lead r-1 created", "lead r-2 created", "lead r-3 created", "deal r-3 deleted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			if c.clock {
				tr = clockedTracker()
			}
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			in := c.call(c.rec)
			rec := learnRecorded("mock-0", 0, in.method, in.url.String(), string(in.body), headerOf(in), `{"ok":true}`)
			db := &learnDb{perTest: []*models.Mock{rec}}
			if served, miss, err := h.learnMatch(ctx, c.call(c.f), db, n, opts); err != nil || served == nil {
				t.Fatalf("record's full round refused: %+v %v", miss, err)
			}
			if got, want := db.consumed[0].Spec.ReqBodyNoise[c.place], []string{learn.NoiseWordsPattern(changedWords(c.rec, c.f))}; !reflect.DeepEqual(got, want) {
				t.Fatalf("learned %v at %s (noise %v); want %v", got, c.place, db.consumed[0].Spec.ReqBodyNoise, want)
			}
			mocks := learnedOnto([]*models.Mock{rec}, db.consumed)
			for _, r := range []struct {
				note string
				want bool
			}{{c.ok, true}, {c.bad, false}, {c.rec + " x", false}} {
				ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: append([]*models.Mock(nil), mocks...)}, c.call(r.note))
				if ok != r.want || !ok && !hasField(differs(diag), c.field) {
					t.Fatalf("replay of %q answered %v (differs %v); want %v, naming %s", r.note, ok, differs(diag), r.want, c.field)
				}
			}
		})
	}
}

// A text whose skeleton moved between the recording and record's full round
// (a word came, a separator changed) pins no word's place: the learn step
// refuses it, naming the place, and never learns the whole text as noise. A
// value that is one word is noise whole, as before.
func TestLearnMatchATextWhoseSkeletonMovedIsRefusedNotNoise(t *testing.T) {
	for _, c := range []struct {
		name, rec, f string
		whole        bool
	}{
		{"a word came", "lead r-1 created", "lead r-2 created now", false},
		{"a separator changed", "lead r-1 created", "lead r-2, created", false},
		{"one word", "r-1", "r-2", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/leads", `{"note":"`+c.rec+`"}`, nil, `{}`)}}
			served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/leads", `{"note":"`+c.f+`"}`, nil), db, n, opts)
			if c.whole {
				if served == nil || !reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, map[string][]string{"body.note": {}}) {
					t.Fatalf("one word: served %v consumed %+v; want body.note noise whole", served, db.consumed)
				}
				return
			}
			if served != nil || miss == nil || !strings.HasPrefix(miss.res.Why, "body.note changed in more than its words") {
				t.Fatalf("served %v miss %+v; want body.note refused, naming why", served, miss)
			}
		})
	}
}

// A words pattern names the words of the one text at its key, in the learn
// step's dialect. It excuses nothing else: not that text whole, named in the
// request matcher's dialect (body.items[].note), no leaf of a container at
// its key, and no value of a repeated query parameter or form field (its
// values are learned as one text, never read so at a replay). Each is
// compared as it came.
func TestStrictCompareAWordsPatternExcusesNoOtherPlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const form = "application/x-www-form-urlencoded"
	for _, c := range []struct {
		name, key, ct string
		rec, bad      [2]string // target, body
		field         string
	}{
		{"the matcher's dialect", "body.items[].note", "", [2]string{"/v1/leads", `{"items":[{"note":"lead r-1 created"}]}`}, [2]string{"/v1/leads", `{"items":[{"note":"deal r-9 deleted"}]}`}, "body.items[].note"},
		{"a container's leaf", "body.obj", "", [2]string{"/v1/leads", `{"obj":{"x":"lead r-1"}}`}, [2]string{"/v1/leads", `{"obj":{"x":"deal r-9"}}`}, "body.obj.x"},
		{"a repeated query parameter", "url.query.note", "", [2]string{"/v1/leads?note=lead+r-1&note=x", ""}, [2]string{"/v1/leads?note=lead+r-9&note=y", ""}, "query.note"},
		{"a repeated form field", "body.note", form, [2]string{"/v1/leads", "note=lead+r-1&note=x"}, [2]string{"/v1/leads", "note=lead+r-9&note=y"}, "body.note"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var header map[string]string
			if c.ct != "" {
				header = map[string]string{"Content-Type": c.ct}
			}
			method := "POST"
			if c.rec[1] == "" {
				method = "GET"
			}
			m := learnRecorded("mock-0", 0, method, c.rec[0], c.rec[1], header, `{"ok":true}`)
			m.Spec.ReqBodyNoise = map[string][]string{c.key: {learn.NoiseWordsPattern([]int{0, 1})}}
			ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{m}}, live(method, c.bad[0], c.bad[1], header))
			if ok || !hasField(differs(diag), c.field) {
				t.Fatalf("%v answered %v (differs %v); want refused, naming %s", c.bad, ok, differs(diag), c.field)
			}
		})
	}
}

// The variable pass puts a recorded value back only where a place is noise
// whole: a text learned by its words is compared as it came, beside a
// variable as anywhere, its other words checked.
func TestVariablesATextLearnedByItsWordsIsComparedBesideAVariable(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		note string
		want bool
	}{{"lead r-9 created", true}, {"deal r-9 deleted", false}} {
		m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","note":"lead r-1 created","n":1}`, 201, `{}`,
			models.MockVariables{"orderId": bindV("r1", "body.id")}), 1)
		m.Spec.ReqBodyNoise = map[string][]string{"body.note": {learn.NoiseWordsPattern([]int{1})}}
		ok, _, diag := matchVar(t, newHTTP(), &varDb{perTest: []*models.Mock{m}}, liveReq("POST", "/orders", `{"id":"l1","note":"`+c.note+`","n":1}`))
		if ok != c.want {
			t.Fatalf("note %q answered %v (differs %v); want %v", c.note, ok, differs(diag), c.want)
		}
	}
}

// headerOf is the headers a live call sets besides its Host, as
// learnRecorded takes them.
func headerOf(in *req) map[string]string {
	out := map[string]string{}
	for k, v := range in.header {
		if k != "Host" {
			out[k] = strings.Join(v, ",")
		}
	}
	return out
}

// noteBoundary is the boundary of multipartNote's form.
const noteBoundary = "keploy-note-boundary"

// multipartNote is a multipart form of two fields, note and item.
func multipartNote(note string) string {
	return "--" + noteBoundary + "\r\nContent-Disposition: form-data; name=\"note\"\r\n\r\n" + note +
		"\r\n--" + noteBoundary + "\r\nContent-Disposition: form-data; name=\"item\"\r\n\r\nbook\r\n--" + noteBoundary + "--\r\n"
}

// changedWords are the words of rec that f changed (learn.NoiseWords).
func changedWords(rec, f string) []int {
	words, _, _ := learn.NoiseWords(rec, f)
	return words
}
