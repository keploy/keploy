package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A consumer that fell behind makes a call of an older test in a later
// test's window: it carries the id a serve before the call's test gave it
// (mock-22), while the call's recording holds the id of a recording of the
// window's own test (mock-60), which F gave the application as recorded.
// Mongo, under the rule a copy follows its source, learned such a value as
// noise where mock-22 was the previous test's serve. HTTP reads the call
// without that rule (learn.ClassifyGiven), so mock-60 stays a place of the
// value: whether mock-22 was served in the previous test, two tests back,
// or between tests, the call is refused and nothing is learned (no noise,
// no variable). The reason's wording is not pinned here.
func TestALateCallsValueAServeBeforeItsTestGaveIsNeverLearned(t *testing.T) {
	const (
		path   = "body.session.id"
		recID  = "ses-7Q4MZ2KD9XWA"
		liveID = "ses-5H8RT3NC6PLB"
	)
	at := time.Date(2026, 11, 3, 9, 41, 12, 417000000, time.UTC)
	answer := func(name string, before time.Duration, id string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			ReqTimestampMock: at.Add(-before),
			ResTimestampMock: at.Add(-before + time.Millisecond),
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api.local/session"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"session":{"id":"` + id + `"}}`},
		}}
	}
	for _, c := range []struct {
		name string
		// back is how many tests before the call's mock-22 was served;
		// between, served once its test ended, outside any test.
		back    int
		between bool
	}{
		{"the previous test's serve", 1, false},
		{"a serve two tests back", 2, false},
		{"a serve between tests", 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			older := answer("mock-22", 40*time.Second, liveID)
			tr := learn.NewTracker()
			tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real, true })
			tr.NewSet()
			tr.Begin()
			if !c.between {
				tr.ServedMock(older)
				tr.ServedMockAsRecorded(older)
			}
			tr.End()
			if c.between {
				tr.ServedMock(older)
				tr.ServedMockAsRecorded(older)
			}
			for i := 1; i < c.back; i++ {
				tr.Begin()
				tr.End()
			}
			tr.Begin()
			o := tr.Placed().Order
			// A serve since the previous test began is handed to the call
			// as F served it (Order.ServedView); an older one as the word
			// rule reads another test's recording (GivenPlaces).
			var raw []learn.Raw
			given := func() ([]learn.GivenPlaces, error) { return nil, nil }
			if c.back == 1 {
				raw = append(raw, o.ServedView(older, learn.RawOf(older, true).ServedIn(o.Of(older))))
			} else {
				given = func() ([]learn.GivenPlaces, error) {
					g, err := o.GivenPlaces(older)
					return []learn.GivenPlaces{g}, err
				}
			}
			window := learn.RawOf(answer("mock-60", 200*time.Millisecond, recID), false).ServedAs(9).ServedAsRecorded()
			rec := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: recID}}}
			live := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: liveID}}}
			learned := learn.ClassifyGiven(tr.Drifts(), []string{path}, live, rec, at, nil, append(raw, window), nil, nil, given)
			p, why := learnPlan(tr, uint64(c.back+1), &models.Mock{Name: "mock-61"}, rec, true, learned, nil, nil)
			if why == "" || len(p.noise) != 0 || len(p.vars) != 0 {
				t.Fatalf("why %q, noise %v, variables %v; want the call refused, nothing learned", why, p.noise, p.vars)
			}
		})
	}
}
