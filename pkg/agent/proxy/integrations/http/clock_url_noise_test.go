package http

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const (
	urlNoiseT1    = "2026-09-01T09:00:00Z"
	urlNoiseT2    = "2026-09-01T09:00:05Z"
	urlNoiseStale = "2026-09-02T09:00:00Z"
	// urlNoiseNow is read on the application's clock a second after
	// urlNoiseBase, a second before the call arrives.
	urlNoiseNow = "2026-10-26T15:50:36Z"
)

var (
	urlNoiseJSON = map[string]string{"Content-Type": "application/json"}
	urlNoiseBase = time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
)

// urlNoiseServed is the mock served for a call to path under keploy.yml's
// url noise pats, the application's clock at urlNoiseBase.
func urlNoiseServed(t *testing.T, path string, pats []string, m *models.Mock) *models.Mock {
	t.Helper()
	in := sent(path, urlNoiseJSON, `{}`)
	in.arrived = urlNoiseBase.Add(2 * time.Second)
	got, _ := strictMatchIn(t, clockConn(true, urlNoiseBase), in, nil, nil, pats, m)
	return got
}

// A keploy.yml url pattern excuses only the words it matches: a clock read
// learned as a word of a query value or a path segment is still checked
// where a pattern matches other text of the value, or only part of the
// word, and is read in the text the learn step read it in, masked where it
// learned under the pattern, as sent where it learned before the pattern
// was added. A pattern that covers the whole word in both excuses the read,
// and nothing else of the value.
func TestAUrlPatternExcusesOnlyTheClockWordItCovers(t *testing.T) {
	const p = urlNoisePlaceholder
	inst := func(prefix, at, suffix string) string { return prefix + at + suffix }
	for _, c := range []struct {
		name         string
		recPath      string
		place        string
		text         string // the text the learn step read, with the word at bytes start-end
		start, end   int
		pats         []string
		call         func(at string) string
		staleServed  bool
		freshRefused bool
	}{
		{name: "query value, learned under a pattern beside the word",
			recPath: "/v1/q?at=id-" + urlNoiseT1, place: "url.query.at", text: p + urlNoiseT1, start: len(p), end: len(p) + len(urlNoiseT1),
			pats: []string{`id-`}, call: func(at string) string { return inst("/v1/q?at=id-", at, "") }},
		{name: "query value, learned before a pattern beside the word",
			recPath: "/v1/q?at=id-" + urlNoiseT1, place: "url.query.at", text: "id-" + urlNoiseT1, start: 3, end: 3 + len(urlNoiseT1),
			pats: []string{`id-`}, call: func(at string) string { return inst("/v1/q?at=id-", at, "") }},
		{name: "query value, a pattern covering part of the word",
			recPath: "/v1/q?at=id-" + urlNoiseT1, place: "url.query.at", text: "id-" + urlNoiseT1, start: 3, end: 3 + len(urlNoiseT1),
			pats: []string{`T[0-9:]+Z`}, call: func(at string) string { return inst("/v1/q?at=id-", at, "") }},
		{name: "query value, a pattern covering the word",
			recPath: "/v1/q?at=id-" + urlNoiseT1, place: "url.query.at", text: "id-" + urlNoiseT1, start: 3, end: 3 + len(urlNoiseT1),
			pats: []string{`[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]+Z`}, call: func(at string) string { return inst("/v1/q?at=id-", at, "") }, staleServed: true},
		{name: "query value, a pattern covering the word, the text beside it changed",
			recPath: "/v1/q?at=id-" + urlNoiseT1, place: "url.query.at", text: "id-" + urlNoiseT1, start: 3, end: 3 + len(urlNoiseT1),
			pats: []string{`[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]+Z`}, call: func(at string) string { return inst("/v1/q?at=xx-", at, "") }, freshRefused: true},
		{name: "path segment a pattern joined, learned under it beside the word",
			recPath: "/v1/u/" + urlNoiseT1, place: "url.path.1", text: p + urlNoiseT1, start: len(p), end: len(p) + len(urlNoiseT1),
			pats: []string{`u/`}, call: func(at string) string { return inst("/v1/u/", at, "") }},
		{name: "first path segment, learned under a pattern beside the word",
			recPath: "/id-" + urlNoiseT1 + "/x", place: "url.path.0", text: p + urlNoiseT1, start: len(p), end: len(p) + len(urlNoiseT1),
			pats: []string{`id-`}, call: func(at string) string { return inst("/id-", at, "/x") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("mock-w", c.recPath, urlNoiseJSON, `{}`)
			m.Spec.Clocks = map[string]models.ClockOffset{c.place: {Res: time.Second, Word: models.ClockWordOf(c.text, c.start, c.end)}}
			if got := urlNoiseServed(t, c.call(urlNoiseStale), c.pats, m); (got != nil) != c.staleServed {
				t.Errorf("a stale instant: served %v; want %v", got != nil, c.staleServed)
			}
			if got := urlNoiseServed(t, c.call(urlNoiseNow), c.pats, m); (got == nil) != c.freshRefused {
				t.Errorf("a fresh instant: refused %v; want %v", got == nil, c.freshRefused)
			}
		})
	}
}

