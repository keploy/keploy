package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Noise an older keploy learned whole at a text is honoured only where the
// recorded text is one word (founder, 2026-11-03). At a text of more than
// one word it no longer leaves the text unchecked: the replay compares the
// text, and a change in any of its words is refused, with a reason that
// names the field and says to record the test again. Each place a replay
// reads a text at is checked so.
func TestReplayHonoursAnOlderKeploysWholeNoiseOnlyAtATextOfOneWord(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body := func(note string) string { return `{"note":"` + note + `","item":"book"}` }
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	multipart := map[string]string{"Content-Type": "multipart/form-data; boundary=" + noteBoundary}
	for _, c := range []struct {
		name, place, field string
		call               func(note string) *req
		one, oneLive       string // a one-word text, and a replay of it its noise passes
		rec                string // a text of more than one word
		bad                []string
	}{
		{"a JSON text", "body.note", "body.note", func(n string) *req { return live("POST", "/v1/leads", body(n), nil) },
			"r-1", "r-2", "lead r-1 created", []string{"deal r-3 deleted", "lead r-2 created"}},
		{"a query value", "url.query.note", "query.note", func(n string) *req { return live("GET", "/v1/leads?note="+url.QueryEscape(n), "", nil) },
			"r-1", "r-2", "lead r-1 at s-1", []string{"deal r-3 on s-3", "lead r-2 at s-1"}},
		{"a header", "header.x-trace", "header.x-trace", func(n string) *req { return live("GET", "/v1/leads", "", map[string]string{"X-Trace": n}) },
			"s-1", "s-2", "tenant=acme; span=s-1", []string{"tenant=evil; span=s-3", "tenant=acme; span=s-2"}},
		{"a path segment", "url.path.2", "path", func(n string) *req { return live("GET", "/v1/leads/"+n, "", nil) },
			"r-1", "r-2", "r-1,v2", []string{"r-3,v9", "r-2,v2"}},
		{"a form field", "body.note", "body.note", func(n string) *req {
			return live("POST", "/v1/leads", "note="+url.QueryEscape(n)+"&item=book", form)
		}, "r-1", "r-2", "lead r-1 created", []string{"deal r-3 deleted", "lead r-2 created"}},
		{"a multipart field", "body.note", "body.note", func(n string) *req { return live("POST", "/v1/leads", multipartNote(n), multipart) },
			"r-1", "r-2", "lead r-1 created", []string{"deal r-3 deleted", "lead r-2 created"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock := func(text string) *models.Mock {
				in := c.call(text)
				m := learnRecorded("mock-0", 0, in.method, in.url.String(), string(in.body), headerOf(in), `{"ok":true}`)
				m.Spec.ReqBodyNoise = map[string][]string{c.place: {}}
				return m
			}
			if ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{mock(c.one)}}, c.call(c.oneLive)); !ok {
				t.Fatalf("one word: %q against %q refused (differs %v); want its old noise whole honoured", c.oneLive, c.one, differs(diag))
			}
			for _, bad := range c.bad {
				ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{mock(c.rec)}}, c.call(bad))
				if ok || diag == nil || !hasField(differs(diag), c.field) {
					t.Fatalf("%q against %q answered %v (differs %v); want refused, naming %s", bad, c.rec, ok, differs(diag), c.field)
				}
				want := models.FieldReason{Field: c.field, Why: staleWholeWhy(c.field)}
				if got := reasonsOf(diag.differs); !reflect.DeepEqual(got, []models.FieldReason{want}) {
					t.Fatalf("%q: reasons %+v; want %+v alone", bad, got, want)
				}
				if !strings.HasPrefix(want.Why, c.field+" was learned as noise whole by an older keploy") || !strings.Contains(want.Why, "Record the test again") {
					t.Fatalf("why %q: want it to name %s and say to record the test again", want.Why, c.field)
				}
			}
		})
	}
}

// A path one segment of which an older keploy learned as noise whole, a
// text of more than one word, is of the call's route: a change there is
// refused with the reason that says to record the test again, and the
// recording is the closest one named. A path another segment of which
// changed too is of another route: refused, with no such reason.
func TestReplayReadsAPathWithAnOlderKeploysWholeNoiseSegmentAsTheCallsRoute(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := learnRecorded("mock-0", 0, "GET", "/v1/leads/r-1,v2/notes", "", nil, `{"ok":true}`)
	m.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}}
	for _, c := range []struct {
		path  string
		route bool
	}{
		{"/v1/leads/r-3,v9/notes", true},
		{"/v1/leads/r-3,v9/tags", false},
		{"/v2/leads/r-3,v9/notes", false},
	} {
		ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{m}}, live("GET", c.path, "", nil))
		if ok || diag == nil || !hasField(differs(diag), "path") {
			t.Fatalf("%s answered %v (differs %v); want refused, naming path", c.path, ok, differs(diag))
		}
		var want []models.FieldReason
		phase := models.MatchPhaseSchema
		if c.route {
			want, phase = []models.FieldReason{{Field: "path", Why: staleWholeWhy("path")}}, models.MatchPhaseStrict
		}
		if got := reasonsOf(diag.differs); !reflect.DeepEqual(got, want) || diag.phase != phase {
			t.Fatalf("%s: reasons %+v in phase %s; want %+v in phase %s", c.path, got, diag.phase, want, phase)
		}
	}
}

