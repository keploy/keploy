package learn

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/bits"
	"math/rand"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// foldedLayouts are clockLayouts' patterns compiled in either case: what
// instantSpans ran over every text, one pattern at a time, before
// scanInstants.
var foldedLayouts = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(clockLayouts))
	for i, lay := range clockLayouts {
		out[i] = regexp.MustCompile("(?i)" + lay.re.String())
	}
	return out
}()

// instantSpansByPatterns is instantSpans as it was, its six patterns run
// over s one after another: the reference the one-pass scan is held to.
// passes counts the patterns' scans of s.
func instantSpansByPatterns(s string, passes *int) []instant {
	var out []instant
	var taken []bool
	for k, re := range foldedLayouts {
		lay := clockLayouts[k]
		*passes++
		for _, m := range re.FindAllStringIndex(s, -1) {
			if m[0] > 0 && isWord(s[m[0]-1]) && isWord(s[m[0]]) || m[1] < len(s) && isWord(s[m[1]-1]) && isWord(s[m[1]]) {
				continue
			}
			if taken != nil {
				free := true
				for i := m[0]; i < m[1]; i++ {
					if taken[i] {
						free = false
						break
					}
				}
				if !free {
					continue
				}
			}
			sp := instant{start: m[0], end: m[1], layout: k}
			if lay.timeWord != nil {
				if _, _, ok := lay.read(s[m[0]:m[1]]); !ok {
					continue
				}
				sp.end = m[0] + uuidTimeEnd(s[m[0]:m[1]])
			} else {
				mo, res, ok := instantKeyOf(k, s[m[0]:m[1]])
				if !ok {
					continue
				}
				sp.key, sp.wall, sp.res, sp.zone, sp.zoned, sp.keyed = mo.key, mo.wall, res, mo.zone, mo.zoned, true
			}
			if taken == nil {
				taken = make([]bool, len(s))
			}
			for i := m[0]; i < m[1]; i++ {
				taken[i] = true
			}
			out = append(out, sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// sameAsThePatterns fails t where s's one-pass scan differs from the
// patterns' scans: each layout's matches, and the instants instantSpans
// keeps of them, every field.
func sameAsThePatterns(t *testing.T, s string) {
	t.Helper()
	got := scanInstants(s)
	for k, re := range foldedLayouts {
		var want, have [][2]int
		for _, m := range re.FindAllStringIndex(s, -1) {
			want = append(want, [2]int{m[0], m[1]})
		}
		for _, m := range got[k] {
			have = append(have, [2]int{m.start, m.end})
		}
		if !reflect.DeepEqual(have, want) {
			t.Fatalf("layout %d's matches in %q: scan %v; its pattern %v", k, s, have, want)
		}
	}
	var passes int
	if have, want := instantSpans(s), instantSpansByPatterns(s, &passes); !reflect.DeepEqual(have, want) {
		t.Fatalf("instantSpans(%q) = %+v; the patterns give %+v", s, have, want)
	}
}

// instantCorpus are texts holding every layout's forms, in either case, at
// and off word boundaries, cut short, run together and overlapping: the
// corpus the scan is held to the patterns on, and the fuzzer's seeds.
func instantCorpus() []string {
	at := time.Date(2026, 10, 22, 11, 14, 16, 473210300, time.UTC)
	ms := strconv.FormatInt(at.UnixMilli(), 10)
	out := []string{
		"", "1", "12:3", "12:34", "12:34:5", "12:34:56", "12:34:56.", "12:34:56.1234567891", "1:23:45", "123:45:67",
		"Thu, 22 Oct 2026 11:14:16 GMT", "thu, 22 oct 2026 11:14:16 gmt", "THU, 22 OCT 2026 11:14:16 UTC",
		"Thu, 22 Oct 2026 11:14:16 +0530", "Thu, 22 Oct 2026 11:14:16 -05", "Thu, 22 Oct 2026 11:14:16", "Thu, 22 Oct 2026 11:14:16 PST",
		"Thu, 22 Oct 2026 11:14:16 -0500", "thu, 22 oct 2026 11:14:16 -0500",
		"Thu,22 Oct 2026 11:14:16 GMT", "XThu, 22 Oct 2026 11:14:16 GMT", "Thu, 22 Oct 2026 11:14:16 GMTX",
		"Thu, 31 Feb 2026 11:14:16 GMT", "Sat, 24 Oct 2026 11:14:16 GMT", "ſat, 24 Oct 2026 11:14:16 GMT",
		"Sun, 25 ſep 2026 11:14:16 GMT", "ſUN, 25 ſEP 2026 11:14:16 GMT", "Sun, 27 Sep 2026 11:14:16 KMT",
		"Date: Thu, 22 Oct 2026 11:14:16 GMT; seq 14", "Mon, 26 Oct 2026 11:14:16 GMT, Tue, 27 Oct 2026 11:14:16 GMT",
		"2026-10-22", "2026-10-22T11", "2026-10-22T11Z", "2026-10-22t11z", "2026-10-22T11:14", "2026-10-22T11:14Z",
		"2026-10-22T11:14:16", "2026-10-22T11:14:16Z", "2026-10-22T11:14:16.473Z", "2026-10-22T11:14:16,473Z",
		"2026-10-22T11:14:16.1234567890Z", "2026-10-22T11:14:16.Z", "2026-10-22 11:14:16", "2026-10-22 11:14:16 UTC",
		"2026-10-22 11:14:16UTC", "2026-10-22 11:14:16 gmt", "2026-10-22T11:14:16+05:30", "2026-10-22T11:14:16+0530",
		"2026-10-22T11:14:16+05", "2026-10-22T11:14:16+05:", "2026-10-22T11:14:16+05:3", "2026-10-22T11:14:16-10:00",
		"2026-10-22T11:14:16+", "2026-10-22T11:14:16 +05:30", "2026-10-22T11+05:30", "2026-10-22 11 ok", "2026-10-22T11a",
		"2026-10-22T1", "2026-10-22T11:1", "2026-10-22 UTC", "2026-10-22UTC", "2026-10-22  UTC", "2026-13-45", "12026-10-22",
		"2026-10-221", "2026-10-22T11:14:16ZZ", "2026-10-22T11:14:16 UT", "x2026-10-22", "2026-10-22x",
		// A case-folded text holds its ISO instants with a lowercase t
		// (and z): the form the learn step reads most.
		"2026-10-22t11:14:16.473z", "2026-10-22t11:14z", "2026-10-22t11:14:16+05:30", "2026-10-22t11:14:16",
		"20261022T111413Z", "20261022t111413z", "20261022T111413", "20261022T11141", "20261022T1114131", "x-amz-date=20261022t111413z&n=7",
		"120261022T111413Z", "20261022T111413Za",
		uuid7(at, "abc123456789abcdef"), strings.ToUpper(uuid7(at, "abc123456789abcdef")),
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8", "6ba7b810-9dad-21d1-80b4-00c04fd430c8", "6ba7b810-9dad-11d1-80b4-00c04fd430c",
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8a", "ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323",
		"1792667656", "1792667656.4732103", "1792667656.", "1792667656.1234567891", ms, ms + "000", ms + "000000",
		"17926676564", "179266765647321030", "17926676564732103001", "179266765647321030012345678901234567890",
		"86ba7546 1792667656.4732103", "at=" + ms + " ok", "seq 14 of 41", "order 2025-13-45 x",
		"2026-10-22T11:14:16Z1792667656", "1792667656-10-22", "1234567890-10-22T11:14:16Z", "11:14:16 2026-10-22",
		"2026-10-22T11:14:16.473Z 12:00 20261022T111413Z " + ms,
		// An earlier layout's instant, then a later layout's holding a time
		// of day at a word boundary: the time of day is the later
		// layout's, never an instant of its own.
		"Thu, 22 Oct 2026 11:14:16 GMT 2026-10-22 11:14:16", "2026-10-22 11:14:16 x 2026-10-23 12:00",
		"Thu, 22 Oct 2026 11:14:16 GMT 2026-10-22 11:14:16 1792667656 20261022T111413Z 11:14",
		"\xe2" + "2026-10-22", "\xc5" + "2026-10-22", "\xc5Thu, 22 Oct 2026 11:14:16 GMT", "\x80\x80" + ms, "\xf0\x9f\xc5\xbfat, 24 Oct 2026 11:14:16 GMT",
		`{"_id":{"$oid":"6527f1a2b3c4d5e6f7a8b9c0"},"createdAt":{"$date":"2026-10-22T11:14:16.473Z"},"ts":` + ms + `}`,
	}
	return out
}

// instantFragments are pieces random texts are built of: parts of every
// layout, their separators, letters in either case, and runes that fold.
var instantFragments = []string{
	"0", "1", "2", "7", "9", "00", "12", "59", "2026", "10", "22", "1792667656", "473", "4732103", "123456789",
	"-", "--", ":", ".", ",", " ", "  ", "+", "T", "t", "Z", "z", "UTC", "utc", "GMT", "gmt", "UT", "GM",
	"Mon", "Thu", "thu", "Sat", "Sun", "SUN", "ſ", "ſun", "K", "Oct", "oct", "Sep", "ſep", "Feb", "Fri", ", ",
	"a", "f", "F", "g", "x", "_", "/", "=", `"`, "\xe2", "\xc5", "\x80", "\xbf",
	"2026-10-22", "T11:14", ":16", ".473", "+05:30", "-0500", "20261022T111413", "6ba7b810", "-9dad", "-11d1", "-71d1", "-80b4", "-00c04fd430c8",
	"Thu, 22 Oct 2026 11:14:16 GMT", "11:14:16", "2026-10-22 11:14:16",
	"6ba7b810-9dad-11d1-80b4-00c04fd430c8", "6BA7B810-9DAD-11D1-80B4-00C04FD430C8", "019a0b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b", "ce3bb3a8-8bbe-4fd8-b25d-1efc489f7323",
}

// randomInstantText is a text of n fragments drawn by r.
func randomInstantText(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(instantFragments[r.Intn(len(instantFragments))])
	}
	return b.String()
}

// The one-pass scan finds what clockLayouts' six patterns, each run over the
// whole text, found: each layout's matches, and every instant instantSpans
// keeps of them (span, layout, moment, resolution, zone), equal. On the
// corpus, on each corpus text inside a payload, and on random texts built of
// the layouts' parts. A pattern changed in clockLayouts without its matcher
// fails here.
func TestInstantSpansIsTheSixPatternsScans(t *testing.T) {
	corpus := instantCorpus()
	for _, s := range corpus {
		sameAsThePatterns(t, s)
		sameAsThePatterns(t, `{"k":"`+s+`","n":7}`)
		sameAsThePatterns(t, "a"+s+"9")
	}
	sameAsThePatterns(t, strings.Join(corpus, " "))
	sameAsThePatterns(t, strings.Join(corpus, ""))
	r := rand.New(rand.NewSource(20261029))
	var kept, matched [numLayouts]int
	for i := 0; i < 20000; i++ {
		s := randomInstantText(r, 1+r.Intn(24))
		sameAsThePatterns(t, s)
		for k, ms := range scanInstants(s) {
			matched[k] += len(ms)
		}
		for _, sp := range instantSpans(s) {
			kept[sp.layout]++
		}
	}
	// The random texts reach every layout: matches its rules keep, and
	// matches they drop (off a word boundary, inside an earlier layout's,
	// or no instant).
	for k := range kept {
		if kept[k] < 100 || matched[k]-kept[k] < 100 {
			t.Errorf("layout %d: %d matches in the random texts, %d kept; want at least 100 kept and 100 dropped", k, matched[k], kept[k])
		}
	}
}

func FuzzInstantSpansIsTheSixPatternsScans(f *testing.F) {
	for _, s := range instantCorpus() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { sameAsThePatterns(t, s) })
}

// instantPayload is a payload of n documents like those api-server's set
// serves: ids, an ISO instant, an epoch, a v7 UUID, an HTTP date and a
// base64 blob each.
func instantPayload(n int) string {
	at := time.Date(2026, 10, 22, 11, 14, 16, 473e6, time.UTC)
	var b strings.Builder
	b.WriteString(`{"cursor":{"firstBatch":[`)
	for i := 0; i < n; i++ {
		at = at.Add(1537 * time.Millisecond)
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"_id":{"$oid":"6527f1a2b3c4d5e6f7a8%04x"},"name":"Report %d for team-%d","createdAt":{"$date":"%s"},"ts":%d,"rid":"%s","sent":"%s","blob":"T0RaaVlUYzFORFl0T0dabE9DMDBZamRrTFdJeVlXRXRaREEzWmpKbVpESXpOVFV6","state":"active"}`,
			i, i, i%7, at.Format("2006-01-02T15:04:05.000Z"), at.UnixMilli(), uuid7(at, "abc123456789abcdef"), at.Format(time.RFC1123))
	}
	b.WriteString(`]},"ok":1}`)
	return b.String()
}

// startsIn is the most matchers a pass over s can try: at each byte, one for
// each layout whose matches can begin with it.
func startsIn(s string) (n int64) {
	for i := 0; i < len(s); i++ {
		n += int64(bits.OnesCount8(layoutStarts[s[i]]))
	}
	return n
}

// readsOnce checks what instantSpans of s did, counted where the scan does
// it: one pass, each byte stepped over once, and a matcher tried only at a
// byte its layout's matches can begin with (at least one try when s holds an
// instant). It returns the spans.
func readsOnce(t *testing.T, what, s string) []instant {
	t.Helper()
	passes, steps, tries := instantScanPasses.Load(), instantScanSteps.Load(), instantScanTries.Load()
	spans := instantSpans(s)
	if got := instantScanPasses.Load() - passes; got != 1 {
		t.Errorf("instantSpans of a %d-byte %s made %d passes over it; want 1", len(s), what, got)
	}
	if got := instantScanSteps.Load() - steps; got != int64(len(s)) {
		t.Errorf("instantSpans of a %d-byte %s stepped over %d bytes; want %d", len(s), what, got, len(s))
	}
	got := instantScanTries.Load() - tries
	if limit := startsIn(s); got > limit {
		t.Errorf("instantSpans of a %d-byte %s tried %d matchers; at most %d can begin in it", len(s), what, got, limit)
	}
	if len(spans) > 0 && got == 0 {
		t.Errorf("instantSpans of a %d-byte %s found %d instants and tried no matcher", len(s), what, len(spans))
	}
	return spans
}

// instantSpans reads each text once, for every layout: one pass, stepping
// over each of its bytes once, where clockLayouts' patterns made six passes.
// Counted, never timed: on a payload like api-server's (about 180 KB) and on
// each corpus text.
func TestInstantSpansReadsATextOnce(t *testing.T) {
	texts := append(instantCorpus(), instantPayload(560))
	for _, s := range texts {
		spans := readsOnce(t, "text", s)
		var old int
		if want := instantSpansByPatterns(s, &old); !reflect.DeepEqual(spans, want) {
			t.Fatalf("instantSpans of a %d-byte text = %d instants; the patterns give %d", len(s), len(spans), len(want))
		}
		if old != len(clockLayouts) {
			t.Errorf("the patterns made %d passes; want %d", old, len(clockLayouts))
		}
	}
	if p := instantPayload(560); len(p) < 150<<10 || len(instantSpans(p)) != 4*560 {
		t.Errorf("the payload is %d bytes with %d instants; want about 180 KB and 4 instants a document", len(p), len(instantSpans(p)))
	}
}

// longScript is a minified script of about n*100 bytes, holding no instant:
// the shape a sparse payload's text has, an HTML page's inline bundle.
func longScript(n int) string {
	return strings.Repeat(`function(e,t){var n=e.width||0,r=t.height-1;return{x:n*2,y:r/3,id:"ab12",v:123456789,k:[0,1,2]}};`, n)
}

// A long script holding few instants is read in one pass, each of its bytes
// stepped over once, and its instants are the ones the patterns find. The
// windowed patterns this scan replaced read such a script only around its
// instants' anchors; the scan reads it whole, once, trying a layout's
// matcher only at a byte that layout's matches can begin with.
func TestALongScriptIsReadInOnePass(t *testing.T) {
	script := longScript(2400)
	s := script + `"at":"2026-10-28T07:16:21.123Z",` + script + `"t":1761635781,` + script + `"d":"Wed, 28 Oct 2026 07:16:21 GMT"`
	spans := readsOnce(t, "script", s)
	var old int
	if want := instantSpansByPatterns(s, &old); !reflect.DeepEqual(spans, want) {
		t.Fatalf("instantSpans of the script = %v; the patterns give %v", spans, want)
	}
	if len(spans) != 3 {
		t.Errorf("found %d instants, want the three the script holds: %v", len(spans), spans)
	}
}

// The CPU a payload's instants cost, before (the six patterns) and after
// (the one pass), on a payload like api-server's: a measurement for the
// round's report, run on a test box.
func BenchmarkInstantSpans(b *testing.B) {
	p := instantPayload(560)
	b.Run("six-patterns", func(b *testing.B) {
		b.SetBytes(int64(len(p)))
		var passes int
		for i := 0; i < b.N; i++ {
			instantSpansByPatterns(p, &passes)
		}
	})
	b.Run("one-pass", func(b *testing.B) {
		b.SetBytes(int64(len(p)))
		for i := 0; i < b.N; i++ {
			instantSpans(p)
		}
	})
}

// The CPU a sparse text's instants cost: a 1.7 MB script holding one
// instant, read by the patterns one after another and by the one pass.
func BenchmarkInstantSpansLongScript(b *testing.B) {
	s := longScript(17000) + `"at":"2026-10-28T07:16:21.123Z"`
	b.Run("six-patterns", func(b *testing.B) {
		b.SetBytes(int64(len(s)))
		var passes int
		for i := 0; i < b.N; i++ {
			instantSpansByPatterns(s, &passes)
		}
	})
	b.Run("one-pass", func(b *testing.B) {
		b.SetBytes(int64(len(s)))
		for i := 0; i < b.N; i++ {
			instantSpans(s)
		}
	})
}

// The one pass is the only read of a text for its instants: instants.go and
// instantscan.go, where instantSpans and its scan live, use nothing of the
// regexp package, neither by importing it nor by calling a pattern's Find or
// Match method (clockLayouts' patterns stay in clock.go, for the layouts'
// reads and for this package's rows). A pattern scan put back beside the one
// pass keeps the pass count at one and the spans equal; this row fails on
// it.
func TestInstantSpansRunsNoPattern(t *testing.T) {
	var methods []string
	rt := reflect.TypeOf(&regexp.Regexp{})
	for i := 0; i < rt.NumMethod(); i++ {
		if n := rt.Method(i).Name; strings.HasPrefix(n, "Find") || strings.HasPrefix(n, "Match") || strings.HasPrefix(n, "ReplaceAll") {
			methods = append(methods, n)
		}
	}
	isPatternMethod := func(n string) bool {
		for _, m := range methods {
			if m == n {
				return true
			}
		}
		return false
	}
	fset := token.NewFileSet()
	for _, name := range []string{"instants.go", "instantscan.go"} {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			if im.Path.Value == `"regexp"` || im.Path.Value == `"regexp/syntax"` {
				t.Errorf("%s imports %s: instantSpans reads a text with the one pass alone", name, im.Path.Value)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && isPatternMethod(sel.Sel.Name) {
				t.Errorf("%s: %s calls a pattern's %s: instantSpans reads a text with the one pass alone", name, fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if len(methods) < 10 {
		t.Fatalf("found %d Find/Match methods on *regexp.Regexp: %v", len(methods), methods)
	}
}
