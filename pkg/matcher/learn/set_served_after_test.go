package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// serveWhen is when round F served a set recording against the instant the
// call reached keploy (Order.ArrivedAt): before it, only after it, or in a
// request of the application's still aloft when it arrived.
type serveWhen int

const (
	servedFirst serveWhen = iota
	servedLater
	servedAloft
)

func (w serveWhen) String() string {
	return [...]string{"before the call arrived", "only after the call arrived", "in a request still aloft when the call arrived"}[w]
}

// givenAround is m as production hands a set recording to a call that
// reached keploy at an instant round F's serve of it was made before or
// after (when): served once, as recorded (view nil) or with the one view
// view, in a tracker's set, and read by Order.ArrivedAt(arrival)
// .GivenPlaces, as integrations' Mongo and Postgres learn steps read it.
func givenAround(t *testing.T, m *models.Mock, when serveWhen, view *models.Mock) GivenPlaces {
	t.Helper()
	return givenToldAround(t, m, when, func(tr *Tracker) {
		if view == nil {
			tr.ServedMockAsRecorded(m)
			return
		}
		tr.ServedMockLive(m, "v1", func() (*models.Mock, error) { return view, nil })
	})
}

// givenToldAround is givenAround with the serve told by tell, called just
// after the serve (nothing: a serve told of nothing).
func givenToldAround(t *testing.T, m *models.Mock, when serveWhen, tell func(tr *Tracker)) GivenPlaces {
	t.Helper()
	tr := NewTracker()
	tr.Begin()
	var arrival time.Time
	arrive := func() {
		time.Sleep(2 * time.Millisecond)
		arrival = time.Now()
		time.Sleep(2 * time.Millisecond)
	}
	switch when {
	case servedFirst:
		tr.ServedMock(m)
		tell(tr)
		arrive()
	case servedLater:
		arrive()
		tr.ServedMock(m)
		tell(tr)
	case servedAloft:
		tr.ServedMockIn(m, tr.Lift())
		tell(tr)
		arrive()
	}
	g, err := tr.Placed().Order.ArrivedAt(arrival).GivenPlaces(m)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// givenRawAfter is m's Raw as Order.ServedView marks one round F served,
// as recorded (view nil) or with the one view view, only after the call
// reached keploy (Raw.servedAfter), read as a set recording: PlacesOf of it.
func givenRawAfter(m, view *models.Mock) GivenPlaces {
	x := clockedDrifts().RawOf(m, true).ServedAs(3)
	if view == nil {
		x = x.ServedAsRecorded()
	} else {
		x = x.ServedLive(clockedDrifts().RawOf(view, true))
	}
	x.servedAfter = true
	return PlacesOf(x)
}

// arrivedShape is a set recording handed to a call one way (how).
type arrivedShape struct {
	how   string
	after bool
	g     GivenPlaces
}

// arrivedAround is m served as recorded (view nil) or with view, read each
// way production hands it: through the tracker before the call arrived,
// only after it, and aloft when it arrived, and as a marked Raw served only
// after it.
func arrivedAround(t *testing.T, m, view *models.Mock) []arrivedShape {
	t.Helper()
	var out []arrivedShape
	for _, w := range []serveWhen{servedFirst, servedLater, servedAloft} {
		out = append(out, arrivedShape{"through the tracker, served " + w.String(), w != servedFirst, givenAround(t, m, w, view)})
	}
	return append(out, arrivedShape{"a marked Raw served only after the call arrived", true, givenRawAfter(m, view)})
}

// A set recording holding the recorded group of an id the call minted,
// answered before the call in the recording, that round F served only
// after the call reached keploy, or in a request still aloft then, was no
// place round F's application had before the call: its making its own
// group shows nothing of whether the application cut the recorded one from
// there, so the group is refused, naming the recording and that reason,
// as a Raw place round F served only after the call is (fGave). Served
// before the call arrived, as recorded or with one view none of round F's
// group, it is chance: the id fresh. Each shape: the set recording alone,
// beside a place of the call's own test left unknown that holds the group
// too, and alone with no answer time.
func TestOutcomeASetRecordingServedOnlyAfterTheCallIsNoPlaceRoundFHad(t *testing.T) {
	const (
		path          = "body.docs.msgs.0._id"
		rec, live     = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f          = "48d1", "4b8a"
		field         = "body.cursor.firstBatch.0._id"
		holdsR, noneF = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
		line          = "the recording's own piece of the id stands at mock-31, which round F did not give the application before the call: round F's making its own piece shows nothing of whether the application cut it from there"
	)
	timed := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	untimed := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = time.Time{}
		return m
	}
	given := func(g GivenPlaces) func() ([]GivenPlaces, error) {
		return func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
	}
	for _, s := range []struct {
		name   string
		mock   func(string) *models.Mock
		decide func(g GivenPlaces) Decision
	}{
		{"alone", timed, func(g GivenPlaces) Decision {
			in := oInput(path, rec, live, nil, nil)
			in.Given = given(g)
			return oWord(t, Decide(in), r, f)
		}},
		{"beside an unknown own place", timed, func(g GivenPlaces) Decision {
			in := oInput(path, rec, live, oServed("mock-51", false, false, Response, oText(field, holdsR), noneF, 2*time.Second), nil)
			in.Given = given(g)
			return oWord(t, Decide(in), r, f)
		}},
		{"alone, with no answer time", untimed, func(g GivenPlaces) Decision {
			in := oInput("body.appId", rec, live, nil, nil)
			in.Given = given(g)
			return oWord(t, Decide(in), r, f)
		}},
	} {
		for _, v := range []struct {
			what string
			view *models.Mock
		}{{"as recorded", nil}, {"with one view none of round F's group", s.mock(noneF)}} {
			for _, a := range arrivedAround(t, s.mock(holdsR), v.view) {
				what := s.name + ": " + a.how + ", " + v.what
				d := s.decide(a.g)
				if !a.after {
					costFresh(t, what, d)
					continue
				}
				oRefused(t, what, d, RefusalUnknown, "mock-31")
				if s.name != "beside an unknown own place" {
					oOneSideLine(t, d, line)
				}
			}
		}
	}
}