// What the rule leaves as it was: noise whole an older keploy learned at a
// container of a text (the container's every leaf unchecked), and at a value
// that is no text. Noise whole named in the request matcher's dialect (the
// request-body learner's) at a text of more than one word is an older
// keploy's too, and refused so.
func TestReplayReadsAnOlderKeploysWholeNoiseAtAContainerOrInTheMatchersDialect(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name, key, rec, live string
		want                 bool
		field                string
	}{
		{"a container's text", "body.obj", `{"obj":{"x":"lead r-1 created"}}`, `{"obj":{"x":"deal r-9 deleted"}}`, true, ""},
		{"a number", "body.n", `{"n":1}`, `{"n":2}`, true, ""},
		{"the matcher's dialect, one word", "body.items[].note", `{"items":[{"note":"r-1"}]}`, `{"items":[{"note":"r-9"}]}`, true, ""},
		{"the matcher's dialect, more than one word", "body.items[].note", `{"items":[{"note":"lead r-1 created"}]}`, `{"items":[{"note":"lead r-9 created"}]}`, false, "body.items[].note"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := learnRecorded("mock-0", 0, "POST", "/v1/leads", c.rec, nil, `{"ok":true}`)
			m.Spec.ReqBodyNoise = map[string][]string{c.key: {}}
			ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{m}}, live("POST", "/v1/leads", c.live, nil))
			if ok != c.want {
				t.Fatalf("%s against %s answered %v (differs %v); want %v", c.live, c.rec, ok, differs(diag), c.want)
			}
			if !c.want {
				want := []models.FieldReason{{Field: c.field, Why: staleWholeWhy(c.field)}}
				if got := reasonsOf(diag.differs); !reflect.DeepEqual(got, want) {
					t.Fatalf("reasons %+v; want %+v", got, want)
				}
			}
		})
	}
}

// The variable pass puts a recorded value back where the mock learned noise
// whole, but not at a text of more than one word an older keploy learned
// whole: that text is compared as it came, beside a variable as anywhere.
func TestVariablesDoNotPutBackAnOlderKeploysWholeNoiseAtAText(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		rec, note string
		want      bool
	}{{"r-1", "r-9", true}, {"lead r-1 created", "deal r-9 deleted", false}, {"lead r-1 created", "lead r-9 created", false}} {
		m := at(varMock("mock-1", "POST", "http://store/orders", `{"id":"r1","note":"`+c.rec+`","n":1}`, 201, `{}`,
			models.MockVariables{"orderId": bindV("r1", "body.id")}), 1)
		m.Spec.ReqBodyNoise = map[string][]string{"body.note": {}}
		ok, _, diag := matchVar(t, newHTTP(), &varDb{perTest: []*models.Mock{m}}, liveReq("POST", "/orders", `{"id":"l1","note":"`+c.note+`","n":1}`))
		if ok != c.want {
			t.Fatalf("note %q against %q answered %v (differs %v); want %v", c.note, c.rec, ok, differs(diag), c.want)
		}
	}
}

