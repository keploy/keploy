package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Round F's word standing whole in a set recording's own recorded text,
// where round F served that recording only after the call reached keploy,
// or in a request still aloft then, is refused as unknown on the
// served-after line, short or long, however the serve was told: with a
// view holding neither word, with two such views, told of nothing, or with
// a view keploy could not keep. The text was written in the recording run,
// and a Raw place whose view Raw.asOfArrival drops is refused the same way.
// Served before the call with views holding neither word, the word is fresh
// (round F gave the application its views, not that text); told of nothing
// or with a view keploy could not keep, it is refused.
func TestOutcomeRoundFsWordInASetRecordingsOwnAnswerServedAfterTheCallHoweverToldIsRefused(t *testing.T) {
	set := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	view := func(key, held string) func(tr *Tracker, m *models.Mock) {
		return func(tr *Tracker, m *models.Mock) {
			v := set(held)
			tr.ServedMockLive(m, key, func() (*models.Mock, error) { return v, nil })
		}
	}
	for _, c := range []struct{ what, rec, live string }{
		{"a short word", "7f3a9c", "2b8e41"},
		{"a word of 32 bits or more", "7f3a9c5d1e", "2b8e41a0c7"},
	} {
		for _, s := range []struct {
			what        string
			serves      []func(tr *Tracker, m *models.Mock)
			freshBefore bool
		}{
			{"with one view holding neither word", []func(*Tracker, *models.Mock){view("v1", "0b0b0b0b0b")}, true},
			{"with two views holding neither word", []func(*Tracker, *models.Mock){view("v1", "0b0b0b0b0b"), view("v2", "0c0c0c0c0c")}, true},
			{"told of nothing", []func(*Tracker, *models.Mock){func(*Tracker, *models.Mock) {}}, false},
			{"with a view keploy could not keep", []func(*Tracker, *models.Mock){func(tr *Tracker, m *models.Mock) { tr.ServedMockLive(m, "", nil) }}, false},
		} {
			for _, w := range []serveWhen{servedFirst, servedLater, servedAloft} {
				what := c.what + " of round F's in the recording's own text, served " + s.what + " " + w.String()
				m := set(c.live)
				tr := NewTracker()
				tr.Begin()
				var arrival time.Time
				arrive := func() {
					time.Sleep(2 * time.Millisecond)
					arrival = time.Now()
					time.Sleep(2 * time.Millisecond)
				}
				if w == servedLater {
					arrive()
				}
				for _, serve := range s.serves {
					if w == servedAloft {
						tr.ServedMockIn(m, tr.Lift())
					} else {
						tr.ServedMock(m)
					}
					serve(tr, m)
				}
				if w != servedLater {
					arrive()
				}
				g, err := tr.Placed().Order.ArrivedAt(arrival).GivenPlaces(m)
				if err != nil {
					t.Fatal(err)
				}
				in := oInput("body.ref", c.rec, c.live, nil, nil)
				in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
				d := oWord(t, Decide(in), c.rec, c.live)
				switch {
				case w != servedFirst:
					oServedAfterRefused(t, what, d)
				case s.freshBefore:
					costFresh(t, what, d)
				default:
					oRefused(t, what, d, RefusalUnknown, "mock-31")
				}
			}
		}
	}
}