// A whole word at a set recording round F served only after the call
// reached keploy, or in a request still aloft then, is no evidence of where
// round F's application took its word: round F gave the application
// nothing there before the call. The recorded word there, answered before
// the call, is refused, saying so: not set aside as a copy that would have
// moved with its source (a short word), nor refused as a conflict (one of
// 32 bits or more), as where round F gave it before the call. Round F's
// word only in what round F gave there after the call is downstream of the
// call (the application wrote it, then read it back), no source of it: the
// word is fresh, as where round F served a Raw place only after the call
// (Raw.asOfArrival). Before the call, round F's view holding it is.
func TestOutcomeAWordAtASetRecordingServedOnlyAfterTheCallIsNoEvidence(t *testing.T) {
	const (
		path = "body.ref"
		why  = "before this call: round F gave it to the application only after the call reached keploy"
	)
	set := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	decide := func(rec, live string, g GivenPlaces) Decision {
		in := oInput(path, rec, live, nil, nil)
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
		return oWord(t, Decide(in), rec, live)
	}
	for _, c := range []struct {
		what      string
		rec, live string
		before    RefusalKind // "": fresh where round F gave it before the call
	}{
		{"a short word", "7f3a9c", "2b8e41", ""},
		{"a word of 32 bits or more", "7f3a9c5d1e", "2b8e41a0c7", RefusalConflict},
	} {
		for _, v := range []struct {
			what string
			view *models.Mock
		}{{"as recorded", nil}, {"with one view none of round F's word", set("0a0a0a0a0a")}} {
			for _, a := range arrivedAround(t, set(c.rec), v.view) {
				what := c.what + " the recording held there: " + a.how + ", " + v.what
				d := decide(c.rec, c.live, a.g)
				switch {
				case a.after:
					oRefused(t, what, d, RefusalUnknown, "mock-31")
					if d.Refusal != nil && !strings.Contains(d.Refusal.Why, why) {
						t.Errorf("%s: refused %q; want it to say %s", what, d.Refusal.Why, why)
					}
				case c.before == "":
					costFresh(t, what, d)
				default:
					oRefused(t, what, d, c.before, "mock-31")
				}
			}
		}
		for _, a := range arrivedAround(t, set("0a0a0a0a0a"), set(c.live)) {
			what := c.what + " round F's view alone held there: " + a.how
			d := decide(c.rec, c.live, a.g)
			if a.after {
				costFresh(t, what, d)
				continue
			}
			oRefused(t, what, d, RefusalUnknown, "mock-31")
		}
	}
}