// Record's full round reads a text of more than one word an older keploy
// learned as noise whole as any text: the words that changed are learned
// (learn.MergeNoise puts that words pattern in the old noise's place), and
// the rest of the text is compared at every replay after. A run that moved
// the text's skeleton is refused, as at a first learning. A one-word text's
// old noise stays whole.
func TestLearnMatchRelearnsAnOlderKeploysWholeNoiseAtATextByItsWords(t *testing.T) {
	w := func(words ...int) []string { return []string{learn.NoiseWordsPattern(words)} }
	body := func(note string) string { return `{"note":"` + note + `","item":"book"}` }
	for _, c := range []struct {
		name, rec, f string
		want         []string // at body.note after the round; nil: refused
		ok, bad      string
	}{
		{"its stamp word changed", "lead r-1 created", "lead r-2 created", w(1), "lead r-3 created", "deal r-3 deleted"},
		{"two words changed", "lead r-1 created", "deal r-2 created", w(0, 1), "deal r-3 created", "deal r-3 deleted"},
		{"its skeleton moved", "lead r-1 created", "lead r-2 created now", nil, "", ""},
		{"one word", "r-1", "r-2", []string{}, "r-3", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			rec := learnRecorded("mock-0", 0, "POST", "/v1/leads", body(c.rec), nil, `{"ok":true}`)
			rec.Spec.ReqBodyNoise = map[string][]string{"body.note": {}}
			db := &learnDb{perTest: []*models.Mock{rec}}
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/leads", body(c.f), nil), db, n, opts)
			if c.want == nil {
				if served != nil || miss == nil || !strings.HasPrefix(miss.res.Why, "body.note changed in more than its words") {
					t.Fatalf("served %v miss %+v err %v; want body.note refused, naming why", served, miss, err)
				}
				return
			}
			if err != nil || served == nil {
				t.Fatalf("record's full round refused: %+v %v", miss, err)
			}
			if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, map[string][]string{"body.note": c.want}) {
				t.Fatalf("learned noise %v; want body.note %v alone", got, c.want)
			}
			kept := *rec
			kept.Spec.ReqBodyNoise = db.consumed[0].Spec.ReqBodyNoise
			for _, r := range []struct {
				note string
				want bool
			}{{c.ok, true}, {c.bad, false}} {
				if r.note == "" {
					continue
				}
				ok, _, diag := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{&kept}}, live("POST", "/v1/leads", body(r.note), nil))
				if ok != r.want {
					t.Fatalf("replay of %q answered %v (differs %v); want %v", r.note, ok, differs(diag), r.want)
				}
			}
		})
	}
}

// The learn step's known noise excuses a text an older keploy learned as
// noise whole only where it is one word, at its key or in the request
// matcher's dialect; noise whole at a container of it still excuses it.
func TestLearnKnownExcusesAnOlderKeploysWholeNoiseOnlyAtOneWord(t *testing.T) {
	shape := func(path, noise, text string) learn.Shape {
		return learn.Shape{Key: "k", Leaves: map[string]learn.Leaf{path: {Path: path, Noise: noise, Type: learn.String, Value: text}}}
	}
	for _, c := range []struct {
		name        string
		learned     map[string][]string
		path, noise string
		rec, live   string
		drift       bool
	}{
		{"one word at its key", map[string][]string{"body.note": {}}, "body.note", "body.note", "r-1", "r-2", false},
		{"more than one word at its key", map[string][]string{"body.note": {}}, "body.note", "body.note", "lead r-1 created", "deal r-2 deleted", true},
		{"one word in the matcher's dialect", map[string][]string{"body.items[].note": {}}, "body.items[0].note", "body.items[].note", "r-1", "r-2", false},
		{"more than one word in the matcher's dialect", map[string][]string{"body.items[].note": {}}, "body.items[0].note", "body.items[].note", "lead r-1 created", "lead r-2 created", true},
		{"a container", map[string][]string{"body.items": {}}, "body.items[0].note", "body.items[].note", "lead r-1 created", "deal r-2 deleted", false},
		{"a container beside its text's own", map[string][]string{"body.items": {}, "body.items[0].note": {}}, "body.items[0].note", "body.items[].note", "lead r-1 created", "deal r-2 deleted", false},
		{"an object beside its text's own", map[string][]string{"body.obj": {}, "body.obj.x": {}}, "body.obj.x", "body.obj.x", "lead r-1 created", "deal r-2 deleted", false},
		{"a container learned by words", map[string][]string{"body.items": {learn.NoiseWordsPattern([]int{0})}, "body.items[0].note": {}}, "body.items[0].note", "body.items[].note", "lead r-1 created", "deal r-2 deleted", true},
	} {
		d := learn.Compare(shape(c.path, c.noise, c.live), shape(c.path, c.noise, c.rec), httpKnown{learned: c.learned})
		if d.Structural != "" || (len(d.Drift) > 0) != c.drift {
			t.Errorf("%s: structural %q drift %v; want drift %v", c.name, d.Structural, d.Drift, c.drift)
		}
	}
	// A value that is no text is read as before, whatever its separators:
	// noise whole learned at an empty object or array excuses it, its type
	// kept.
	for _, l := range []learn.Leaf{
		{Path: "body.meta", Noise: "body.meta", Type: learn.EmptyObj, Value: "{}"},
		{Path: "body.tags", Noise: "body.tags", Type: learn.EmptyArray, Value: "[]"},
	} {
		k := httpKnown{learned: map[string][]string{l.Path: {}}}
		if excused, typed := k.Excused(l); !excused || !typed {
			t.Errorf("%s (%c): excused %v typed %v; want its learned noise to excuse it, its type kept", l.Path, l.Type, excused, typed)
		}
	}
}

