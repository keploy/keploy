package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// asideDb is gateDb whose open test is offered recordings of tests the
// replay set aside (integrations.SetAsideReader). It holds each call to its
// round as itself, so the matcher and the miss read it.
type asideDb struct {
	gateDb
	aside []*models.Mock
}

func (d asideDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d asideDb) RangeSetAside(kind models.Kind, _ []string, visit func([]*models.Mock) bool) {
	rangeAside(d.aside, kind, visit)
}

// rangeAside offers aside to visit as one handed test's batch, of kind.
func rangeAside(aside []*models.Mock, kind models.Kind, visit func([]*models.Mock) bool) {
	var out []*models.Mock
	for _, mk := range aside {
		if kind == "" || mk.Kind == kind {
			out = append(out, mk)
		}
	}
	if len(out) > 0 {
		visit(out)
	}
}

// A call one more than the open test recorded, which a set-aside test's
// recording is exactly, is answered with that recording, unreported (the
// store names it on the test's evidence): the set recorded the call once
// more. A set-aside recording of another call answers none: the call stays
// the test's extra call. With the switch off nothing is served aside, as on
// main.
func TestACallBeyondTheTestsCountIsServedAnExactSetAsideCopy(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/v1/secret/pat"
	raw := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\n\r\n"
	answer := func(m *models.Mock, body string) *models.Mock {
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: body, Header: map[string]string{"Content-Length": "3"}}
		return m
	}
	own := answer(onHost("mock-own", path, host), "own")
	store := func(aside *models.Mock) asideDb {
		return asideDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{updateUnFilteredReturn: true, deleteFilteredReturn: true}}, consumed: []*models.Mock{own}}, aside: []*models.Mock{aside}}
	}
	copyOf := answer(onHost("mock-aside", path, host), "set")
	other := answer(onHost("mock-aside", path+"/v2", host), "set")

	t.Setenv(provenswitch.Env, "1")
	status, body, r := exchangeOf(t, store(copyOf), addr, raw)
	if status != 200 || body != "set" || r != nil {
		t.Fatalf("a call beyond the test's count with an exact set-aside copy got %d %q, reported %+v; want the copy's answer, unreported", status, body, r)
	}
	status, _, r = answerOf(t, store(other), addr, raw)
	if status != 502 || r.MissKind != models.MissExtraCall || r.Recorded != 1 || r.ClosestMock != "mock-own" {
		t.Fatalf("a call beyond the test's count with no exact set-aside copy got %d, reported %+v; want the extra call repeating mock-own", status, r)
	}

	t.Setenv(provenswitch.Env, "0")
	status, body, _ = exchangeOf(t, store(copyOf), addr, raw)
	if status != 502 || body == "set" {
		t.Fatalf("with the switch off a call was served %d %q; want the miss main makes, nothing served aside", status, body)
	}
}

// asideMemDb is a store that shows aside as the set-aside recordings
// (integrations.SetAsideReader) beside mockMemDb's reads.
type asideMemDb struct {
	*mockMemDb
	aside []*models.Mock
}

func (d *asideMemDb) RangeSetAside(kind models.Kind, _ []string, visit func([]*models.Mock) bool) {
	rangeAside(d.aside, kind, visit)
}

// A set-aside test's recording whose request holds a read of the
// application's clock the learn step learned (MockSpec.Clocks) answers a
// later test's call only when the call carries a read of the clock made in
// that call's own window (since the previous serve on its connection), the
// check its own test's call gets (clockDiffs), never unchecked. A value
// moved thirty days, the epoch, the recorded value itself, a read made
// before the later call's window, and every value where keploy cannot tell
// the application's clock are not that recording: the call stays a miss.
func TestASetAsideRecordingsClockReadIsCheckedInTheLaterCallsWindow(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	recAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }
	now := base.Add(time.Second)
	aside := func() *models.Mock {
		m := recorded("pat-fill", "/v1/pat", map[string]string{"Content-Type": "application/json"}, `{"q":"x","since":"`+stamp(recAt)+`"}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"body.since": {Res: time.Second}}
		return m
	}
	call := func(since string) *req {
		r := sent("/v1/pat", map[string]string{"Content-Type": "application/json"}, `{"q":"x","since":"`+since+`"}`)
		r.arrived = base.Add(2 * time.Second)
		return r
	}
	for _, c := range []struct {
		name     string
		since    string
		appClock bool
		served   bool
	}{
		{"a read of the clock in the later call's window", stamp(now), true, true},
		{"moved thirty days", stamp(now.AddDate(0, 0, -30)), true, false},
		{"the epoch", "1970-01-01T00:00:00Z", true, false},
		{"the recorded value", stamp(recAt), true, false},
		{"a read made before the later call's window", stamp(base.Add(-10 * time.Second)), true, false},
		{"keploy cannot tell the app's clock", stamp(now), false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &asideMemDb{mockMemDb: &mockMemDb{updateUnFilteredReturn: true}, aside: []*models.Mock{aside()}}
			got, err := newHTTP().setAsideAnswer(clockConn(c.appClock, base), call(c.since), db, models.ProvenFlakyHeaderNoise(), nil, nil, true, false)
			if err != nil {
				t.Fatalf("setAsideAnswer: %v", err)
			}
			if c.served {
				if got == nil || got.Name != "pat-fill" {
					t.Fatalf("served %v; want the set-aside recording, its clock read made in the call's window", got)
				}
				if db.updateUnFilteredMockOld == nil || db.updateUnFilteredMockOld.Name != "pat-fill" {
					t.Fatal("served without its claim through the store's consume door")
				}
				return
			}
			if got != nil {
				t.Fatalf("served %s; want the call left a miss: its clock read is checked as in its own test", got.Name)
			}
			if db.updateUnFilteredMockOld != nil {
				t.Fatalf("claimed %s for a call it does not answer", db.updateUnFilteredMockOld.Name)
			}
		})
	}
}
