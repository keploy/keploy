package http

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// stampedSend is a recorded POST /send whose body.content a client test
// named by its own clock (`<p>E2E email body ${Date.now()}</p>`), which the
// learn step learned as a read of the clock: the word at bytes 18-31, read
// since its test began, and body.since, a plain read.
func stampedSend() *models.Mock {
	const content = "<p>E2E email body 1730000000000</p>"
	rec := recorded("mock-6", "/send", map[string]string{"Content-Type": "application/json"},
		`{"content":"`+content+`","since":"2026-09-01T09:00:00Z","to":"a@b.c"}`)
	rec.Spec.Clocks = map[string]models.ClockOffset{
		"body.content": {Res: time.Millisecond, Layout: "epoch-ms", Since: models.SinceTestBegin, Word: models.ClockWordOf(content, 18, 31)},
		"body.since":   {Res: time.Second},
	}
	return rec
}

// clockMissOf matches in, a call no recording is, against mocks under the
// proven switch, and returns its miss report and what the agent logged
// that a learned read of the clock does not hold.
func clockMissOf(t *testing.T, in *req, mocks ...*models.Mock) (*models.MockMismatchReport, []observer.LoggedEntry) {
	t.Helper()
	return clockMissIn(t, in, nil, mocks...)
}

// clockMissIn is clockMissOf under keploy.yml's request-body noise body.
func clockMissIn(t *testing.T, in *req, body map[string][]string, mocks ...*models.Mock) (*models.MockMismatchReport, []observer.LoggedEntry) {
	t.Helper()
	return clockMissWhere(t, true, in, body, nil, mocks...)
}

// clockMissWhere is clockMissIn under keploy.yml's url noise urlNoise too,
// where appClock says whether keploy can tell the application's clock at
// the call (clockConn).
func clockMissWhere(t *testing.T, appClock bool, in *req, body map[string][]string, urlNoise []string, mocks ...*models.Mock) (*models.MockMismatchReport, []observer.LoggedEntry) {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	in.arrived = base.Add(2 * time.Second)
	core, logs := observer.New(zap.WarnLevel)
	h := &HTTP{Logger: zap.New(core)}
	db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
	ok, _, diag, err := h.match(clockConn(appClock, base), in, db, models.ProvenFlakyHeaderNoise(), body, urlNoise, true, false, false, true, true)
	if err != nil || ok || diag == nil {
		t.Fatalf("match ok=%v diag=%v err=%v; want a miss", ok, diag, err)
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: in.method, URL: in.url, Host: "api.example", Header: in.header}, in.body, db, nil, nil, body, diag)
	if r == nil {
		t.Fatal("no miss report")
	}
	return r, logs.FilterMessage("a learned read of the application's clock does not hold").All()
}

// A call of another route than every recording's (an extra POST /search a
// replay makes that no recording holds) is closest to a recording of
// another route, POST /send, which learned reads of the clock. That
// recording is not the call, so its learned reads are no reads the call
// made: the agent logs no read of the clock that does not hold, and the
// miss report names the fields that differ (the path, the recording's
// body.content and body.since the call lacks, its own body.txt) with no
// reason of a clock read, which would blame the clock for a call no
// recording is.
func TestAClockReadOfARecordingOfAnotherRouteIsNoReasonOfAMiss(t *testing.T) {
	in := sent("/search", map[string]string{"Content-Type": "application/json"}, `{"txt":"E2E"}`)
	r, said := clockMissOf(t, in, stampedSend())
	if len(said) != 0 {
		t.Fatalf("the agent logged %d reads of the clock that do not hold, first %v; want none for a recording of another route", len(said), said[0].ContextMap())
	}
	for _, f := range []string{"path", "body.content", "body.since", "body.txt"} {
		if !hasField(r.Differs, f) {
			t.Fatalf("Differs = %v; want %s named", r.Differs, f)
		}
	}
	if len(r.FieldReasons) != 0 {
		t.Fatalf("FieldReasons = %+v; want none: the closest recording is of another route", r.FieldReasons)
	}
}

