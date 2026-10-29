package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
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

// viewDb is againDb whose open view shows own, the open test's recordings,
// none of them consumed, read in the round the call first read.
type viewDb struct {
	againDb
	own []*models.Mock
}

func (d viewDb) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return d.own, nil }
func (d viewDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return d.own, nil }
func (d viewDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

// A marked series' call none of its recordings answered is the set's
// background miss and nothing else, whatever else the call is: a recording
// made elsewhere (another test's, which judging would move to the worker
// scope), a call an earlier test made whose recording already answered (a
// repeat note), or one whose array came in another order than a recording
// of the open test's (unstable_order). Each of those explains a test's own
// miss; the series' miss is charged to no test, names the series' newest
// recording and says why the series answered none. Each case first shows,
// with the call of no marked series, that the store does explain the call
// so, so the row reads what the series' miss leaves out.
func TestAMarkedSeriesMissIsTheSetsBackgroundMissAndNothingElse(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/internal/claims/next"
	const why = "its reads of the clock do not hold on this run's clock"
	get := "GET " + path + "?since=1793000470 HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\n\r\n"
	copyOfGet := func(name string) *models.Mock {
		m := onHost(name, path, host)
		m.Spec.HTTPReq.URL = path + "?since=1793000470"
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "none-claimed", Header: map[string]string{"Content-Length": "12"}}
		return m
	}
	postBody := `{"ids":[3,1,2]}`
	post := "POST " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(postBody)) + "\r\n\r\n" + postBody
	inOrder := onHost("mock-own-post", path, host)
	inOrder.Spec.HTTPReq.Method = "POST"
	inOrder.Spec.HTTPReq.Header["Content-Type"] = "application/json"
	inOrder.Spec.HTTPReq.Body = `{"ids":[1,2,3]}`
	inOrder.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "{}", Header: map[string]string{"Content-Length": "2"}}
	newest := onHost("mock-poll-12", path, host)
	t.Setenv(provenswitch.Env, "1")

	for _, tc := range []struct {
		name    string
		store   integrations.MockMemDb
		raw     string
		explain func(*models.MockMismatchReport) bool
	}{
		{
			"a recording made elsewhere",
			againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, staged: []*models.Mock{copyOfGet("mock-other-test")}}},
			get,
			func(r *models.MockMismatchReport) bool { return r.NotOwned && r.ClosestMock == "mock-other-test" },
		},
		{
			"an earlier test's answered call it repeats",
			againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}, answered: []integrations.AnsweredCall{{Mock: copyOfGet("mock-earlier"), AnsweredAt: time.Now().Add(-time.Minute)}}}}},
			get,
			func(r *models.MockMismatchReport) bool { return r.Repeat != nil && r.ClosestMock == "mock-earlier" },
		},
		{
			"an array in another order than the open test's recording",
			viewDb{againDb: againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}}}, own: []*models.Mock{inOrder}},
			post,
			func(r *models.MockMismatchReport) bool { return r.Reason == models.MissUnstableOrder },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registerSeries(t, func(*models.Mock) (*models.Mock, *integrations.SeriesMiss) { return nil, nil })
			if status, _, r := answerOf(t, tc.store, addr, tc.raw); status != 502 || r.Background || !tc.explain(r) {
				t.Fatalf("a call of no marked series got %d, reported %+v; want the miss the store explains", status, r)
			}
			registerSeries(t, func(*models.Mock) (*models.Mock, *integrations.SeriesMiss) {
				return nil, &integrations.SeriesMiss{NextSteps: why, Newest: newest}
			})
			status, _, r := answerOf(t, tc.store, addr, tc.raw)
			if status != 502 || !r.Background || r.ClosestMock != "mock-poll-12" || r.NextSteps != why || r.NotOwned || r.Repeat != nil || r.Reason != "" || r.MissKind == models.MissExtraCall {
				t.Fatalf("a marked series' call nothing answered got %d, reported %+v; want the set's background miss naming mock-poll-12 and saying %q, and nothing else", status, r, why)
			}
			// What the report shows beside the name is of the recording it
			// names, never of another the open view held (the replay prints
			// ClosestMockReq against the call, under ClosestMock's name).
			if r.ClosestMockReq != "" || r.Diff != "" || len(r.FieldDiffs) != 0 || len(r.Differs) != 0 || len(r.FieldReasons) != 0 {
				t.Fatalf("the series' miss names mock-poll-12 but shows another recording's request %q, diff %q, fields %v %v %v", r.ClosestMockReq, r.Diff, r.FieldDiffs, r.Differs, r.FieldReasons)
			}
		})
	}
}

