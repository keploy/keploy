package learn

import (
	"encoding/base64"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// instantOfWord is the instant the reader reads at the first changed word
// of (rec, live), nil where it reads none.
func instantOfWord(t *testing.T, rec, live string) *instantWord {
	t.Helper()
	rd := readText(rec, live)
	if len(rd.Words) == 0 {
		t.Fatalf("%q -> %q: no changed word", rec, live)
	}
	return rd.Words[0].Instant
}

// Every number of 10, 13, 16 or 19 digits in either run is an instant:
// one that parses (2000-2100) by its moment, one that does not by its
// exact text. Other lengths are no instant.
func TestReaderEpochNumbersAreInstants(t *testing.T) {
	for _, c := range []struct {
		what, rec, live string
		res             time.Duration
	}{
		{"seconds", "1792667656", "1792667731", time.Second},
		{"milliseconds", "1792667656473", "1792667731901", time.Millisecond},
		{"microseconds", "1792667656473120", "1792667731901555", time.Microsecond},
		{"nanoseconds", "1792667656473120999", "1792667731901555123", time.Nanosecond},
		{"seconds in a text", "at=1792667656;n=2", "at=1792667731;n=2", time.Second},
	} {
		iw := instantOfWord(t, c.rec, c.live)
		if iw == nil || !iw.Number || iw.ByText || iw.Rec == nil || iw.Live == nil {
			t.Errorf("%s (%q -> %q): %+v; want a number read by its moment in both runs", c.what, c.rec, c.live, iw)
			continue
		}
		if iw.Rec.res != c.res || iw.Live.res != c.res {
			t.Errorf("%s: resolution %v/%v; want %v", c.what, iw.Rec.res, iw.Live.res, c.res)
		}
	}
	// A 13-digit number outside 2000-2100: matched by its exact text.
	for _, c := range []struct{ what, rec, live string }{
		{"13 digits before 2000", "0000000000473", "0000000000901"},
		{"13 digits after 2100", "9999999999473", "9999999999901"},
		{"10 digits in one run only parses", "1792667656", "0000000001"},
		// An epoch's length in one run is enough: the other run's number
		// is matched by its text, parse or not.
		{"10 digits in R, 8 in F", "1792667656", "17926676"},
		{"10 digits in R that do not parse, 8 in F", "0000000001", "17926676"},
	} {
		iw := instantOfWord(t, c.rec, c.live)
		if iw == nil || !iw.Number || !iw.ByText {
			t.Errorf("%s (%q -> %q): %+v; want a number matched by its exact text", c.what, c.rec, c.live, iw)
		}
	}
	for _, c := range []struct{ rec, live string }{{"12345", "12399"}, {"123456789012", "123456789099"}, {"17926676561", "17926677311"}} {
		if iw := instantOfWord(t, c.rec, c.live); iw != nil {
			t.Errorf("%q -> %q: an instant %+v; want none (no epoch length)", c.rec, c.live, iw)
		}
	}
}

// Every token that parses in main's layouts is an instant, read whole: the
// changed words of one instant share one reading of it.
func TestReaderLayoutTokensAreInstants(t *testing.T) {
	rec, live := "due 2026-10-31T10:50:13.305Z sharp", "due 2026-10-31T10:51:27.905Z sharp"
	rd := readText(rec, live)
	if len(rd.Words) < 2 {
		t.Fatalf("%q -> %q: %q; want the instant's changed words", rec, live, changedPairs(rd))
	}
	first := rd.Words[0].Instant
	for _, w := range rd.Words {
		if w.Instant == nil || w.Instant != first {
			t.Errorf("word %q>%q: instant %p; want the one instant %p", w.Rec, w.Live, w.Instant, first)
		}
	}
	if first == nil || first.Rec == nil || rec[first.RecAt[0]:first.RecAt[1]] != "2026-10-31T10:50:13.305Z" || live[first.LiveAt[0]:first.LiveAt[1]] != "2026-10-31T10:51:27.905Z" {
		t.Errorf("the instant %+v; want it whole in each run", first)
	}
	if len(rd.Instants) != 1 {
		t.Errorf("instants %d; want one", len(rd.Instants))
	}
}

// A cut finer than a millisecond is fine; a millisecond or coarser is
// coarse: microseconds and nanoseconds are fine, milliseconds, BSON dates,
// 13-digit epochs and seconds are coarse.
func TestReaderFineAndCoarseCuts(t *testing.T) {
	for _, c := range []struct {
		what, rec, live string
		fine            bool
	}{
		{"nanoseconds", "1792667656473120999", "1792667731901555123", true},
		{"microseconds", "1792667656473120", "1792667731901555", true},
		{"an ISO microsecond", "2026-10-31T10:50:13.305120Z", "2026-10-31T10:51:27.905555Z", true},
		{"a 13-digit epoch", "1792667656473", "1792667731901", false},
		{"an ISO millisecond", "2026-10-31T10:50:13.305Z", "2026-10-31T10:51:27.905Z", false},
		{"seconds", "1792667656", "1792667731", false},
	} {
		iw := instantOfWord(t, c.rec, c.live)
		if iw == nil || iw.fine() != c.fine {
			t.Errorf("%s: %+v fine %v; want %v", c.what, iw, iw != nil && iw.fine(), c.fine)
		}
	}
	for _, c := range []struct {
		res  time.Duration
		fine bool
	}{{time.Nanosecond, true}, {100 * time.Nanosecond, true}, {time.Microsecond, true}, {100 * time.Microsecond, true}, {time.Millisecond, false}, {time.Second, false}} {
		if fineCut(c.res) != c.fine {
			t.Errorf("fineCut(%v) = %v; want %v", c.res, !c.fine, c.fine)
		}
	}
	// A BSON date: typed, a millisecond: coarse, whole.
	at := time.Date(2026, 10, 31, 10, 50, 13, 305_000_000, time.UTC)
	iw, ok := readTypedPair(readerBSONDate(at), readerBSONDate(at.Add(74*time.Second)))
	if !ok || !iw.Typed || iw.fine() || iw.Rec == nil || iw.Rec.res != time.Millisecond {
		t.Errorf("a BSON date: %+v %v; want a typed instant, coarse", iw, ok)
	}
	if _, ok := readTypedPair(Leaf{Path: "body.n", Type: String, Value: "x"}, Leaf{Path: "body.n", Type: String, Value: "y"}); ok {
		t.Errorf("a string leaf read as a typed instant")
	}
}

// readerBSONDate is a BSON datetime leaf at at, as the Mongo decoder gives one.
func readerBSONDate(at time.Time) Leaf {
	return InstantLeaf("body.at", "body.at", at, time.Millisecond)
}

// By moment is main's cuts: one rule names the held instant's moment.
// Equal keys at a fine resolution are a fine copy; a millisecond copy, or a
// value that is the second of a finer read the place holds, is coarse; a
// finer value is no copy of a second the place holds, and a moment no rule
// names is not held.
func TestReaderByMoment(t *testing.T) {
	read := func(s string) moment {
		sp := instantSpans(s)
		if len(sp) != 1 {
			t.Fatalf("%q: %d instants", s, len(sp))
		}
		return sp[0].moment()
	}
	for _, c := range []struct {
		what, held, m string
		ok, fine      bool
	}{
		{"a nanosecond copy", "1792667656473120999", "1792667656473120999", true, true},
		{"a millisecond copy", "1792667656473", "1792667656473", true, false},
		{"a value that is the second of a microsecond read held", "2026-10-21T08:34:16.473120Z", "2026-10-21T08:34:16Z", true, false},
		{"a microsecond value beside a second held: no copy", "2026-10-21T08:34:16Z", "2026-10-21T08:34:16.473120Z", false, false},
		{"another moment", "2026-10-21T08:34:16Z", "2026-10-21T09:34:17Z", false, false},
		// One moment at two resolutions is cut at the coarser of them.
		{"a nanosecond read held, its millisecond copy", "1792667656473000000", "1792667656473", true, false},
		{"a millisecond read held, its nanosecond copy", "1792667656473", "1792667656473000000", true, false},
		// Of several rules that name the value, the finest is the cut: a
		// microsecond's truncation names it as well as a millisecond's.
		{"the finest of several cuts", "2026-10-21T08:34:16.473000999Z", "2026-10-21T08:34:16.473Z", true, true},
	} {
		ok, fine := byMoment(read(c.held), read(c.m))
		if ok != c.ok || fine != c.fine {
			t.Errorf("%s: held %v fine %v; want %v %v", c.what, ok, fine, c.ok, c.fine)
		}
	}
	x := newReaderIndex(nil)
	held, fine, readOK := x.holdsMoment(`{"at":"`+strings.Repeat("x", 3)+` 1792667656473120999"}`, read("1792667656473120999"))
	if !held || !fine || !readOK {
		t.Errorf("a place holding the nanosecond read: held %v fine %v read %v", held, fine, readOK)
	}
}

// Round F's view of a recording it gave the application earlier in the set
// (Tracker.ServedMockLive: a view told for a serve the read saw) is kept as
// instants, read as the recording's answer is (GivenPlaces.answer), so that
// a changed instant that copies what F gave there is judged as at any other
// place. Only under the proven switch; nothing where F gave the recording
// as recorded, where keploy cannot tell what F gave (its words say so), or
// with the switch off. The set reads each view once, however many calls
// read the recording.
func TestReaderKeepsRoundFsViewOfASetRecordingsInstants(t *testing.T) {
	const (
		recAt   = "2026-10-20T10:00:00.123456Z"
		viewAt  = "2026-10-20T10:00:05.654321Z"
		otherAt = "2026-10-20T10:00:09.111111Z"
	)
	momentOf := func(s string) moment {
		t.Helper()
		sp := NewDriftCache().spansOf(s)
		if len(sp) != 1 || !sp[0].keyed {
			t.Fatalf("%q reads as %d instants; want one", s, len(sp))
		}
		return sp[0].moment()
	}
	keysOf := func(ms []givenMoment) []instantKey {
		var out []instantKey
		for _, g := range ms {
			if !slices.Contains(out, g.m.key) {
				out = append(out, g.m.key)
			}
		}
		return out
	}
	m := jobsMock(recAt, "5")
	loads := map[string]int{}
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) { loads[id]++; return jobsMock(id, "9"), nil }
	}
	// A view whose own stamps say nothing of when it was recorded: its
	// instants stand where the recording does all the same.
	unstamped := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			loads[id]++
			v := jobsMock(id, "9")
			v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = time.Time{}, time.Time{}
			return v, nil
		}
	}
	for _, c := range []struct {
		name   string
		proven string
		tell   func(tr *Tracker)
		want   []string // the instants of fView, in order
	}{
		{"told with a view", "1", func(tr *Tracker) { tr.ServedMockLive(m, "a", view(viewAt)) }, []string{viewAt}},
		{"told with a view, the switch off", "0", func(tr *Tracker) { tr.ServedMockLive(m, "a", view(viewAt)) }, nil},
		{"told as recorded", "1", func(tr *Tracker) { tr.ServedMockAsRecorded(m) }, nil},
		{"told nothing", "1", func(*Tracker) {}, nil},
		{"told with a view that cannot be read", "1", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}, nil},
		{"told as recorded, then with a view", "1", func(tr *Tracker) {
			tr.ServedMockAsRecorded(m)
			tr.ServedMock(m)
			tr.ServedMockLive(m, "a", view(viewAt))
		}, []string{viewAt}},
		{"told with a view stamped with no time", "1", func(tr *Tracker) { tr.ServedMockLive(m, "a", unstamped(viewAt)) }, []string{viewAt}},
		{"told with two views", "1", func(tr *Tracker) {
			tr.ServedMockLive(m, "a", view(viewAt))
			tr.ServedMock(m)
			tr.ServedMockLive(m, "b", view(otherAt))
		}, []string{viewAt, otherAt}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.proven)
			clear(loads)
			tr := NewTracker()
			tr.Begin()
			tr.ServedMock(m)
			c.tell(tr)
			tr.End()
			tr.Begin()
			tr.End()
			tr.Begin()
			o := tr.Placed().Order
			for _, read := range []string{"first read", "cached read"} {
				g, err := o.GivenPlaces(m)
				if err != nil {
					t.Fatalf("%s: %v", read, err)
				}
				// Each instant may be read in more than one form of its
				// field (readAnswerMoments): compare the instants named.
				if got := keysOf(g.answer); len(got) != 1 || got[0] != momentOf(recAt).key {
					t.Errorf("%s: the recording's answer names %d instants; want its own one, as recorded", read, len(got))
				}
				got := keysOf(g.fView)
				if len(got) != len(c.want) {
					t.Fatalf("%s: round F's view names %d instants (%+v); want %d", read, len(got), g.fView, len(c.want))
				}
				for k, want := range c.want {
					if got[k] != momentOf(want).key {
						t.Errorf("%s: view instant %d is not %s", read, k, want)
					}
				}
				for _, v := range g.fView {
					if v.mock != m.Name || !strings.HasPrefix(v.where, m.Name+" at ") || !v.at.Equal(m.Spec.ReqTimestampMock) {
						t.Errorf("%s: a view instant stands at %q of %q recorded %v; want mock-6's field, recorded when mock-6 was", read, v.where, v.mock, v.at)
					}
					if a := g.answer[0]; v.late != a.late || v.noTime != a.noTime {
						t.Errorf("%s: a view instant reads late %v, no time %v; want the recording's %v, %v", read, v.late, v.noTime, a.late, a.noTime)
					}
				}
			}
			// With the switch off the set keeps no view's instants: it reads
			// the views for their words, and nothing reads their instants.
			if o.places != nil && c.proven == "0" {
				if len(o.places.views) == 0 {
					t.Errorf("the set kept no view: the switch-off row reads nothing")
				}
				for key, v := range o.places.views {
					if len(v.moments) != 0 {
						t.Errorf("switch off: the set keeps %d instants of the view %q", len(v.moments), key)
					}
				}
			}
			for id, n := range loads {
				if n != 1 {
					t.Errorf("the view %s was read %d times over two reads of one set; want once", id, n)
				}
			}
		})
	}
	// A view its words were read from that cannot be read again for its
	// instants (an order keeping no set's places reads each view anew) is
	// one keploy cannot tell: its words say unread, the fail-closed side.
	t.Run("a view read for its words and not again", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		reads := 0
		tr.ServedMockLive(m, "a", func() (*models.Mock, error) {
			if reads++; reads > 1 {
				return nil, errors.New("gone")
			}
			return jobsMock(viewAt, "9"), nil
		})
		tr.End()
		tr.Begin()
		o := tr.Placed().Order
		o.places = nil
		g, err := o.GivenPlaces(m)
		if err != nil {
			t.Fatal(err)
		}
		if !g.words.unread || len(g.fView) != 0 {
			t.Errorf("words unread %v, %d view instants; want unread and none", g.words.unread, len(g.fView))
		}
	})
}