// A call of the recording's route that carries no value at a place the
// learn step learned as a read of the clock is refused, the place named,
// and its reason says the call carries no value there, of a word read and
// of a plain read alike: never that the text is too short to hold the
// word's sides, or how a value there fails, which names a value the call
// never sent. The agent logs it with that reason.
func TestAClockReadTheCallDoesNotCarrySaysSo(t *testing.T) {
	in := sent("/send", map[string]string{"Content-Type": "application/json"}, `{"to":"a@b.c"}`)
	r, said := clockMissOf(t, in, stampedSend())
	whys := map[string]string{}
	for _, fr := range r.FieldReasons {
		whys[fr.Field] = fr.Why
	}
	logged := map[string]string{}
	for _, e := range said {
		c := e.ContextMap()
		f, _ := c["field"].(string)
		logged[f], _ = c["reason"].(string)
	}
	for _, f := range []string{"body.content", "body.since"} {
		why := whys[f]
		learned := f + " was learned as " + stampedSend().Spec.Clocks[f].String() + ", a read of the application's clock"
		if !hasField(r.Differs, f) || !strings.HasPrefix(why, learned) || !strings.HasSuffix(why, ", and this call carries no value at "+f+": it changed") {
			t.Fatalf("Differs %v, the reason of %s is %q; want it named, and that the call carries no value there", r.Differs, f, why)
		}
		if strings.Contains(why, "too short") {
			t.Fatalf("the reason of %s is %q; want no text the call never sent", f, why)
		}
		if logged[f] != why {
			t.Fatalf("the agent logged %q for %s; want %q", logged[f], f, why)
		}
	}
}

// reportWhy is the reason r gives field, "" when it gives none.
func reportWhy(r *models.MockMismatchReport, field string) string {
	for _, fr := range r.FieldReasons {
		if fr.Field == field {
			return fr.Why
		}
	}
	return ""
}

// A call of the recording's route that carries the place is checked, not
// called absent. Its word, learned as read in its call's window, read from
// a clock thirty days back is refused for how the word fails (the value is
// not a read made in this call's window), and so is an empty text, which
// the call carries too (too short to hold the word's sides).
func TestAClockReadTheCallCarriesIsCheckedNotCalledAbsent(t *testing.T) {
	for _, c := range []struct{ name, content, want string }{
		{"a stale word", "<p>E2E email body 1727800000000</p>", ", and the value this call carries is not "},
		{"an empty text", "", "too short"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := stampedSend()
			word := m.Spec.Clocks["body.content"]
			word.Since = ""
			m.Spec.Clocks["body.content"] = word
			in := sent("/send", map[string]string{"Content-Type": "application/json"}, `{"content":"`+c.content+`","since":"2026-09-01T09:00:00Z","to":"a@b.c"}`)
			r, _ := clockMissOf(t, in, m)
			why := reportWhy(r, "body.content")
			if !hasField(r.Differs, "body.content") || !strings.HasPrefix(why, "body.content was learned as ") || !strings.Contains(why, c.want) || strings.Contains(why, "carries no value") {
				t.Fatalf("Differs %v, the reason of body.content is %q; want it refused for how the carried value fails (%q)", r.Differs, why, c.want)
			}
		})
	}
}

// A place the learn step learned as a read of the clock that keploy.yml
// declares noise is not compared, so a call that does not carry it is not
// refused there either: the user's noise comes before the absent check.
// The call's other learned place it lacks, body.since, is still refused as
// absent.
func TestAClockReadKeployYmlDeclaresNoiseIsNotRefusedAbsent(t *testing.T) {
	in := sent("/send", map[string]string{"Content-Type": "application/json"}, `{"to":"a@b.c"}`)
	r, said := clockMissIn(t, in, map[string][]string{"content": {}}, stampedSend())
	if hasField(r.Differs, "body.content") || reportWhy(r, "body.content") != "" {
		t.Fatalf("Differs %v, the reason of body.content %q; want it not compared: keploy.yml declares it noise", r.Differs, reportWhy(r, "body.content"))
	}
	if why := reportWhy(r, "body.since"); !strings.HasSuffix(why, ", and this call carries no value at body.since: it changed") || len(said) != 1 {
		t.Fatalf("the reason of body.since is %q, %d clock warnings; want it refused as absent, once", why, len(said))
	}
}

