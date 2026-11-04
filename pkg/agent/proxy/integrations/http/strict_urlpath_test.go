package http

import (
	"fmt"
	"slices"
	"strings"
	"testing"

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