// Places learned on a path before a pattern that crosses "/" was added
// (url.path.2 and url.path.3, two instants), then keploy.yml gains `v1/u`,
// which joins the segments before them: the masked path's segment 2 is the
// path's own segment 3, and a mock records no url noise with its places.
// The recording says which path the learn step read: its masked path has
// no segment 3, so the places are read in the path as sent, and the stale
// first instant at the path's own segment 2 is checked. It is refused, as
// with no url noise and as where the places were learned under the
// pattern.
func TestAClockSegmentLearnedBeforeAJoiningPatternIsCheckedWhereItWasLearned(t *testing.T) {
	for _, c := range []struct {
		name   string
		places []string
		pats   []string
	}{
		{"learned before the pattern", []string{"url.path.2", "url.path.3"}, []string{`v1/u`}},
		{"no url noise", []string{"url.path.2", "url.path.3"}, nil},
		{"learned under the pattern", []string{"url.path.1", "url.path.2"}, []string{`v1/u`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("mock-2", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
			m.Spec.Clocks = map[string]models.ClockOffset{}
			for _, place := range c.places {
				m.Spec.Clocks[place] = models.ClockOffset{Res: time.Second}
			}
			if got := urlNoiseServed(t, "/v1/u/"+urlNoiseT1+"/"+urlNoiseNow, c.pats, m); got != nil {
				t.Fatalf("served %s: the stale recorded first instant was never checked", got.Name)
			}
		})
	}
}

// Two clock segments a recording holds the same instant at, learned under
// `v1/u` (url.path.1 and url.path.2 of the masked path): the masked
// segment 2 and the path's own segment 2 then hold the same text in the
// recording, yet are other segments of the call, and each learned read is
// checked at the learn step's place. A stale instant at either is refused;
// two reads in the call's window are answered.
func TestTwoEqualClockSegmentsAfterAJoiningPatternAreEachChecked(t *testing.T) {
	m := recorded("mock-2", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT1, urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": {Res: time.Second}, "url.path.2": {Res: time.Second}}
	for _, c := range []struct {
		first, second string
		answered      bool
	}{
		{urlNoiseNow, urlNoiseStale, false},
		{urlNoiseStale, urlNoiseNow, false},
		{urlNoiseNow, urlNoiseNow, true},
	} {
		if got := urlNoiseServed(t, "/v1/u/"+c.first+"/"+c.second, []string{`v1/u`}, m); (got != nil) != c.answered {
			t.Errorf("/v1/u/%s/%s: answered %v; want %v", c.first, c.second, got != nil, c.answered)
		}
	}
}

// A recording's URL is absolute, as the recorder writes it: the clock check
// reads its path, as for a path-only recording, under url noise that masks
// the whole path, crosses "/", or names one bare segment.
func TestAnAbsoluteRecordedURLIsReadInItsPathUnderUrlNoise(t *testing.T) {
	for _, pat := range []string{`^/v1/u/.*$`, `u/[^/]+`} {
		t.Run(pat, func(t *testing.T) {
			m := pathStamped()
			m.Spec.HTTPReq.URL = "http://api.example/v1/u/" + urlNoiseT1
			if got := urlNoiseServed(t, "/v1/u/"+urlNoiseStale, []string{pat}, m); got == nil {
				t.Fatal("refused; want answered as for a path-only recording")
			}
		})
	}
	t.Run("a bare segment's pattern", func(t *testing.T) {
		m := pathStamped()
		m.Spec.HTTPReq.URL = "http://api.example/v1/u/" + urlNoiseT1
		r, said := clockMissWhere(t, true, sent("/v1/u/"+urlNoiseStale, urlNoiseJSON, `{}`), nil, []string{`^[0-9T:-]+Z$`}, m)
		if why := reportWhy(r, "path"); r.ClosestMock != "mock-p" || !strings.HasPrefix(why, "path was learned as ") || len(said) != 1 {
			t.Fatalf("closest %q, why %q, %d warnings; want mock-p refused for its stale read, logged once", r.ClosestMock, why, len(said))
		}
	})
}

// With no url noise, a path is split once and a query value is read as it
// is: the walk's cost per candidate stays what it is without the masks.
func TestUrlValuesWithNoUrlNoiseAllocateOnlyThePathsSplit(t *testing.T) {
	k := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), nil)}}
	if n := testing.AllocsPerRun(100, func() { _ = k.urlPathOf("/v1/u/" + urlNoiseT1) }); n != 1 {
		t.Errorf("urlPathOf allocates %v times; want 1, the split", n)
	}
	if n := testing.AllocsPerRun(100, func() { _ = k.urlValueOf("id-" + urlNoiseT1) }); n != 0 {
		t.Errorf("urlValueOf allocates %v times; want none", n)
	}
}

