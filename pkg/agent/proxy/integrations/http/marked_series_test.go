package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// seriesAsked is what a registered SeriesAnswerer was handed: the call as a
// recording holds its request, when keploy had it, and the parser's known.
type seriesAsked struct {
	live    *models.Mock
	arrived time.Time
	known   learn.Known
}

// registerSeries makes answer the HTTP SeriesAnswerer for t, recording what
// each call hands it, and returns those asks.
func registerSeries(t *testing.T, answer func(live *models.Mock) (*models.Mock, *integrations.SeriesMiss)) *[]seriesAsked {
	t.Helper()
	var asked []seriesAsked
	t.Cleanup(integrations.RegisterSeriesAnswerer(models.HTTP, func(_ context.Context, _ integrations.MockMemDb, live *models.Mock, arrived time.Time, known learn.Known) (*models.Mock, *integrations.SeriesMiss, error) {
		asked = append(asked, seriesAsked{live: live, arrived: arrived, known: known})
		served, miss := answer(live)
		return served, miss, nil
	}))
	return &asked
}

// A timer's GET that judging marked as a series set-wide comes in a test's
// window with its read of the clock moved, so exact serving answers none of
// it. The parser hands the call, as a recording holds its request, to what
// the build registered for HTTP (integrations.AnswerSeries): the recording it
// answers with is served, reported as a marked series' answer naming it,
// counting against no test. A series miss is the set's background miss,
// named as the series' newest recording, never one call more than the test
// recorded although the test answered a recording the call repeats. With
// none registered (keploy itself) and with the switch off the call misses as
// it did before.
func TestAMarkedSeriesCallIsAnsweredOrMissedAsTheBuildRegisters(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/internal/claims/next"
	raw := "GET " + path + "?since=1793000470 HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\nDate: Wed, 30 Oct 2026 01:01:10 GMT\r\n\r\n"
	poll := onHost("mock-poll-9", path, host)
	poll.Spec.HTTPReq.URL = path + "?since=1793000410"
	poll.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "none-claimed", Header: map[string]string{"Content-Length": "12"}}
	// The test answered a recording of the very call earlier in its window:
	// the call repeats it, which a series' miss is never charged as.
	own := onHost("mock-own", path, host)
	own.Spec.HTTPReq.URL = path + "?since=1793000470"
	own.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "own", Header: map[string]string{"Content-Length": "3"}}
	store := againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{own}}}
	t.Setenv(provenswitch.Env, "1")

	asked := registerSeries(t, func(*models.Mock) (*models.Mock, *integrations.SeriesMiss) { return poll, nil })
	before := time.Now()
	status, body, r := answerOf(t, store, addr, raw)
	if status != 200 || body != "none-claimed" || r.Answer != models.AnswerBackgroundSeries || r.ClosestMock != "mock-poll-9" || r.CountsAgainstTest() {
		t.Fatalf("a marked series' call got %d %q, reported %+v; want mock-poll-9's answer, reported as a marked series' answer naming it", status, body, r)
	}
	if len(*asked) != 1 {
		t.Fatalf("the answerer was asked %d times; want once", len(*asked))
	}
	a := (*asked)[0]
	req := a.live.Spec.HTTPReq
	if a.live.Kind != models.HTTP || req == nil || req.Method != "GET" || req.URL != path+"?since=1793000470" || req.Header["Host"] != host || a.live.Spec.HTTPResp != nil {
		t.Fatalf("the answerer was handed %+v (request %+v); want the call's GET, its URL as it came and its Host, no answer", a.live, req)
	}
	if a.arrived.Before(before) {
		t.Fatalf("the answerer was handed arrived %v, before the call was sent (%v)", a.arrived, before)
	}
	if excused, _ := a.known.Excused(learn.Leaf{Path: "header.date", Noise: "header.date", Type: learn.String, Value: "x"}); !excused {
		t.Fatal("the answerer's known does not excuse the static header noise the parser's learn step excuses")
	}

	newest := onHost("mock-poll-12", path, host)
	registerSeries(t, func(*models.Mock) (*models.Mock, *integrations.SeriesMiss) {
		return nil, &integrations.SeriesMiss{NextSteps: "its reads of the clock do not hold on this run's clock", Newest: newest}
	})
	status, _, r = answerOf(t, store, addr, raw)
	if status != 502 || !r.Background || r.ClosestMock != "mock-poll-12" || r.MissKind == models.MissExtraCall || r.NextSteps != "its reads of the clock do not hold on this run's clock" {
		t.Fatalf("an off-clock marked series' call got %d, reported %+v; want the set's background miss naming mock-poll-12, never an extra call", status, r)
	}

	// A call of no marked series goes on as before: one call more than the
	// test recorded.
	registerSeries(t, func(*models.Mock) (*models.Mock, *integrations.SeriesMiss) { return nil, nil })
	status, _, r = answerOf(t, store, addr, raw)
	if status != 502 || r.Background || r.MissKind != models.MissExtraCall {
		t.Fatalf("a call of no marked series got %d, reported %+v; want the extra-call miss", status, r)
	}

	t.Setenv(provenswitch.Env, "0")
	asked = registerSeries(t, func(*models.Mock) (*models.Mock, *integrations.SeriesMiss) { return poll, nil })
	status, _, r = answerOf(t, store, addr, raw)
	if status != 502 || r.Answer != "" || r.Background || len(*asked) != 0 {
		t.Fatalf("with the switch off a marked series' call got %d, reported %+v, asked %d times; want the miss main makes, never asked", status, r, len(*asked))
	}
}

// With no SeriesAnswerer registered for HTTP (keploy itself registers none)
// a call goes on exactly as before: one call more than the test recorded.
func TestACallWithNoSeriesAnswererMissesAsBefore(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/internal/claims/next"
	raw := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\n\r\n"
	poll := onHost("mock-poll-9", path, host)
	poll.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "none-claimed", Header: map[string]string{"Content-Length": "12"}}
	store := againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{poll}}}
	t.Setenv(provenswitch.Env, "1")
	if served, miss, err := integrations.AnswerSeries(context.Background(), store, seriesCall(&req{method: "GET"}), time.Now(), nil); served != nil || miss != nil || err != nil {
		t.Fatalf("AnswerSeries with none registered = %v, %v, %v; want all nil", served, miss, err)
	}
	status, _, r := answerOf(t, store, addr, raw)
	if status != 502 || r.Background || r.MissKind != models.MissExtraCall {
		t.Fatalf("with no series answerer the call got %d, reported %+v; want the extra-call miss", status, r)
	}
}
