package http

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// urlPath reads a path as keploy.yml's url noise masks it whole: its
// segments are the learn step's (learnNoise.segments) and the walk's, each
// marked where a pattern's match is in it, whatever a pattern matches in one
// segment alone; and the path's own segments are marked where a pattern
// matched any of their text. A literal mask in the path is no pattern's
// match, and the "/" a pattern takes is no segment's text.
func TestURLPathIsThePathAsUrlNoiseMasksItWhole(t *testing.T) {
	const p = urlNoisePlaceholder
	for _, c := range []struct {
		path      string
		pats      []string
		segs      []string
		noised    []bool
		own       []string
		ownNoised []bool
	}{
		{"/v1/u/2026-09-01T09:00:00Z", nil, []string{"v1", "u", "2026-09-01T09:00:00Z"}, []bool{false, false, false}, nil, []bool{false, false, false}},
		{"/v1/u/2026-09-01T09:00:00Z", []string{`T[0-9:]+Z`}, []string{"v1", "u", "2026-09-01" + p}, []bool{false, false, true}, nil, []bool{false, false, true}},
		{"/v1/u/2026-09-01T09:00:00Z", []string{`^/v1/u/.*$`}, []string{p}, []bool{true}, nil, []bool{true, true, true}},
		{"/v1/u/2026-09-01T09:00:00Z", []string{`u/[^/]+`}, []string{"v1", p}, []bool{false, true}, nil, []bool{false, true, true}},
		{"/v1/u/2026-09-01T09:00:00Z", []string{`v1/u`}, []string{p, "2026-09-01T09:00:00Z"}, []bool{true, false}, nil, []bool{true, true, false}},
		{"/v1/u/2026-09-01T09:00:00Z", []string{`/u/`}, []string{"v1" + p + "2026-09-01T09:00:00Z"}, []bool{true}, nil, []bool{false, true, false}},
		{"/v1/u", []string{`^/`}, []string{p + "v1", "u"}, []bool{true, false}, nil, []bool{false, false}},
		{"/a/b", []string{`a`, `keploy`}, []string{"{{" + p + ".urlnoise}}", "b"}, []bool{true, false}, nil, []bool{true, false}},
		{"/" + p + "/b", []string{`zzz`}, []string{p, "b"}, []bool{false, false}, nil, []bool{false, false}},
		{"v1//b", []string{`b`}, []string{"v1", "", p}, []bool{false, false, true}, nil, []bool{false, false, true}},
		{"/ab", []string{`x*`}, nil, nil, []string{"ab"}, []bool{false}},
		{"", []string{`x*`}, nil, nil, []string{""}, []bool{false}},
	} {
		t.Run(fmt.Sprintf("%s %q", c.path, c.pats), func(t *testing.T) {
			res := compileURLNoise(zap.NewNop(), c.pats)
			u := strictKnown{n: strictNoise{url: res}}.urlPathOf(c.path)
			noised, ownNoised := marked(u.segs, u.mask), marked(u.own, u.taken)
			if want := (learnNoise{url: res}).segments(c.path); !slices.Equal(u.segs, want) {
				t.Fatalf("segments %q; want the learn step's %q", u.segs, want)
			}
			if want := strings.Split(strings.TrimPrefix(c.path, "/"), "/"); !slices.Equal(u.own, want) || (c.own != nil && !slices.Equal(u.own, c.own)) {
				t.Fatalf("own segments %q; want %q", u.own, want)
			}
			if c.segs != nil && (!slices.Equal(u.segs, c.segs) || !slices.Equal(noised, c.noised)) {
				t.Fatalf("segments %q marked %v; want %q marked %v", u.segs, noised, c.segs, c.noised)
			}
			if !slices.Equal(ownNoised, c.ownNoised) {
				t.Fatalf("own segments %q marked %v; want %v", u.own, ownNoised, c.ownNoised)
			}
		})
	}
}