// A pattern that covers a path segment learned as a read of the clock whole,
// in the recording and the call, excuses it whatever it holds: a call whose
// segment is no instant, masked as a run of other length, is answered.
func TestAUrlPatternCoveringAClockSegmentWholeExcusesWhateverItHolds(t *testing.T) {
	m := pathStamped()
	if got := urlNoiseServed(t, "/v1/u/abc", []string{`[^/]`}, m); got == nil {
		t.Fatal("refused; want answered: every byte of the segment is noise in both")
	}
}

// A query value at a place learned as a read of the clock whole that is
// empty in the recording (a file edited by hand) and in the call is covered
// by no pattern, one that matches the empty text included: the call is
// refused. So is a word read whose place in the text is no place (a hand
// edit), under a pattern that matches in the value, and nothing panics.
func TestAUrlPatternCoversNoEmptyValueNorAWordOfNoPlace(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	m := recorded("mock-e", "/v1/q?at=", urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.query.at": {Res: time.Second}}
	if got := urlNoiseServed(t, "/v1/q?at=", []string{`x*`}, m); got != nil {
		t.Fatal("an empty value answered: a match of the empty text covered it")
	}
	m = recorded("mock-h", "/v1/q?at=id-"+urlNoiseT1, urlNoiseJSON, `{}`)
	w := models.ClockWordOf("id-"+urlNoiseT1, 3, 3+len(urlNoiseT1))
	w.Start = -1
	m.Spec.Clocks = map[string]models.ClockOffset{"url.query.at": {Res: time.Second, Word: w}}
	if got := urlNoiseServed(t, "/v1/q?at=id-"+urlNoiseT1, []string{`id-`}, m); got != nil {
		t.Fatal("a word of no place answered")
	}
}

// A time range in the path, /users/123/events/<from>/<to>, learned under
// keploy.yml's `/users/[0-9]+`, set before recording: url.path.2 (<from>, a
// day before the call) and url.path.3 (<to>, the call's) of the masked
// path. The path's own segment 3 holds <from> too, yet the recording says
// the learn step read the masked path: its own segment 2 ("events") holds
// no instant. So each place is read in the masked path alone: both
// instants fresh are answered, and a stale one at either is refused.
func TestATimeRangeLearnedUnderAJoiningPatternIsReadInTheMaskedPath(t *testing.T) {
	const (
		from = "2026-08-31T09:00:00Z" // recorded, a day before urlNoiseT1
		ago  = "2026-10-25T15:50:36Z" // a day before urlNoiseNow
	)
	day := models.ClockOffset{Min: -24 * time.Hour, Max: -24*time.Hour + time.Second, Res: time.Second}
	now := models.ClockOffset{Res: time.Second}
	for _, c := range []struct {
		name, rec, call string
		pats            []string
		clocks          map[string]models.ClockOffset
		answered        bool
	}{
		{"/users/[0-9]+, both fresh", "/users/123/events/" + from + "/" + urlNoiseT1, "/users/123/events/" + ago + "/" + urlNoiseNow,
			[]string{`/users/[0-9]+`}, map[string]models.ClockOffset{"url.path.2": day, "url.path.3": now}, true},
		{"/users/[0-9]+, a stale from", "/users/123/events/" + from + "/" + urlNoiseT1, "/users/123/events/" + urlNoiseStale + "/" + urlNoiseNow,
			[]string{`/users/[0-9]+`}, map[string]models.ClockOffset{"url.path.2": day, "url.path.3": now}, false},
		{"/users/[0-9]+, a stale to", "/users/123/events/" + from + "/" + urlNoiseT1, "/users/123/events/" + ago + "/" + urlNoiseStale,
			[]string{`/users/[0-9]+`}, map[string]models.ClockOffset{"url.path.2": day, "url.path.3": now}, false},
		{"v1/u, both fresh", "/v1/u/" + from + "/" + urlNoiseT1, "/v1/u/" + ago + "/" + urlNoiseNow,
			[]string{`v1/u`}, map[string]models.ClockOffset{"url.path.1": day, "url.path.2": now}, true},
		{"no url noise, both fresh", "/v1/u/" + from + "/" + urlNoiseT1, "/v1/u/" + ago + "/" + urlNoiseNow,
			nil, map[string]models.ClockOffset{"url.path.2": day, "url.path.3": now}, true},
		{"one clock segment joined, fresh", "/users/123/events/" + urlNoiseT1, "/users/123/events/" + urlNoiseNow,
			[]string{`/users/[0-9]+`}, map[string]models.ClockOffset{"url.path.2": now}, true},
		{"v1/u inside the path and a query clock read, all fresh", "/api/v1/u/" + from + "/" + urlNoiseT1 + "?at=" + urlNoiseT1,
			"/api/v1/u/" + ago + "/" + urlNoiseNow + "?at=" + urlNoiseNow,
			[]string{`v1/u`}, map[string]models.ClockOffset{"url.path.2": day, "url.path.3": now, "url.query.at": now}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("mock-r", c.rec, urlNoiseJSON, `{}`)
			m.Spec.Clocks = c.clocks
			if got := urlNoiseServed(t, c.call, c.pats, m); (got != nil) != c.answered {
				t.Errorf("%s: answered %v; want %v", c.call, got != nil, c.answered)
			}
		})
	}
}

