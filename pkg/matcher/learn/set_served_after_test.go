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
	tr := NewTracker()
	tr.Begin()
	var arrival time.Time
	arrive := func() {
		time.Sleep(2 * time.Millisecond)
		arrival = time.Now()
		time.Sleep(2 * time.Millisecond)
	}
	serve := func() {
		if view == nil {
			tr.ServedMockAsRecorded(m)
			return
		}
		tr.ServedMockLive(m, "v1", func() (*models.Mock, error) { return view, nil })
	}
	switch when {
	case servedFirst:
		tr.ServedMock(m)
		serve()
		arrive()
	case servedLater:
		arrive()
		tr.ServedMock(m)
		serve()
	case servedAloft:
		tr.ServedMockIn(m, tr.Lift())
		serve()
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
		why  = "(round F gave it the application only after the call reached keploy)"
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
