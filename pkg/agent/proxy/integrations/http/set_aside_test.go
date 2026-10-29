package http

import (
	"testing"

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

func (d asideDb) SetAsideMocks(kind models.Kind) []*models.Mock {
	var out []*models.Mock
	for _, mk := range d.aside {
		if kind == "" || mk.Kind == kind {
			out = append(out, mk)
		}
	}
	return out
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
