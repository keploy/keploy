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
)

// A text one word of which changed between the recording and record's full
// round (a fresh id, a stamp the lead moved) is learned as noise of that word
// alone: the rest of the text is still compared at every replay (founder,
// 2026-10-22). Learned whole, "lead <stamp> created" would pass "deal <stamp>
// deleted" too. Each place the learn step reads a text at is checked so: a
// JSON string, a query value, a header and a path segment.
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

// headerOf is the headers a live call sets besides the ones live always
// sets, as learnRecorded takes them.
func headerOf(in *req) map[string]string {
	out := map[string]string{}
	for k, v := range in.header {
		if k != "Host" && k != "Content-Type" {
			out[k] = strings.Join(v, ",")
		}
	}
	return out
}

// changedWords are the words of rec that f changed (learn.NoiseWords).
func changedWords(rec, f string) []int {
	words, _, _ := learn.NoiseWords(rec, f)
	return words
}