// Where keploy cannot tell the application's clock at the call, a learned
// place the call does not carry is still refused as absent, the more direct
// cause: the check that the call carries a value there comes before the
// check that keploy can tell the clock to read it against. The place the
// call does carry, a word of the recorded form, is refused for the clock
// keploy cannot tell. The agent logs each with its reason.
func TestAClockReadTheCallDoesNotCarryIsAbsentWhereKeployCannotTellTheClock(t *testing.T) {
	const cannotTell = "keploy cannot tell the application's clock at this call"
	in := sent("/send", map[string]string{"Content-Type": "application/json"}, `{"content":"<p>E2E email body 1730000000999</p>","to":"a@b.c"}`)
	r, said := clockMissWhere(t, false, in, nil, nil, stampedSend())
	absent, carried := reportWhy(r, "body.since"), reportWhy(r, "body.content")
	if !strings.HasSuffix(absent, ", and this call carries no value at body.since: it changed") || strings.Contains(absent, cannotTell) {
		t.Fatalf("the reason of body.since, which the call does not carry, is %q; want it refused as absent, not for the clock keploy cannot tell", absent)
	}
	if !strings.HasPrefix(carried, "body.content was learned as ") || !strings.Contains(carried, cannotTell) || strings.Contains(carried, "carries no value") {
		t.Fatalf("the reason of body.content, which the call carries, is %q; want it refused for the clock keploy cannot tell (%q)", carried, cannotTell)
	}
	logged := map[string]string{}
	for _, e := range said {
		c := e.ContextMap()
		f, _ := c["field"].(string)
		logged[f], _ = c["reason"].(string)
	}
	if len(said) != 2 || logged["body.since"] != absent || logged["body.content"] != carried {
		t.Fatalf("the agent logged %d clock warnings, %v; want body.since and body.content, each with its reason", len(said), logged)
	}
}

// pathStamped is a recorded POST /v1/u/<instant> whose third path segment
// the learn step learned as a plain read of the clock, the instant a month
// before the call.
func pathStamped() *models.Mock {
	m := recorded("mock-p", "/v1/u/2026-09-01T09:00:00Z", map[string]string{"Content-Type": "application/json"}, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}}
	return m
}

// A call of the recording's own route whose path segment the learn step
// learned as a read of the clock, sent with a stale instant there, is
// refused at that segment, which the report names "path". The walk excused
// the segment, so the recording is of the call's route (phase Strict) and
// the refusal keeps its reason: the agent logs it and the miss explains it,
// never only "path differs" as if the request's structure changed.
func TestAClockReadInAPathSegmentOfItsOwnRouteKeepsItsReason(t *testing.T) {
	r, said := clockMissOf(t, sent("/v1/u/2026-09-02T09:00:00Z", map[string]string{"Content-Type": "application/json"}, `{}`), pathStamped())
	why := reportWhy(r, "path")
	if r.ClosestMock != "mock-p" || r.MatchPhase != models.MatchPhaseStrict || !hasField(r.Differs, "path") {
		t.Fatalf("closest %q, phase %q, Differs %v; want mock-p of the call's route, phase %q, path named", r.ClosestMock, r.MatchPhase, r.Differs, models.MatchPhaseStrict)
	}
	if !strings.HasPrefix(why, "path was learned as ") || !strings.Contains(why, ": it changed") || len(said) != 1 {
		t.Fatalf("the reason of path is %q, %d clock warnings; want the clock check's refusal, logged once", why, len(said))
	}
}

// The same call beside a recording of another route that differs from it in
// as few fields (its path's second segment), compared first: the recording
// of the call's own route stays closest, for its refused clock read in a
// path segment is no part of the route, and the miss keeps its reason.
func TestAClockReadInAPathSegmentKeepsItsRecordingClosest(t *testing.T) {
	other := recorded("mock-w", "/v1/w/2026-09-02T09:00:00Z", map[string]string{"Content-Type": "application/json"}, `{}`)
	r, said := clockMissOf(t, sent("/v1/u/2026-09-02T09:00:00Z", map[string]string{"Content-Type": "application/json"}, `{}`), other, pathStamped())
	if r.ClosestMock != "mock-p" || r.MatchPhase != models.MatchPhaseStrict {
		t.Fatalf("closest %q, phase %q; want mock-p of the call's route, phase %q", r.ClosestMock, r.MatchPhase, models.MatchPhaseStrict)
	}
	if why := reportWhy(r, "path"); !strings.HasPrefix(why, "path was learned as ") || len(said) != 1 {
		t.Fatalf("the reason of path is %q, %d clock warnings; want the clock check's refusal, logged once", why, len(said))
	}
}

