package learn

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording F served changed in ways keploy keeps no one view of
// (Raw.ServedChanged): two serves of it in the set gave the application
// different views, or a serve's view could not be read. Read as served with
// no view, F gave nothing there, and a word only a view holds passed as
// noise: a value the application copied from what F served, unchecked at
// replay (FALSE GREEN). A word the value holds now that stands in any of the
// views, and not in the recorded text, is refused; where a view could not be
// read, so is every word the value holds now. A word no view holds is no
// copy of what F gave there: the value is learned as with no view. So under
// ClassifyGiven and ClassifyFollowing, read whole or lazily, in a tracker's
// test (the views bear the recording's identity, MockID, and the set's
// caches by recording must not hand them the recording's payload) and with
// a cache of no tracker.
func TestAWordAServeOfARecordingMayHaveGivenIsRefusedWhereItsServesDiffer(t *testing.T) {
	const path = "bind.$8"
	// "6c7d" stands in chanceSrcLive alone: neither the recording
	// (chanceSrcRec) nor other holds it.
	const other = "11112222-3333-4444-5555-666677778888"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const (
		inView = "as record's full round gave it in one of "
		unread = "where keploy could not read what it gave the application"
	)
	type classify func(dc *DriftCache, raw []Raw) []Learned
	for _, cl := range []struct {
		name string
		run  classify
	}{
		{"ClassifyGiven", func(dc *DriftCache, raw []Raw) []Learned {
			return ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
		}},
		{"ClassifyFollowing", func(dc *DriftCache, raw []Raw) []Learned {
			return ClassifyFollowing(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
		}},
	} {
		noView := cl.run(noSet(clockedDrifts()), jobsServed(chanceSrcRec, "5", Raw{}))
		if noView[0].Kind == KindUnaccounted {
			t.Fatalf("%s: with no view the value is refused (%q): the row pins nothing", cl.name, noView[0].Why)
		}
		for _, c := range []struct {
			name  string
			views []*models.Mock
			want  string // the refusal's words; "" learns as with no view
		}{
			{"the first of two views holds the word", []*models.Mock{jobsMock(chanceSrcLive, "5"), jobsMock(chanceSrcRec, "5")}, inView + "2 serves"},
			{"the second of two views holds the word", []*models.Mock{jobsMock(other, "5"), jobsMock(chanceSrcLive, "5")}, inView + "2 serves"},
			{"the last of three views holds the word", []*models.Mock{jobsMock(other, "5"), jobsMock(chanceSrcRec, "6"), jobsMock(chanceSrcLive, "7")}, inView + "3 serves"},
			{"no view holds the word", []*models.Mock{jobsMock(other, "5"), jobsMock(chanceSrcRec, "6")}, ""},
			{"a serve's view could not be read", nil, unread},
			{"a view of another recording", []*models.Mock{jobsMock(chanceSrcLive, "5"), {Name: "mock-7", Kind: models.PostgresV3, Spec: jobsMock(other, "5").Spec}}, unread},
		} {
			for _, lazy := range []bool{false, true} {
				for _, tracked := range []bool{false, true} {
					dc := clockedDrifts()
					var tr *Tracker
					if tracked {
						tr = NewTracker()
						tr.NewSet()
						tr.Begin()
						dc = tr.Drifts()
						dc.appClock = true
					}
					src := jobsMock(chanceSrcRec, "5")
					x := RawOf(src, true)
					if lazy {
						x = dc.LazyRawOf(src, true, nil)
					}
					// The recording is read first, as F serves it before its
					// views are kept.
					_ = x.payload()
					views := make([]Raw, 0, len(c.views))
					for _, m := range c.views {
						v := RawOf(m, true)
						if lazy {
							v = dc.LazyRawOf(m, true, nil)
						}
						views = append(views, v)
					}
					got := cl.run(dc, append(eStartup(), x.ServedAs(6).ServedChanged(views...)))
					if tr != nil {
						tr.End()
					}
					at := cl.name + ", " + c.name
					if lazy {
						at += ", lazy"
					}
					if tracked {
						at += ", in a tracker's test"
					}
					switch {
					case c.want == "" && !reflect.DeepEqual(got, noView):
						t.Errorf("%s: learned %s (%q), want as with no view: %s (%q)", at, got[0].Kind, got[0].Why, noView[0].Kind, noView[0].Why)
					case c.want != "" && (got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, c.want)):
						t.Errorf("FALSE GREEN? %s: learned %s (%q), want refused: %q", at, got[0].Kind, got[0].Why, c.want)
					}
				}
			}
		}
	}
}

