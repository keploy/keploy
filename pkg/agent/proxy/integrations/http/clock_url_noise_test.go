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
// path's own segment 3, and a mock records no url noise with its places, so
// the learned read is checked in both where both hold one. A stale first
// instant is refused, as with no url noise and as where the places were
// learned under the pattern.
func TestAClockSegmentLearnedBeforeAJoiningPatternIsCheckedInBothReadings(t *testing.T) {
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