// An outside recording's words a set keeps (outsideWords) say whether round
// F gave it the application before the call that reads them, not before
// the first call that kept them: its kept entry is keyed by that too
// (wordsKey.servedAfter). Handed, served as recorded, to three calls of one
// set, before the first and third arrived and only after the second and
// fourth did: the recorded word there is set aside where round F gave it
// before the call, and refused where round F gave it only after, its words
// read once each way.
func TestOutcomeAnOutsideRecordingsKeptWordsSayWhenRoundFGaveIt(t *testing.T) {
	const rec, live = "ids 7f3a9c", "ids 2b8e41"
	m := getAnswer("mock-7", "http://api/items/7", `{"ref":"7f3a9c"}`, eRecAt.Add(-time.Minute))
	tr := NewTracker()
	tr.NewSet()
	for c, after := range []bool{false, true, false, true} {
		tr.Begin()
		dc := tr.Drifts()
		x := dc.LazyRawOf(m, true, nil).ServedAs(1).ServedAsRecorded().OfAnotherTest()
		x.servedAfter = after
		b := wordsRead(dc)
		d := oWord(t, Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, []Raw{x}, nil, nil)), "7f3a9c", "2b8e41")
		builds := wordsRead(dc) - b
		tr.End()
		what := "call " + strconv.Itoa(c+1) + ", round F served it " + map[bool]string{false: "before the call", true: "only after the call"}[after]
		t.Logf("%s: %s (words read %d)", what, oDescribe([]Decision{d}), builds)
		if want := []int{1, 1, 0, 0}[c]; builds != want {
			t.Errorf("%s: read its words %d times; want %d", what, builds, want)
		}
		if !after {
			costFresh(t, what, d)
			continue
		}
		oRefused(t, what, d, RefusalUnknown, "mock-7")
	}
}

// servedAfterLine is the line of a word refused at a set recording round F
// served only after the call reached keploy (servedAfterWhy).
const servedAfterLine = "keploy cannot tell what round F gave at mock-31 before this call: round F gave it to the application only after the call reached keploy"

// oServedAfterRefused checks d is refused as unknown at mock-31, on the
// line that says round F served it only after the call.
func oServedAfterRefused(t *testing.T, what string, d Decision) {
	t.Helper()
	oRefused(t, what, d, RefusalUnknown, "mock-31")
	if d.Refusal != nil && d.Refusal.Why != servedAfterLine {
		t.Errorf("%s: refused %q; want %q", what, d.Refusal.Why, servedAfterLine)
	}
}