// A recording of another route is compared by the walk alone: its learned
// reads of the clock are no reads the call made, so they are neither
// checked nor counted. A call of another route that carries the
// recording's very body differs from it in its path only, though the clock
// check would refuse body.content's word and body.since, an instant a month
// back; and beside a recording of a third route whose walk finds two fields
// differing, it stays the closest.
func TestARecordingOfAnotherRouteIsComparedByTheWalkAlone(t *testing.T) {
	body := `{"content":"<p>E2E email body 1730000000000</p>","since":"2026-09-01T09:00:00Z","to":"a@b.c"}`
	call := func() *req { return sent("/search", map[string]string{"Content-Type": "application/json"}, body) }
	r, said := clockMissOf(t, call(), stampedSend())
	if len(said) != 0 || len(r.FieldReasons) != 0 || len(r.Differs) != 1 || r.Differs[0] != "path" {
		t.Fatalf("Differs %v, FieldReasons %+v, %d clock warnings; want the path alone, no reason, no warning", r.Differs, r.FieldReasons, len(said))
	}
	mail := recorded("mock-m", "/mail", map[string]string{"Content-Type": "application/json"}, strings.Replace(body, "a@b.c", "z@y.x", 1))
	if r, _ := clockMissOf(t, call(), mail, stampedSend()); r.ClosestMock != "mock-6" {
		t.Fatalf("closest %q, Differs %v; want mock-6, which differs in its path alone", r.ClosestMock, r.Differs)
	}
}

// Under exact serving a call is also diagnosed against the open test's
// answered recordings (answeredDiag). When the closest of them is of
// another route, it is not the call either: its learned reads of the clock
// give no reason, though every field that differs is still named.
func TestAnAnsweredRecordingOfAnotherRouteGivesNoClockReason(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stamped := stampedSend()
	db := answeredDb{mockMemDb: &mockMemDb{}, answered: []*models.Mock{stamped}}
	in := sent("/search", map[string]string{"Content-Type": "application/json"}, `{"txt":"E2E"}`)
	matched, diag := missOf(t, in, db)
	if matched == diag || diag.closest != stamped || diag.phase != models.MatchPhaseSchema {
		t.Fatalf("closest %v, phase %q; want the answered mock-6, of another route", diag.closest, diag.phase)
	}
	for _, f := range diag.differs {
		if f.why != "" {
			t.Fatalf("%s gives the reason %q; want none: the closest recording is of another route", f.shape, f.why)
		}
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: in.method, URL: in.url, Host: "api.example", Header: in.header}, in.body, db, nil, nil, nil, diag)
	if r == nil || r.ClosestMock != "mock-6" || len(r.FieldReasons) != 0 || !hasField(r.Differs, "path") || !hasField(r.Differs, "body.content") {
		t.Fatalf("report %+v; want mock-6 closest, path and body.content named, no field reasons", r)
	}
}

// A recording of another route gives no reason of a keploy.yml noise
// pattern either. A body field the user's pattern guards, whose value in the
// call is outside it, is still named, but the miss neither warns that the
// pattern does not excuse the field nor gives the pattern as its reason: the
// recording is not the call, so its pattern's verdict is not why the call
// missed. Of the call's own route the same field keeps its reason
// (TestStrictNoisePatternMissWarnsItsReason).
func TestARecordingOfAnotherRouteGivesNoNoisePatternReason(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body := map[string][]string{"id": {`^1$`}}
	rec := recorded("mock-x", "/x", map[string]string{"Content-Type": "application/json"}, `{"id":1}`)
	in := sent("/y", map[string]string{"Content-Type": "application/json"}, `{"id":2}`)
	core, logs := observer.New(zap.WarnLevel)
	h := &HTTP{Logger: zap.New(core)}
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	ok, _, diag, err := h.match(context.Background(), in, db, models.ProvenFlakyHeaderNoise(), body, nil, true, false, false, true, true)
	if err != nil || ok || diag == nil {
		t.Fatalf("match ok=%v diag=%v err=%v; want a miss", ok, diag, err)
	}
	r := newHTTP().buildHTTPMismatchReport(&http.Request{Method: in.method, URL: in.url, Host: "api.example", Header: in.header}, in.body, db, nil, nil, body, diag)
	if r == nil || r.ClosestMock != "mock-x" || !hasField(r.Differs, "path") || !hasField(r.Differs, "body.id") || len(r.FieldReasons) != 0 {
		t.Fatalf("report %+v; want mock-x closest, path and body.id named, no field reasons", r)
	}
	if n := logs.FilterMessage("a keploy.yml noise pattern does not excuse a field").Len(); n != 0 {
		t.Fatalf("a recording of another route warned %d times that its pattern does not excuse a field", n)
	}
}