// at reads the learn step's place url.path.<i> in the masked path, as the
// application sent it where no pattern crossed "/"; past the masked path's
// segments, where a pattern joined the path's own, in the path's own
// segment; past both, nowhere.
func TestURLPathAtReadsTheMaskedSegmentThenThePathsOwn(t *testing.T) {
	u := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), []string{`v1/u`})}}.urlPathOf("/v1/u/2026-09-01T09:00:00Z/x")
	for _, c := range []struct {
		i      int
		seg    string
		noised bool
		ok     bool
	}{
		{0, urlNoisePlaceholder, true, true},
		{1, "2026-09-01T09:00:00Z", false, true},
		{2, "x", false, true},
		{3, "x", false, true},
		{4, "", false, false},
	} {
		if v, ok := u.at(c.i); v.masked != c.seg || slices.Contains(v.mask, true) != c.noised || ok != c.ok {
			seg, noised := v.masked, slices.Contains(v.mask, true)
			t.Fatalf("at(%d) = %q, %v, %v; want %q, %v, %v", c.i, seg, noised, ok, c.seg, c.noised, c.ok)
		}
	}
	inSegment := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), []string{`T[0-9:]+Z`})}}.urlPathOf("/v1/u/2026-09-01T09:00:00Z")
	if v, ok := inSegment.at(2); v.sent != "2026-09-01T09:00:00Z" || v.masked != "2026-09-01"+urlNoisePlaceholder || !slices.Contains(v.mask, true) || !slices.Equal(v.taken, marksOf("2026-09-01", "T09:00:00Z")) || !ok {
		t.Fatalf("at(2) of a path no pattern joined = %+v, %v; want the segment as sent and masked, marked", v, ok)
	}
	whole := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), []string{`^/v1/.*$`})}}.urlPathOf("/v1/u/2026-09-01T09:00:00Z")
	if v, ok := whole.at(2); v.sent != "2026-09-01T09:00:00Z" || v.masked != v.sent || !slices.Equal(v.taken, marksOf("", "2026-09-01T09:00:00Z")) || !slices.Equal(v.mask, v.taken) || !ok {
		t.Fatalf("at(2) of a path masked whole = %+v, %v; want the path's own segment, taken whole", v, ok)
	}
}

// marked says, of each segment, whether any of its bytes is marked (none
// where marks is nil).
func marked(segs []string, marks [][]bool) []bool {
	out := make([]bool, len(segs))
	for i := range segs {
		if marks != nil {
			if len(marks[i]) != len(segs[i]) {
				panic(fmt.Sprintf("segment %q has %d marks", segs[i], len(marks[i])))
			}
			out[i] = slices.Contains(marks[i], true)
		}
	}
	return out
}

// marksOf is the marks of clear+noise: false over clear, true over noise.
func marksOf(clear, noise string) []bool {
	out := make([]bool, len(clear)+len(noise))
	for j := len(clear); j < len(out); j++ {
		out[j] = true
	}
	return out
}

// ownIndexOf is the path's own segment its masked segment i is, whole:
// where a pattern that crosses "/" joined the path, a masked segment that
// holds no mask's byte is one own segment, every byte of it, which no
// pattern's match took; a segment with a mask's byte, an empty one, and
// every segment of a path no pattern joined is none. Read over paths of
// distinct segments, each found by its text alone, under patterns that
// join, take a "/" alone, anchor, or match the empty text.
func TestOwnIndexOfIsTheOwnSegmentAMaskedSegmentIsWhole(t *testing.T) {
	pool := []string{"v1", "u", "users", "123", "events", "x", "y", "z9", "", "2026-09-01T09:00:00Z", "1756717200000", "u-1"}
	pats := []string{`v1/u`, `u/`, `/users/[0-9]+`, `[0-9]+/events`, `^/[^/]+/[^/]+`, `x/y`, `/`, `^`, `x*`, `(?:)`, `z9$`, `[0-9T:-]+Z/`, `u(/z9)?`, `[0-9]+`}
	r := rand.New(rand.NewSource(7))
	found := 0
	for n := 0; n < 20000; n++ {
		segs := slices.Clone(pool)
		r.Shuffle(len(segs), func(a, b int) { segs[a], segs[b] = segs[b], segs[a] })
		path := strings.Join(segs[:1+r.Intn(6)], "/")
		if r.Intn(5) > 0 {
			path = "/" + path
		}
		var ps []string
		for k := r.Intn(3); k > 0; k-- {
			ps = append(ps, pats[r.Intn(len(pats))])
		}
		u := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), ps)}}.urlPathOf(path)
		seen := map[int]bool{}
		for i, seg := range u.segs {
			want := -1
			if u.joined() && seg != "" && !slices.Contains(u.mask[i], true) {
				want = slices.Index(u.own, seg)
				if want < 0 || slices.Contains(u.taken[want], true) {
					t.Fatalf("%q under %q: masked segment %d %q is no own segment whole (own %q)", path, ps, i, seg, u.own)
				}
			}
			if got := u.ownIndexOf(i); got != want || (got >= 0 && seen[got]) {
				t.Fatalf("%q under %q: ownIndexOf(%d) = %d; want %d (masked %q, own %q)", path, ps, i, got, want, u.segs, u.own)
			}
			if want >= 0 {
				seen[want] = true
				found++
			}
		}
	}
	if found == 0 {
		t.Fatalf("no masked segment was an own segment: the paths read no case")
	}
}