// Places learned on the path as sent (url.path.2 and url.path.3), then
// keploy.yml gains `v1/u`, which joins segments, and a pattern that covers
// the masked path's segment 2 whole. The noise covers the masked segment,
// yet the place is read where the recording says it was learned, the
// path's own segment 2, which holds the stale recorded instant: refused.
func TestNoiseCoveringTheMaskedSegmentLeavesTheOwnLearnedSegmentChecked(t *testing.T) {
	m := recorded("mock-e", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}, "url.path.3": {Res: time.Second}}
	if got := urlNoiseServed(t, "/v1/u/"+urlNoiseT1+"/"+urlNoiseNow, []string{`v1/u`, `[0-9T:-]+Z$`}, m); got != nil {
		t.Fatalf("served %s: the stale recorded instant at the learned place url.path.2 was never checked", got.Name)
	}
}

// A place learned under `v1/u` (url.path.1 of the masked path), whose
// masked segment a second pattern covers whole in the recording and the
// call: the user declared it noise, so a stale instant there is answered.
func TestNoiseCoveringAJoinedPathsLearnedSegmentWholeExcusesIt(t *testing.T) {
	m := recorded("mock-c", "/v1/u/"+urlNoiseT1+"/x", urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": {Res: time.Second}}
	if got := urlNoiseServed(t, "/v1/u/"+urlNoiseStale+"/x", []string{`v1/u`, `[0-9T:-]+Z`}, m); got == nil {
		t.Fatalf("refused; want answered: the pattern covers the segment whole in both")
	}
}

// A place learned on the path as sent (url.path.2, an instant), then
// keploy.yml gains `v1/u`: the masked path's segment 2 is the path's own
// segment 3, "x", no instant, so the recording says the place was read in
// the path as sent. The masked segment 2 is then not the learned place: a
// call that sends an instant there for "x" is another route, refused,
// though its instant is fresh; the recorded path is answered.
func TestAMaskedSegmentBesideAPlaceLearnedAsSentIsComparedAsSent(t *testing.T) {
	m := recorded("mock-x", "/v1/u/"+urlNoiseNow+"/x", urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}}
	for _, c := range []struct {
		call     string
		answered bool
	}{
		{"/v1/u/" + urlNoiseNow + "/" + urlNoiseNow, false},
		{"/v1/u/" + urlNoiseNow + "/x", true},
	} {
		if got := urlNoiseServed(t, c.call, []string{`v1/u`}, m); (got != nil) != c.answered {
			t.Errorf("%s: answered %v; want %v", c.call, got != nil, c.answered)
		}
	}
}