// An earlier place holds an instant in any form main reads it in: its text
// and its declared forms (holdsExact(text) || holdsExact(rawExtra(text))),
// so a percent-encoded query, a base64url cursor or a base64 copy holds a
// recorded µs instant at its fine cut, as the plain text does.
func TestReaderIndexHoldsAnInstantInEveryFormMainReads(t *testing.T) {
	r := "2026-10-31T10:50:13.305123+05:30"
	m := readText(r, "2026-10-31T10:51:27.905456+05:30").Instants[0].Rec
	dc := NewDriftCache()
	for _, place := range []string{
		"ok " + r,
		"/jobs?since=" + url.QueryEscape(r) + "&page=2",
		`{"next":"` + base64.RawURLEncoding.EncodeToString([]byte(`{"since":"`+r+`","page":2}`)) + `"}`,
		`{"c":"` + base64.StdEncoding.EncodeToString([]byte(r)) + `"}`,
	} {
		if !dc.holdsExact(place, *m) && !dc.holdsExact(dc.rawExtra(place), *m) {
			t.Fatalf("%q: main does not hold the moment: the row reads nothing", place)
		}
		for _, x := range []*readerIndex{newReaderIndex(nil), newReaderIndex(dc)} {
			if held, fine, read := x.text(place).holdsMoment(*m); !held || !fine || !read {
				t.Errorf("%q: main holds the moment exactly; the reader's index held %v fine %v read %v", place, held, fine, read)
			}
		}
	}
}