// A SeriesAnswerer that fails (its read of the store did) fails the call as
// a matcher's error does: the parser returns that error, the app gets no
// answer, and nothing is reported. The call goes on to no later step, so it
// is neither answered with a set-aside copy nor charged to the open test as
// its miss, which a background call keploy could not read never is.
func TestASeriesAnswerersErrorIsTheCallsError(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/internal/claims/next"
	raw := "GET " + path + "?since=1793000470 HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\n\r\n"
	store := againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}}}
	t.Setenv(provenswitch.Env, "1")
	failed := errors.New("the store's read of the set's series failed")
	t.Cleanup(integrations.RegisterSeriesAnswerer(models.HTTP, func(context.Context, integrations.MockMemDb, *models.Mock, time.Time, learn.Known) (*models.Mock, *integrations.SeriesMiss, error) {
		return nil, nil, failed
	}))

	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	_ = app.SetDeadline(time.Now().Add(10 * time.Second))
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, store, models.OutgoingOptions{})
	}()
	if _, err := io.WriteString(app, raw); err != nil {
		t.Fatal(err)
	}
	// The app's read and the parser's end are awaited together: an answer
	// the parser writes is read at once (the pipe holds none), and once the
	// parser returned its connection ends, which ends the app's read.
	answered := make(chan int, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	select {
	case status := <-answered:
		t.Fatalf("the app's read ended (status %d) before the parser returned; want no answer and the answerer's error", status)
	case err := <-done:
		if !errors.Is(err, failed) {
			t.Fatalf("the parser returned %v; want the answerer's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return")
	}
	_ = prox.Close()
	if status := <-answered; status != 0 {
		t.Fatalf("the app got %d for a call whose series answerer failed; want no answer", status)
	}
	if reported != nil {
		t.Fatalf("the call was reported as %v; want nothing reported", reported)
	}
}

// heldMarkDb is the store a call of holdingDb is held to (HoldToRound):
// another store than the parser's, whose reads are the call's.
type heldMarkDb struct{ againDb }

// holdingDb is againDb that holds each call to its round as held.
type holdingDb struct {
	againDb
	held *heldMarkDb
}

func (d holdingDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d.held, func() bool { return false }
}

// The answerer of a marked series' call is handed the store held to the
// call's first read (integrations.HoldToRound), never the parser's own: the
// held store hands the series of the staging the call was made in, so a
// call is never answered from the series of a set the replay staged after
// it (integrations.MarkedSeries.Restaged).
func TestASeriesAnswererIsHandedTheStoreHeldToTheCall(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/internal/claims/next"
	raw := "GET " + path + "?since=1793000470 HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\n\r\n"
	poll := onHost("mock-poll-9", path, host)
	poll.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "none-claimed", Header: map[string]string{"Content-Length": "12"}}
	base := againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}}}
	store := holdingDb{againDb: base, held: &heldMarkDb{againDb: base}}
	t.Setenv(provenswitch.Env, "1")
	var handed []integrations.MockMemDb
	t.Cleanup(integrations.RegisterSeriesAnswerer(models.HTTP, func(_ context.Context, db integrations.MockMemDb, _ *models.Mock, _ time.Time, _ learn.Known) (*models.Mock, *integrations.SeriesMiss, error) {
		handed = append(handed, db)
		return poll, nil, nil
	}))
	if status, _, r := answerOf(t, store, addr, raw); status != 200 || r.Answer != models.AnswerBackgroundSeries {
		t.Fatalf("a marked series' call got %d, reported %+v; want the series' answer", status, r)
	}
	if len(handed) != 1 || handed[0] != integrations.MockMemDb(store.held) {
		t.Fatalf("the answerer was handed %d stores (%T); want the one store held to the call's first read", len(handed), handed)
	}
}