// A call whose path is one recording's, beside a recording whose path
// segment the learn step learned as a read of the clock and the call fills
// with no instant ("me"): that segment is no read of any clock, so the path
// differs there as it would with nothing learned, and the recording is of
// another route. The recording whose path is the call's stays closest,
// compared first or last, and so where it differs in more fields, and no
// read of the clock is blamed for the miss.
func TestALiteralSegmentBesideAClockSegmentKeepsItsRecordingClosest(t *testing.T) {
	json := map[string]string{"Content-Type": "application/json"}
	clk := func() *models.Mock {
		m := recorded("mock-p2", "/v1/u/2026-09-01T09:00:00Z", json, `{"a":2}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}}
		return m
	}
	for _, c := range []struct {
		name, lit string
		want      []string
		lastFirst bool
	}{
		{"one field, compared last", `{"a":1}`, []string{"body.a"}, false},
		{"one field, compared first", `{"a":1}`, []string{"body.a"}, true},
		{"two fields", `{"a":1,"b":1}`, []string{"body.a", "body.b"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := []*models.Mock{clk(), recorded("mock-me", "/v1/u/me", json, c.lit)}
			if c.lastFirst {
				pool[0], pool[1] = pool[1], pool[0]
			}
			r, said := clockMissOf(t, sent("/v1/u/me", json, `{"a":2}`), pool...)
			if r.ClosestMock != "mock-me" || r.MatchPhase != models.MatchPhaseStrict || strings.Join(r.Differs, " ") != strings.Join(c.want, " ") {
				t.Fatalf("closest %q, phase %q, Differs %v; want mock-me, whose path is the call's, phase %q, Differs %v", r.ClosestMock, r.MatchPhase, r.Differs, models.MatchPhaseStrict, c.want)
			}
			if len(r.FieldReasons) != 0 || len(said) != 0 {
				t.Fatalf("FieldReasons %+v, %d clock warnings; want no read of the clock blamed", r.FieldReasons, len(said))
			}
		})
	}
}

// A path segment the learn step learned as a word read of the clock in a
// longer text ("user-<ms>"): a call that carries a stale word there, the
// text beside it the recorded one, is of the recording's route and refused
// for the read; one whose text beside the word changed ("team-<ms>"), or
// whose word is no instant ("user-me"), carries no read of the clock there,
// so its path differs as with nothing learned: another route, no reason, no
// warning.
func TestAWordReadInAPathSegmentIsOfTheRouteOnlyInItsForm(t *testing.T) {
	json := map[string]string{"Content-Type": "application/json"}
	const seg = "user-1730000000000"
	m := func() *models.Mock {
		m := recorded("mock-w", "/v1/u/"+seg, json, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Millisecond, Layout: "epoch-ms", Word: models.ClockWordOf(seg, 5, len(seg))}}
		return m
	}
	r, said := clockMissOf(t, sent("/v1/u/user-1730000000999", json, `{}`), m())
	if r.MatchPhase != models.MatchPhaseStrict || !strings.HasPrefix(reportWhy(r, "path"), "path was learned as ") || len(said) != 1 {
		t.Fatalf("phase %q, the reason of path %q, %d clock warnings; want the stale word refused on the call's route, logged once", r.MatchPhase, reportWhy(r, "path"), len(said))
	}
	for _, other := range []string{"team-1730000000999", "user-me"} {
		r, said := clockMissOf(t, sent("/v1/u/"+other, json, `{}`), m())
		if r.MatchPhase == models.MatchPhaseStrict || len(r.Differs) != 1 || r.Differs[0] != "path" || len(r.FieldReasons) != 0 || len(said) != 0 {
			t.Fatalf("%s: phase %q, Differs %v, FieldReasons %+v, %d clock warnings; want another route, the path alone, no reason, no warning", other, r.MatchPhase, r.Differs, r.FieldReasons, len(said))
		}
	}
}

// keploy.yml's url noise that reaches a path segment learned as a read of
// the clock, in the recording's value and the call's, leaves the segment
// unchecked, as at the clock check (clockDiffs): the call is answered
// though its segment, the noise masked, differs and is no instant whole.
func TestAClockSegmentKeployYmlUrlNoiseReachesIsNotChecked(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	m := recorded("mock-n", "/v1/u/2026-09-01T09:00:00Z", map[string]string{"Content-Type": "application/json"}, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}}
	in := sent("/v1/u/me-T09:00:00Z", map[string]string{"Content-Type": "application/json"}, `{}`)
	in.arrived = base.Add(2 * time.Second)
	if got, diag := strictMatchIn(t, clockConn(true, base), in, nil, nil, []string{`T[0-9:]+Z`}, m); got == nil {
		t.Fatalf("refused (%v); want the segment keploy.yml's noise reaches left unchecked", differs(diag))
	}
}

// keploy.yml's url noise that reaches a path segment learned as a read of
// the clock in one of the two values alone (the call's "me", or the
// recorded instant) excuses nothing there: a pattern reaches a place only
// where it matches both values, as the walk excuses a field. "me" is no
// read of the clock either, so the path differs as with nothing learned:
// the recording is of another route, the path alone named, no reason, no
// warning.
func TestAClockSegmentKeployYmlUrlNoiseReachesInOneValueAloneIsAnotherRoute(t *testing.T) {
	const seg = "me"
	call, rec := "/v1/u/"+seg, pathStamped().Spec.HTTPReq.URL
	recSeg := learnNoise{}.segments(rec)[2]
	for _, c := range []struct {
		name, pat string
		inCall    bool
	}{
		{"the call's value alone", seg + "$", true},
		{"the recorded value alone", `T[0-9:]+Z`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The pattern matches the one value the row names, in its
			// segment and in its whole path alike.
			re := regexp.MustCompile(c.pat)
			if re.MatchString(seg) != c.inCall || re.MatchString(call) != c.inCall || re.MatchString(recSeg) == c.inCall || re.MatchString(rec) == c.inCall {
				t.Fatalf("pattern %q does not match the one value the row names", c.pat)
			}
			r, said := clockMissWhere(t, true, sent(call, map[string]string{"Content-Type": "application/json"}, `{}`), nil, []string{c.pat}, pathStamped())
			if r.ClosestMock != "mock-p" || r.MatchPhase == models.MatchPhaseStrict || strings.Join(r.Differs, " ") != "path" || len(r.FieldReasons) != 0 || len(said) != 0 {
				t.Fatalf("closest %q, phase %q, Differs %v, FieldReasons %+v, %d clock warnings; want mock-p of another route, the path alone, no reason, no warning", r.ClosestMock, r.MatchPhase, r.Differs, r.FieldReasons, len(said))
			}
		})
	}
}

// The miss names a field of a recording of another route by its shape, as
// a failed test's reason does, never a value an object key of it may be.
func TestARecordingOfAnotherRouteNamesAFieldByItsShape(t *testing.T) {
	const key = "3f2b8c1e-9d4a-4b7e-8c1a-2e5f6a7b8c9d"
	json := map[string]string{"Content-Type": "application/json"}
	r, _ := clockMissOf(t, sent("/y", json, `{"u":{"`+key+`":2}}`), recorded("mock-x", "/x", json, `{"u":{"`+key+`":1}}`))
	if r.ClosestMock != "mock-x" || !hasField(r.Differs, "body.u.{}") || strings.Contains(strings.Join(r.Differs, " "), key) {
		t.Fatalf("closest %q, Differs %v; want mock-x, body.u.{} named, never the key", r.ClosestMock, r.Differs)
	}
}

// A place learned as a read of the clock under an object key that may be a
// value, which the call does not carry: the reason and the agent's warning
// name it by its shape, never the key.
func TestAnAbsentClockReadIsNamedByItsShape(t *testing.T) {
	const key = "3f2b8c1e-9d4a-4b7e-8c1a-2e5f6a7b8c9d"
	json := map[string]string{"Content-Type": "application/json"}
	m := recorded("mock-k", "/send", json, `{"u":{"`+key+`":"2026-09-01T09:00:00Z"},"to":"a"}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"body.u." + key: {Res: time.Second}}
	r, said := clockMissOf(t, sent("/send", json, `{"u":{},"to":"a"}`), m)
	found := false
	for _, fr := range r.FieldReasons {
		if strings.Contains(fr.Field+fr.Why, key) {
			t.Fatalf("a reason names the key: %+v", fr)
		}
		found = found || strings.HasSuffix(fr.Why, ", and this call carries no value at body.u.{}: it changed")
	}
	for _, e := range said {
		if c := e.ContextMap(); strings.Contains(c["reason"].(string)+c["field"].(string), key) {
			t.Fatalf("a warning names the key: %v", c)
		}
	}
	if !found || len(said) != 1 {
		t.Fatalf("FieldReasons %+v, %d clock warnings; want the absent reason at body.u.{}, logged once", r.FieldReasons, len(said))
	}
}
