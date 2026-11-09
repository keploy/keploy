package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The proxy gives its learn step the requests a client test sent in its
// recording, asks the ingress for the test's requests, and gives it each one
// that came, under the proven switch alone: with it off the learn step reads
// no client test's request, as before it could.
func TestTheProxyGivesItsLearnStepAClientTestsRequestsUnderTheSwitchOnly(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	cases := []models.ClientCase{{Name: "post-apps-1", Req: models.HTTPReq{Method: "POST", URL: "http://api/apps", Body: `{"name":"app-1"}`, Timestamp: at.Add(-time.Hour)}}}
	live := &models.HTTPReq{Method: "POST", URL: "http://api/apps", Body: `{"name":"app-2"}`}
	direct := []learn.ClientCase{{Name: "post-apps-1", Req: &cases[0].Req}}

	t.Setenv(provenswitch.Env, "")
	p := &Proxy{learnTracker: learn.NewTracker()}
	p.learnTracker.Begin()
	p.TakeClientCases("test-1", cases)
	if p.learnTracker.WantsIncoming() {
		t.Fatal("the switch off: the learn step was given the test's requests")
	}
	p.learnTracker.SetCases(direct)
	if p.WantsIncoming() {
		t.Fatal("the switch off: the proxy asks the ingress for requests")
	}
	p.TakeIncoming(live, "", at)
	if n := len(p.learnTracker.Placed().Earlier); n != 0 {
		t.Fatalf("the switch off: the learn step holds %d places of the test's requests", n)
	}

	t.Setenv(provenswitch.Env, "1")
	p = &Proxy{learnTracker: learn.NewTracker()}
	p.learnTracker.Begin()
	p.TakeClientCases("test-1", cases)
	if !p.learnTracker.WantsIncoming() || !p.WantsIncoming() {
		t.Fatal("the switch on: the learn step was not given the test's requests, or the proxy asks for none")
	}
	p.TakeIncoming(live, "", at)
	if got := p.learnTracker.Placed().Earlier; len(got) != 1 || got[0].Mock != "test case post-apps-1" {
		t.Fatalf("the switch on: the learn step holds %+v", got)
	}
	p.learnTracker.Begin()
	p.TakeClientCases("test-1", nil)
	if p.WantsIncoming() {
		t.Fatal("no requests given: the proxy asks for some")
	}
}