// Round F's word standing whole in a set recording's own recorded answer,
// served as recorded, was written in the recording run, before round F
// existed: no word round F's application made. Where round F served that
// recording only after the call reached keploy, or in a request still aloft
// then, it shows nothing of where round F's application took its word, so
// the word is refused as unknown with that reason, never learned fresh: a
// short word and one of 32 bits or more, the recording answered before the
// call in the recording or after it, through the tracker and as a marked
// Raw, as an own Raw place served as recorded only after the call is
// (Raw.asOfArrival). Served before the call, answered before it, the short
// word is set aside and the long one a conflict; answered after it, each
// is refused.
func TestOutcomeRoundFsWordInASetRecordingsOwnAnswerServedOnlyAfterTheCallIsRefused(t *testing.T) {
	answeredBefore := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	answeredAfter := func(held string) *models.Mock { return receiveOf("mock-31", held, oAt.Add(time.Minute)) }
	for _, c := range []struct {
		what      string
		rec, live string
		before    RefusalKind // "": fresh, served before the call and answered before it
	}{
		{"a short word", "7f3a9c", "2b8e41", ""},
		{"a word of 32 bits or more", "7f3a9c5d1e", "2b8e41a0c7", RefusalConflict},
	} {
		for _, s := range []struct {
			what   string
			mock   func(string) *models.Mock
			before RefusalKind
		}{
			{"answered before the call in the recording", answeredBefore, c.before},
			{"answered after the call in the recording", answeredAfter, RefusalUnknown},
		} {
			for _, a := range arrivedAround(t, s.mock(c.live), nil) {
				what := c.what + " of round F's in the recording's own answer, " + s.what + ": " + a.how
				in := oInput("body.ref", c.rec, c.live, nil, nil)
				g := a.g
				in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
				d := oWord(t, Decide(in), c.rec, c.live)
				switch {
				case a.after:
					oServedAfterRefused(t, what, d)
				case s.before == "":
					costFresh(t, what, d)
				default:
					oRefused(t, what, d, s.before, "mock-31")
				}
			}
		}
	}
}

// A set recording round F served with a serve told of nothing, or with a
// view keploy could not keep, gave round F's application something keploy
// cannot read. Served only after the call reached keploy, or in a request
// still aloft then, whatever it gave is downstream of the call, no source
// of it: where the recording's text holds neither word, the word is fresh;
// where it holds the recorded word, answered before the call in the
// recording, that word is refused as unknown with that reason. Served
// before the call, keploy cannot tell round F's word did not come from it:
// refused as unknown either way.
func TestOutcomeAnUnreadSetRecordingServedOnlyAfterTheCallIsNoSource(t *testing.T) {
	set := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	for _, c := range []struct{ what, rec, live string }{
		{"a short word", "7f3a9c", "2b8e41"},
		{"a word of 32 bits or more", "7f3a9c5d1e", "2b8e41a0c7"},
	} {
		for _, h := range []struct {
			what, held string
			recorded   bool
		}{{"neither word", "0a0a0a0a0a", false}, {"the recorded word", c.rec, true}} {
			for _, tell := range []struct {
				what string
				tell func(m *models.Mock) func(tr *Tracker)
			}{
				{"told of nothing", func(*models.Mock) func(*Tracker) { return func(*Tracker) {} }},
				{"with a view keploy could not keep", func(m *models.Mock) func(*Tracker) {
					return func(tr *Tracker) { tr.ServedMockLive(m, "", nil) }
				}},
			} {
				for _, w := range []serveWhen{servedFirst, servedLater, servedAloft} {
					what := c.what + ", the recording holding " + h.what + ", served " + tell.what + " " + w.String()
					m := set(h.held)
					g := givenToldAround(t, m, w, tell.tell(m))
					in := oInput("body.ref", c.rec, c.live, nil, nil)
					in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
					d := oWord(t, Decide(in), c.rec, c.live)
					switch {
					case w == servedFirst:
						oRefused(t, what, d, RefusalUnknown, "mock-31")
					case h.recorded:
						oServedAfterRefused(t, what, d)
					default:
						costFresh(t, what, d)
					}
				}
			}
		}
	}
}