// An instant whose word count changes between the runs is one instant read
// whole in each run: a zero fraction dropped (RFC3339Nano, Java's
// Instant.toString), or a ' ' to 'T' with a zone added. Its moved words are
// an unequal stretch, and the instants each run's words lie in pair by
// position. Where the runs' words there lie in unequal counts of instants,
// nothing pairs, and each run's instant stays one-sided.
func TestReaderAnInstantWhoseWordCountChangesIsOneInstant(t *testing.T) {
	for _, c := range []struct{ rec, live, rInst, lInst string }{
		{"x 2026-10-31T10:50:13.305Z y", "x 2026-10-31T10:51:27Z y", "2026-10-31T10:50:13.305Z", "2026-10-31T10:51:27Z"},
		{"2026-10-31 10:50:13.305+05:30", "2026-10-31 10:51:27+05:30", "2026-10-31 10:50:13.305+05:30", "2026-10-31 10:51:27+05:30"},
		{"2024-01-02 03:04:05", "2024-01-02T03:04:05Z", "2024-01-02 03:04:05", "2024-01-02T03:04:05Z"},
		{"x 2026-10-31T10:50:13.305Z y 17", "x 2026-10-31T10:51:27Z y 18", "2026-10-31T10:50:13.305Z", "2026-10-31T10:51:27Z"},
	} {
		rd := readText(c.rec, c.live)
		var one *instantWord
		for _, w := range rd.Words {
			if w.Rec == "17" {
				if w.Instant != nil {
					t.Errorf("%q -> %q: the word 17>18 reads as an instant %+v", c.rec, c.live, w.Instant)
				}
				continue
			}
			if w.Instant == nil || one != nil && w.Instant != one {
				t.Errorf("%q -> %q: word %q>%q instant %+v; want the one instant", c.rec, c.live, w.Rec, w.Live, w.Instant)
				continue
			}
			one = w.Instant
		}
		if one == nil || one.Rec == nil || one.Live == nil || len(rd.Instants) != 1 {
			t.Errorf("%q -> %q: %d instants, %+v; want one, with a moment in each run", c.rec, c.live, len(rd.Instants), one)
			continue
		}
		if got := c.rec[one.RecAt[0]:one.RecAt[1]]; got != c.rInst {
			t.Errorf("%q: R's instant %q; want %q", c.rec, got, c.rInst)
		}
		if got := c.live[one.LiveAt[0]:one.LiveAt[1]]; got != c.lInst {
			t.Errorf("%q: F's instant %q; want %q", c.live, got, c.lInst)
		}
	}
	// R's moved words lie in two instants, F's in one: nothing pairs.
	rd := readText("at 10:50:13.305 10:50:14 b", "at 10:51:27 b")
	for _, iw := range rd.Instants {
		if iw.Rec != nil && iw.Live != nil {
			t.Errorf("an instant paired across unequal counts of instants: %+v", iw)
		}
	}
	if len(rd.Instants) < 2 {
		t.Errorf("%d instants; want each run's own", len(rd.Instants))
	}
}