// A Raw F did not serve says nothing of how F served it, and one handed
// back as not served (WithServed(false)) drops what ServedChanged said.
func TestServedChangedSaysNothingOfARecordingFDidNotServe(t *testing.T) {
	views := []Raw{RawOf(jobsMock(chanceSrcLive, "5"), true)}
	if x := RawOf(jobsMock(chanceSrcRec, "5"), false).ServedChanged(views...); x.unseen || x.views != nil {
		t.Errorf("a recording F did not serve reads as served changed: unseen %v, %d views", x.unseen, len(x.views))
	}
	x := RawOf(jobsMock(chanceSrcRec, "5"), true).ServedAs(6).ServedChanged(views...)
	if !x.unseen || len(x.views) != 1 || x.Live() || x.AsRecorded() {
		t.Fatalf("served changed: unseen %v, %d views, live %v, as recorded %v", x.unseen, len(x.views), x.Live(), x.AsRecorded())
	}
	if y := x.WithServed(false); y.unseen || y.views != nil {
		t.Errorf("WithServed(false) kept what ServedChanged said: unseen %v, %d views", y.unseen, len(y.views))
	}
	if y := x.ServedLive(views[0]); y.unseen || y.views != nil || !y.Live() {
		t.Errorf("ServedLive after ServedChanged: unseen %v, %d views, live %v", y.unseen, len(y.views), y.Live())
	}
}

// A view keploy could not read back from its recording (a lazy Raw whose
// loader fails) is no view of what record's full round served there
// (Raw.viewOf). Read as a view of nothing, it would hold none of the words
// the serve gave, so beside the other serves of a recording served in
// differing ways (ServedChanged) a word only it gave would stand in none of
// the views and pass as noise: a value the application copied from that
// serve, unchecked at replay (FALSE GREEN). With ServedChanged, as the only
// view or beside views that were read, every word the value holds now is
// refused as one keploy could not read; with ServedLive the value is
// learned as with no view. Under ClassifyGiven and ClassifyFollowing, in a
// tracker's test and with a cache of no tracker.
func TestAViewThatCouldNotBeReadIsNoView(t *testing.T) {
	const path = "bind.$8"
	// "6c7d" stands in chanceSrcLive alone: neither the recording
	// (chanceSrcRec) nor other holds it.
	const other = "11112222-3333-4444-5555-666677778888"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	const unread = "where keploy could not read what it gave the application"
	gone := func() (*models.Mock, error) { return nil, errors.New("the parked answer is gone") }
	for _, cl := range []struct {
		name string
		run  func(dc *DriftCache, raw []Raw) []Learned
	}{
		{"ClassifyGiven", func(dc *DriftCache, raw []Raw) []Learned {
			return ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
		}},
		{"ClassifyFollowing", func(dc *DriftCache, raw []Raw) []Learned {
			return ClassifyFollowing(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
		}},
	} {
		noView := cl.run(noSet(clockedDrifts()), jobsServed(chanceSrcRec, "5", Raw{}))
		if noView[0].Kind == KindUnaccounted {
			t.Fatalf("%s: with no view the value is refused (%q): the row pins nothing", cl.name, noView[0].Why)
		}
		for _, c := range []struct {
			name  string
			serve func(dc *DriftCache, x Raw) Raw
			want  string // the refusal's words; "" learns as with no view
		}{
			{"the one view (ServedLive)", func(dc *DriftCache, x Raw) Raw {
				return x.ServedLive(dc.LazyRawOf(jobsMock(chanceSrcLive, "5"), true, gone))
			}, ""},
			{"the only view of differing serves", func(dc *DriftCache, x Raw) Raw {
				return x.ServedChanged(dc.LazyRawOf(jobsMock(chanceSrcLive, "5"), true, gone))
			}, unread},
			{"after a view that was read and holds no word of the value", func(dc *DriftCache, x Raw) Raw {
				return x.ServedChanged(dc.LazyRawOf(jobsMock(other, "5"), true, nil), dc.LazyRawOf(jobsMock(chanceSrcLive, "6"), true, gone))
			}, unread},
			{"before a view that was read and holds no word of the value", func(dc *DriftCache, x Raw) Raw {
				return x.ServedChanged(dc.LazyRawOf(jobsMock(chanceSrcLive, "5"), true, gone), dc.LazyRawOf(jobsMock(other, "6"), true, nil))
			}, unread},
		} {
			for _, tracked := range []bool{false, true} {
				dc := clockedDrifts()
				var tr *Tracker
				if tracked {
					tr = NewTracker()
					tr.NewSet()
					tr.Begin()
					dc = tr.Drifts()
					dc.appClock = true
				}
				src := jobsMock(chanceSrcRec, "5")
				x := dc.LazyRawOf(src, true, nil)
				_ = x.payload()
				got := cl.run(dc, append(eStartup(), c.serve(dc, x.ServedAs(6))))
				if tr != nil {
					tr.End()
				}
				at := cl.name + ", " + c.name
				if tracked {
					at += ", in a tracker's test"
				}
				switch {
				case c.want == "" && !reflect.DeepEqual(got, noView):
					t.Errorf("FALSE GREEN? %s: an unread view learned %s (%q), want as with no view: %s (%q)", at, got[0].Kind, got[0].Why, noView[0].Kind, noView[0].Why)
				case c.want != "" && (got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, c.want)):
					t.Errorf("FALSE GREEN? %s: an unread view learned %s (%q), want refused: %q", at, got[0].Kind, got[0].Why, c.want)
				}
			}
		}
	}
}