// A set recording with no answer time keploy can read, or no recorded
// time, holding the recorded whole word, that round F served only after the call
// reached keploy, or in a request still aloft then, is refused saying round
// F gave it only after the call: that is why it shows nothing, whatever its
// times. Served before the call, the line says which time keploy cannot
// read.
func TestOutcomeAnUntimedSetRecordingServedOnlyAfterTheCallSaysSo(t *testing.T) {
	for _, u := range []struct {
		what, why string
		mock      func(held string) *models.Mock
	}{
		{"with no answer time", "(" + noAnswerAt + ")", func(held string) *models.Mock {
			m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
			m.Spec.ResTimestampMock = time.Time{}
			return m
		}},
		{"with neither a request nor an answer time", "(" + noAnswerAt + ")", func(held string) *models.Mock {
			m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
			m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = time.Time{}, time.Time{}
			return m
		}},
	} {
		for _, c := range []struct{ what, rec, live string }{
			{"a short word", "7f3a9c", "2b8e41"},
			{"a word of 32 bits or more", "7f3a9c5d1e", "2b8e41a0c7"},
		} {
			for _, a := range arrivedAround(t, u.mock(c.rec), nil) {
				what := c.what + " the recording " + u.what + " held there: " + a.how
				in := oInput("body.ref", c.rec, c.live, nil, nil)
				g := a.g
				in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
				d := oWord(t, Decide(in), c.rec, c.live)
				if a.after {
					oServedAfterRefused(t, what, d)
					continue
				}
				oRefused(t, what, d, RefusalUnknown, "mock-31")
				if d.Refusal != nil && !strings.Contains(d.Refusal.Why, u.why) {
					t.Errorf("%s: refused %q; want it to say %q", what, d.Refusal.Why, u.why)
				}
			}
		}
	}
	// A Raw whose recorded time keploy cannot read (Raw.noTime), as the
	// rows of a set recording with no recorded time build it.
	for _, after := range []bool{false, true} {
		x := clockedDrifts().RawOf(receiveOf("mock-31", "7f3a9c5d1e", oAt.Add(-time.Minute)), true).ServedAs(3).ServedAsRecorded()
		x.at, x.noTime, x.servedAfter = oAt, true, after
		what := "a marked Raw with no recorded time keploy can read, served as recorded " + map[bool]string{false: "before the call", true: "only after the call"}[after]
		in := oInput("body.ref", "7f3a9c5d1e", "2b8e41a0c7", nil, nil)
		g := PlacesOf(x)
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
		d := oWord(t, Decide(in), "7f3a9c5d1e", "2b8e41a0c7")
		if after {
			oServedAfterRefused(t, what, d)
			continue
		}
		oRefused(t, what, d, RefusalUnknown, "mock-31")
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "("+noAnswerAt+")") {
			t.Errorf("%s: refused %q; want it to say %q", what, d.Refusal.Why, noAnswerAt)
		}
	}
}

// Round F's piece of an id the call minted standing in a set recording's
// own recorded answer, served as recorded, is no piece round F's
// application made there. Where round F served the recording only after
// the call reached keploy, or in a request still aloft then, the piece
// there is unknown, as a whole word is: where the recording shows nothing
// of the id's long words, the id's pieces are made whole, so it is chance
// and the id is fresh, the line naming the recording; where it holds the
// recorded id's first long word too, the piece is refused with that
// reason. Served before the call, it is set aside, as a copy moves with
// its source.
func TestOutcomeRoundFsPieceInASetRecordingsOwnAnswerServedOnlyAfterTheCall(t *testing.T) {
	const (
		path      = "body.docs.msgs.0._id"
		rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f      = "48d1", "4b8a"
		holdsF    = "c1ae4c69-77aa-4b8a-b2c3-d4e5f6071829"
		chance    = `mock-31 holds "48d1" or "4b8a" as a piece of an id, and nothing of the id's words of 32 bits or more`
	)
	for _, h := range []struct {
		what, held string
		idShown    bool
	}{
		{"round F's piece alone", holdsF, false},
		{"round F's piece and the recorded id's first long word", holdsF + " ed35a78f", true},
	} {
		m := receiveOf("mock-31", h.held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		for _, a := range arrivedAround(t, m, nil) {
			what := h.what + " in the recording's own answer: " + a.how
			in := oInput(path, rec, live, nil, nil)
			g := a.g
			in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
			d := oWord(t, Decide(in), r, f)
			switch {
			case a.after && h.idShown:
				oServedAfterRefused(t, what, d)
			case a.after:
				costFresh(t, what, d)
				if !strings.Contains(d.Line, chance) {
					t.Errorf("%s: line %q; want it to name the recording as chance: %s", what, d.Line, chance)
				}
			default:
				costFresh(t, what, d)
			}
		}
	}
}