// Places learned under two url noises: url.path.1 holds an instant only in
// the masked path under `v1/u` ("u" as sent), and url.path.2 of the path
// /v1/u/<instant>/x only as sent (its masked segment 2 is "x"), so no one
// path is the mock's. url.path.2 is read where its recording holds it, the
// path's own segment 2: a call with both reads in its window is answered.
func TestAPlaceItsRecordingHoldsAsSentAloneIsReadAsSent(t *testing.T) {
	m := recorded("mock-a", "/v1/u/"+urlNoiseT1+"/x", urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": {Res: time.Second}, "url.path.2": {Res: time.Second}}
	if got := urlNoiseServed(t, "/v1/u/"+urlNoiseNow+"/x", []string{`v1/u`}, m); got == nil {
		t.Fatalf("refused; want answered: url.path.2 is the path's own segment 2, a read in the call's window")
	}
}

// A time range learned on the path as sent (url.path.2, a day back, and
// url.path.3), then keploy.yml gains `v1/u`: the masked path has no segment
// 3, so the recording says the places were read as sent, and the masked
// segment 2 (the path's own segment 3, <to>) is not url.path.2's. The
// recording replayed in its own window is answered.
func TestATimeRangeLearnedBeforeAJoiningPatternIsReadAsSent(t *testing.T) {
	const ago = "2026-10-25T15:50:36Z" // a day before urlNoiseNow
	day := models.ClockOffset{Min: -24 * time.Hour, Max: -24*time.Hour + time.Second, Res: time.Second}
	m := recorded("mock-s", "/v1/u/"+ago+"/"+urlNoiseNow, urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": day, "url.path.3": {Res: time.Second}}
	if got := urlNoiseServed(t, "/v1/u/"+ago+"/"+urlNoiseNow, []string{`v1/u`}, m); got == nil {
		t.Fatalf("refused; want answered: each read holds at its place in the path as sent")
	}
}

// Places learned under `v1/u` (url.path.1, the call's clock, and url.path.2,
// a day back), then a pattern covers the masked segment 2 whole. Its
// recording no longer says what segment 2 held, but url.path.1 holds an
// instant only in the masked path, so the mock was read there: url.path.2
// is the masked segment the noise covers, excused, and a stale value there
// is answered.
func TestNoiseCoveringAMaskedLearnedSegmentExcusesItBesideAnotherInstant(t *testing.T) {
	day := models.ClockOffset{Min: -24 * time.Hour, Max: -24*time.Hour + time.Second, Res: time.Second}
	m := recorded("mock-v", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": {Res: time.Second}, "url.path.2": day}
	if got := urlNoiseServed(t, "/v1/u/"+urlNoiseNow+"/"+urlNoiseStale, []string{`v1/u`, `[0-9T:-]+Z$`}, m); got == nil {
		t.Fatalf("refused; want answered: url.path.2 is the masked segment the pattern covers whole")
	}
}

// Places whose recording reads in no one path: url.path.1 holds an instant
// only in the masked path under `v1/u` ("u" as sent), url.path.3 only as
// sent (the masked path has no segment 3), so they were learned under two
// url noises. url.path.2 holds an instant in both, and the recording does
// not say which the learn step read: both are checked (fail closed). A call
// whose own segment 2 does not hold at url.path.2's offset is refused, and
// so is one whose masked segment 2 does not; a call where both hold is
// answered.
func TestAPlaceTheRecordingReadsInNoOnePathIsCheckedInBoth(t *testing.T) {
	const ago = "2026-10-25T15:50:36Z" // a day before urlNoiseNow
	now := models.ClockOffset{Res: time.Second}
	day := models.ClockOffset{Min: -24 * time.Hour, Max: -24*time.Hour + time.Second, Res: time.Second}
	for _, c := range []struct {
		name     string
		second   models.ClockOffset
		third    models.ClockOffset
		call     string
		answered bool
	}{
		{"the own segment stale", day, day, "/v1/u/" + urlNoiseNow + "/" + ago, false},
		{"the masked segment stale", now, day, "/v1/u/" + urlNoiseNow + "/" + ago, false},
		{"both in the window", now, now, "/v1/u/" + urlNoiseNow + "/" + urlNoiseNow, true},
	} {
		m := recorded("mock-b", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": now, "url.path.2": c.second, "url.path.3": c.third}
		if got := urlNoiseServed(t, c.call, []string{`v1/u`}, m); (got != nil) != c.answered {
			t.Errorf("%s: answered %v; want %v", c.name, got != nil, c.answered)
		}
	}
}

// Places learned on the path as sent (/reports/<T1>/<T2>), then keploy.yml
// gains `reports/[0-9T:-]+Z`, which crosses "/" and covers url.path.1
// whole: the masked path is [mask, <T2>], so its segment 1 is url.path.2's
// own segment, which the clock check reads as sent. A call with every read
// in its window is answered, at the same offsets and at a time range's; a
// stale read there is refused; url.path.1, which the pattern covers whole
// in both, is noise.
func TestAPlaceReadAsSentIsTheMaskedSegmentItIsWhole(t *testing.T) {
	const ago = "2026-10-25T15:50:36Z" // a day before urlNoiseNow
	now := models.ClockOffset{Res: time.Second}
	day := models.ClockOffset{Min: -24 * time.Hour, Max: -24*time.Hour + time.Second, Res: time.Second}
	for _, c := range []struct {
		name     string
		first    models.ClockOffset
		call     string
		answered bool
	}{
		{"both now", now, "/reports/" + urlNoiseNow + "/" + urlNoiseNow, true},
		{"both now, a stale second", now, "/reports/" + urlNoiseNow + "/" + urlNoiseStale, false},
		{"both now, a stale first the pattern covers", now, "/reports/" + urlNoiseStale + "/" + urlNoiseNow, true},
		{"a time range", day, "/reports/" + ago + "/" + urlNoiseNow, true},
		{"a time range, a stale to", day, "/reports/" + ago + "/" + urlNoiseStale, false},
	} {
		m := recorded("mock-r", "/reports/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": c.first, "url.path.2": now}
		if got := urlNoiseServed(t, c.call, []string{`reports/[0-9T:-]+Z`}, m); (got != nil) != c.answered {
			t.Errorf("%s: answered %v; want %v", c.name, got != nil, c.answered)
		}
	}
}

// A time range learned on the path as sent (url.path.2, a day back, and
// url.path.3), then keploy.yml gains `v1/u`: the masked path's segments 1
// and 2 are the path's own segments 2 and 3 whole, the places the clock
// check reads. A call with other instants, each in its window, is of the
// recording's route, answered; a stale instant at either place is refused.
func TestATimeRangeLearnedBeforeAJoiningPatternIsAnsweredInItsWindow(t *testing.T) {
	const ago = "2026-10-25T15:50:36Z" // a day before urlNoiseNow
	day := models.ClockOffset{Min: -24 * time.Hour, Max: -24*time.Hour + time.Second, Res: time.Second}
	for _, c := range []struct {
		call     string
		answered bool
	}{
		{"/v1/u/" + ago + "/" + urlNoiseNow, true},
		{"/v1/u/" + urlNoiseStale + "/" + urlNoiseNow, false},
		{"/v1/u/" + ago + "/" + urlNoiseStale, false},
	} {
		m := recorded("mock-t", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": day, "url.path.3": {Res: time.Second}}
		if got := urlNoiseServed(t, c.call, []string{`v1/u`}, m); (got != nil) != c.answered {
			t.Errorf("%s: answered %v; want %v", c.call, got != nil, c.answered)
		}
	}
}

// A place read as sent excuses the masked segment it is only where it is
// that segment in the call's path too: under a pattern that joins more of
// the call's segments, the call's masked segment 1 is its own segment 5, so
// a call of seven segments is another route, though the clock check's
// places hold instants in its window; a call the pattern joins as it did
// the recording is answered.
func TestAMaskedSegmentThatIsAnotherOwnSegmentInTheCallIsComparedAsRecorded(t *testing.T) {
	const pat = `v1/u(/[0-9T:-]+Z/[0-9T:-]+Z/y)?`
	for _, c := range []struct {
		call     string
		answered bool
	}{
		{"/v1/u/" + urlNoiseNow + "/" + urlNoiseNow + "/y/" + urlNoiseNow + "/" + urlNoiseT2, false},
		{"/v1/u/" + urlNoiseNow + "/" + urlNoiseNow, true},
	} {
		m := recorded("mock-j", "/v1/u/"+urlNoiseT1+"/"+urlNoiseT2, urlNoiseJSON, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}, "url.path.3": {Res: time.Second}}
		if got := urlNoiseServed(t, c.call, []string{pat}, m); (got != nil) != c.answered {
			t.Errorf("%s: answered %v; want %v", c.call, got != nil, c.answered)
		}
	}
}

// Places learned under `v1/u` (url.path.1 and url.path.3): the masked
// segment 2, "x", is the path's own segment 3, a learned place but one the
// clock check reads in the masked path, not there. A call that puts an
// instant for "x" is another route, refused; the recorded "x" is answered.
func TestAMaskedSegmentThatIsTheOwnSegmentOfAMaskedPlaceIsComparedAsRecorded(t *testing.T) {
	for _, c := range []struct {
		call     string
		answered bool
	}{
		{"/v1/u/" + urlNoiseNow + "/" + urlNoiseNow + "/" + urlNoiseNow, false},
		{"/v1/u/" + urlNoiseNow + "/x/" + urlNoiseNow, true},
	} {
		m := recorded("mock-m", "/v1/u/"+urlNoiseT1+"/x/"+urlNoiseT2, urlNoiseJSON, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.1": {Res: time.Second}, "url.path.3": {Res: time.Second}}
		if got := urlNoiseServed(t, c.call, []string{`v1/u`}, m); (got != nil) != c.answered {
			t.Errorf("%s: answered %v; want %v", c.call, got != nil, c.answered)
		}
	}
}