// A refused text an older keploy learned as noise whole is said in the
// agent's log as that, with its field and why, never as a read of the
// application's clock; a field that differs for no reason of its own is
// not said so.
func TestAnOlderKeploysRefusedWholeNoiseIsLoggedAsNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := recorded("lead", "/v1/leads", map[string]string{"Content-Type": "application/json"}, `{"note":"lead r-1 created","item":"book"}`)
	rec.Spec.ReqBodyNoise = map[string][]string{"body.note": {}}
	in := sent("/v1/leads", map[string]string{"Content-Type": "application/json"}, `{"note":"deal r-3 deleted","item":"pen"}`)
	core, logs := observer.New(zap.WarnLevel)
	h := &HTTP{Logger: zap.New(core)}
	ok, _, _, err := h.match(context.Background(), in, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}, models.ProvenFlakyHeaderNoise(), nil, nil, true, false, false, true, true)
	if err != nil || ok {
		t.Fatalf("match ok=%v err=%v; want body.note refused", ok, err)
	}
	if n := logs.FilterMessage("a learned read of the application's clock does not hold").Len(); n != 0 {
		t.Fatalf("logged %d refused reads of the clock; want none", n)
	}
	said := logs.FilterMessage("noise an older keploy learned no longer leaves a text unchecked").All()
	if len(said) != 1 {
		t.Fatalf("logged %d stale-noise lines; want one, for body.note", len(said))
	}
	if c := said[0].ContextMap(); c["mock"] != "lead" || c["field"] != "body.note" || c["reason"] != staleWholeWhy("body.note") {
		t.Fatalf("the log says %v; want the mock, body.note and why", c)
	}
}

// A place learned as a read of the application's clock is checked as one
// (clockDiffs), whatever noise an older keploy learned at it: noise whole
// in the request matcher's dialect at a stamp of more than one word (an
// HTTP date) neither excuses it nor refuses it as stale noise.
func TestAClockReadIsNoStaleNoiseOfAnOlderKeploy(t *testing.T) {
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	recAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	body := func(at time.Time) string { return `{"items":[{"since":"` + at.Format(http.TimeFormat) + `"}]}` }
	rec := recorded("usage", "/v1/usage", map[string]string{"Content-Type": "application/json"}, body(recAt))
	rec.Spec.Clocks = map[string]models.ClockOffset{"body.items[0].since": {Res: time.Second}}
	rec.Spec.ReqBodyNoise = map[string][]string{"body.items[].since": {}}
	for _, c := range []struct {
		at   time.Time
		want bool
	}{{base, true}, {base.AddDate(0, 0, -30), false}} {
		in := sent("/v1/usage", map[string]string{"Content-Type": "application/json"}, body(c.at))
		in.arrived = base.Add(2 * time.Second)
		got, diag := strictMatchIn(t, clockConn(true, base), in, nil, nil, nil, rec)
		if (got != nil) != c.want {
			t.Fatalf("since %s: served %v (differs %v); want %v", c.at, got != nil, differs(diag), c.want)
		}
		if !c.want {
			for _, r := range reasonsOf(diag.differs) {
				if r.Why == staleWholeWhy(r.Field) {
					t.Fatalf("since %s: refused as stale noise %+v; want a refused read of the clock", c.at, r)
				}
			}
		}
	}
}

// A path segment the learn step learned as a read of the application's
// clock is checked as one, though an older keploy also learned it as noise
// whole and it is more than one word (a wall clock with a space before its
// time): a read in the call's window is answered, and any other value is
// refused with the clock read's reason alone, never with the one that says
// an older keploy's whole noise no longer excuses it.
func TestAClockSegmentAnOlderKeployLearnedAsNoiseWholeIsCheckedAsAClockRead(t *testing.T) {
	seg := func(v string) string { return "/v1/u/" + url.PathEscape(v) }
	m := recorded("mock-2", seg("2026-10-26 15:50:35"), urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second, Zone: models.ZoneApp, Layout: "iso-second-space", AppZone: "UTC"}}
	m.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}}
	for _, c := range []struct {
		value, why string
	}{
		{"2026-10-26 15:50:36", ""},
		{"2026-09-02 09:00:00", "not a read of the application's clock made in this call's window"},
		{"lead r-1", "no instant keploy reads"},
	} {
		in := sent(seg(c.value), urlNoiseJSON, `{}`)
		in.arrived = urlNoiseBase.Add(2 * time.Second)
		got, diag := strictMatchIn(t, clockConn(true, urlNoiseBase), in, nil, nil, nil, m)
		if c.why == "" {
			if got == nil {
				t.Errorf("%q, a read in the call's window, refused (differs %v); want answered", c.value, differs(diag))
			}
			continue
		}
		if got != nil || diag == nil {
			t.Errorf("%q answered; want refused", c.value)
			continue
		}
		r := reasonsOf(diag.differs)
		if len(r) != 1 || r[0].Field != "path" || !strings.Contains(r[0].Why, "learned as clock") || !strings.Contains(r[0].Why, c.why) {
			t.Errorf("%q: reasons %+v; want the clock read's (%s) alone", c.value, r, c.why)
		}
	}
}