// A segment of no instant ("me") where the masked segment is a place read
// as sent is a path difference, its recording another route; an instant
// there is no path difference, its instant the clock check's.
func TestASegmentOfNoInstantAtAPlaceReadAsSentIsAPathDifference(t *testing.T) {
	k := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), []string{`v1/u`})}, clocks: map[string]models.ClockOffset{
		"url.path.2": {Res: time.Second}, "url.path.3": {Res: time.Second}}}
	const rec = "/v1/u/2026-09-01T09:00:00Z/2026-09-01T09:00:05Z"
	if d := k.pathDiffs(rec, "/v1/u/me/2026-10-26T15:50:36Z"); len(d) != 1 || d[0].diff.Path != "path" {
		t.Fatalf("pathDiffs = %v; want the path", fieldDiffs(d))
	}
	if d := k.pathDiffs(rec, "/v1/u/2026-10-26T15:50:36Z/2026-10-26T15:50:36Z"); d != nil {
		t.Fatalf("pathDiffs = %v; want none", fieldDiffs(d))
	}
}

// A masked segment at a learned place the clock check reads in the masked
// path differs only in that place's form, though it is, whole, the own
// segment of a place read as sent of another form. Under `v1/u` the masked
// segment 1 of /v1/u/<T1>/x holds url.path.1, learned there as an ISO 8601
// second, and is url.path.2's own segment, learned as sent in any layout:
// a number of milliseconds there is a path difference; another ISO 8601
// second is none.
func TestAMaskedSegmentAtAMaskedPlaceDiffersOnlyInThatPlacesForm(t *testing.T) {
	k := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), []string{`v1/u`})}, clocks: map[string]models.ClockOffset{
		"url.path.1": {Res: time.Second, Layout: "iso-second"}, "url.path.2": {Res: time.Second}}}
	const rec = "/v1/u/2026-09-01T09:00:00Z/x"
	if d := k.pathDiffs(rec, "/v1/u/1761493836000/x"); len(d) != 1 || d[0].diff.Path != "path" {
		t.Fatalf("pathDiffs = %v; want the path", fieldDiffs(d))
	}
	if d := k.pathDiffs(rec, "/v1/u/2026-10-26T15:50:36Z/x"); d != nil {
		t.Fatalf("pathDiffs = %v; want none", fieldDiffs(d))
	}
}

// A masked segment that is no own segment whole (a mask's byte in it) is
// compared as recorded: one at a place read as sent, also beside a place
// the learn step never writes (url.path.-1, a recording's file edited by
// hand), which names no segment; and one that holds part of a place read
// as sent, where a pattern ends inside its instant, though the call's
// instant there is in its window.
func TestAMaskedSegmentWithAMaskAtAPlaceReadAsSentIsComparedAsRecorded(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	now := models.ClockOffset{Res: time.Second}
	for _, c := range []struct {
		pats      []string
		clocks    map[string]models.ClockOffset
		rec, call string
	}{
		{[]string{`v1/u`, `-x`}, map[string]models.ClockOffset{"url.path.-1": now, "url.path.2": now},
			"/v1/u/2026-09-01T09:00:00Z/a-x", "/v1/u/2026-09-01T09:00:00Z/b-x"},
		{[]string{`v1/u/[0-9]+-[0-9]+`}, map[string]models.ClockOffset{"url.path.2": now},
			"/v1/u/2026-09-01T09:00:00Z/x", "/v1/u/2026-10-26T15:50:36Z/x"},
	} {
		k := strictKnown{n: strictNoise{url: compileURLNoise(zap.NewNop(), c.pats)}, clocks: c.clocks}
		if d := k.pathDiffs(c.rec, c.call); len(d) != 1 || d[0].diff.Path != "path" {
			t.Fatalf("%s under %q: pathDiffs = %v; want the path", c.call, c.pats, fieldDiffs(d))
		}
	}
}
