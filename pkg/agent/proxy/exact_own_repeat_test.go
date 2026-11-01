package proxy

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
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// ownRepeatExchange is a recorded opaque TCP exchange of req answered with
// resp, made at at.
func ownRepeatExchange(name, req, resp string, at time.Time) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
		GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: req}}}},
		GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: models.String, Data: resp}}}},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// genericCall sends req on a new connection through the generic parser,
// served from mm, and returns the four bytes it was answered with, or the
// miss's report when it was answered nothing.
func genericCall(t *testing.T, mm integrations.MockMemDb, req string) (string, *models.MockMismatchReport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	defer cancel()
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
	app, agentEnd := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		err := generic.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
		done <- err
	}()
	if _, err := io.WriteString(app, req); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err == nil {
		_ = app.Close()
		<-done
		return string(got), nil
	}
	err := <-done
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("%q was answered nothing, and its miss has no report: %v", req, err)
	}
	return "", r.MismatchReport()
}

// Under the proven switch, on a set served by the background rules, an
// exchange identical to one the open test's own recording already answered
// is one exchange more than the test recorded: the background never answers
// it, though the pool holds a recording of it (one the application made
// between tests). It misses as an extra call, and the pool's recording stays
// the background's: between tests it answers the exchange. Answered by the
// pool's copy, a test that sent a recorded write twice passed.
func TestTheBackgroundNeverAnswersAGenericExchangeOneMoreThanTheTestRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	// other is the pool's recording of an exchange the test never recorded:
	// it answers one.
	other := ownRepeatExchange("mock-pool-info", "INFO", "INF0", now.Add(time.Microsecond))
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool, other}, models.BaseTime, now.Add(time.Hour))
	poolNames := []string{pool.Name, other.Name}
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}

	if got, miss := genericCall(t, mm, "PING"); got != "OWN1" {
		t.Fatalf("test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	got, miss := genericCall(t, mm, "PING")
	if got != "" {
		t.Fatalf("test-0's second PING, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if got, miss := genericCall(t, mm, "INFO"); got != "INF0" {
		t.Fatalf("test-0's INFO, an exchange it never recorded, was answered %q (miss %+v); want the pool's INF0", got, miss)
	}

	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	if got, miss := genericCall(t, mm, "PING"); got != "POOL" {
		t.Fatalf("the PING between tests was answered %q (miss %+v); want the pool's POOL", got, miss)
	}
}

// A connection the proxy wrapped in a scoped view (scopedMockDb) before exact
// serving began keeps that view for its life (scopedFor decides once per
// connection). Its calls keep the held store's own-call decision through the
// view: an exchange identical to one the open test's own recording already
// answered misses as an extra call, recorded once, and the pool's recording
// of it stays unspent. Without the view passing the decision on, the pool
// answered the exchange on such a connection.
func TestAScopedConnectionKeepsTheBackgroundOffAnExchangeOneMoreThanTheTestRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool}, models.BaseTime, now.Add(time.Hour))
	poolNames := []string{pool.Name}
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}
	scoped := &scopedMockDb{MockMemDb: mm}

	if got, miss := genericCall(t, scoped, "PING"); got != "OWN1" {
		t.Fatalf("test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	got, miss := genericCall(t, scoped, "PING")
	if got != "" {
		t.Fatalf("test-0's second PING on a scoped connection, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if mm.MockSpent(pool.Name) {
		t.Fatal("the pool's PING was spent by a call it did not answer")
	}
}

// workerProbe is a recording of GET http://deps<path> made at at, answered
// body, as seqCall records one.
func workerProbe(name string, at time.Duration, path, body string) *models.Mock {
	m := seqCall(name, at, body)
	m.Spec.HTTPReq.URL = "http://deps" + path
	return m
}

// httpMissCall sends GET path through the HTTP parser and returns the body
// it was answered with, or, when it was answered no 200, the miss's report.
func httpMissCall(t *testing.T, mm *MockManager, path string) (string, *models.MockMismatchReport) {
	t.Helper()
	return httpMissSend(t, mm, "GET "+path+" HTTP/1.1\r\nHost: deps\r\n\r\n")
}

// httpMissSend is httpMissCall for the whole request raw.
func httpMissSend(t *testing.T, mm *MockManager, raw string) (string, *models.MockMismatchReport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	defer cancel()
	app, agentEnd := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		err := httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
		done <- err
	}()
	if _, err := io.WriteString(app, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err == nil && resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(resp.ContentLength)))
		_ = app.Close()
		<-done
		return string(body), nil
	}
	_ = app.Close()
	err = <-done
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("%q was answered no 200, and its miss has no report: %v", raw, err)
	}
	return "", r.MismatchReport()
}

// Under the proven switch, on a set served by today's rules (no background
// rules) whose view shows a test the worker scope the run reached
// (ServeExactWorker, models.Mapping.Worker), a request identical to one the
// open test's own recording already answered is one call more than the test
// recorded: the worker scope's identical recording never answers it. It
// misses as an extra call, recorded once, and the worker scope's recording
// stays unspent: between tests it answers the request. A request the test
// never recorded is still answered by the worker scope. Answered by the
// worker scope's copy, a test that made its recorded call twice passed.
func TestTheWorkerScopeNeverAnswersARequestOneMoreThanTheTestRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	worker := workerProbe("mock-worker-x", time.Second, "/x", "worker")
	other := workerProbe("mock-worker-y", 2*time.Second, "/y", "why")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	mm := stagedManager(t, nil, []*models.Mock{worker, other, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	workerNames := []string{worker.Name, other.Name}
	mm.ServeExactWorker(workerNames)
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() || integrations.S12Background(mm) {
		t.Fatal("precondition: test-a is not served exactly, by today's rules")
	}

	if got, miss := httpMissCall(t, mm, "/x"); got != "own" {
		t.Fatalf("test-a's GET /x was answered %q (miss %+v); want its own", got, miss)
	}
	got, miss := httpMissCall(t, mm, "/x")
	if got != "" {
		t.Fatalf("test-a's second GET /x, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second GET /x missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if got, miss := httpMissCall(t, mm, "/y"); got != "why" {
		t.Fatalf("test-a's GET /y, a request it never recorded, was answered %q (miss %+v); want the worker scope's", got, miss)
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	if got, miss := httpMissCall(t, mm, "/x"); got != "worker" {
		t.Fatalf("the GET /x between tests was answered %q (miss %+v); want the worker scope's, unspent", got, miss)
	}
}

// A request that differs from the open test's own answered recording only
// where learning made noise (an id the application sends anew on each call)
// is one call more than the test recorded too. The learned-noise pass
// (learnedMatch) picks from every unspent recording it matches modulo that
// noise, the worker scope's copy among them; the copy's consume door asks
// the own-call check, which reads the recording's learned noise as the
// strict matcher does, and refuses it. The request misses as an extra call,
// recorded once, and the worker scope's recording stays unspent: between
// tests it answers the request.
func TestALearnedNoiseRequestOneMoreThanTheTestRecordedIsNotTheWorkerScopes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const recorded = `{"id":"rec","q":1}`
	noisy := func(m *models.Mock) *models.Mock {
		m.Spec.HTTPReq.Method = models.Method("POST")
		m.Spec.HTTPReq.Header["Content-Type"] = "application/json"
		m.Spec.HTTPReq.Body = recorded
		m.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
		return m
	}
	post := func(id string) string {
		b := `{"id":"` + id + `","q":1}`
		return "POST /x HTTP/1.1\r\nHost: deps\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\n\r\n" + b
	}
	worker := noisy(workerProbe("mock-worker-x", time.Second, "/x", "worker"))
	own := noisy(workerProbe("mock-own-x", 4*time.Second, "/x", "own"))
	mm := stagedManager(t, nil, []*models.Mock{worker, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	mm.ServeExactWorker([]string{worker.Name})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})

	if got, miss := httpMissSend(t, mm, post("r1")); got != "own" {
		t.Fatalf("test-a's POST /x was answered %q (miss %+v); want its own, through its learned noise", got, miss)
	}
	got, miss := httpMissSend(t, mm, post("r2"))
	if got != "" {
		t.Fatalf("test-a's second POST /x, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second POST /x missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	if got, miss := httpMissSend(t, mm, post("r3")); got != "worker" {
		t.Fatalf("the POST /x between tests was answered %q (miss %+v); want the worker scope's, unspent", got, miss)
	}
}

// A request one more than the test recorded that no background recording
// is about to answer asks whether it is one of the test's own when it
// misses (integrations.OwnRepeat), and is matched again with the background
// hidden: its miss names no background recording as its closest, as when
// the background holds a copy of it. The worker scope's GET /x/2 is never
// compared with the test's second GET /x.
func TestARequestOneMoreThanTheTestRecordedIsDiagnosedWithTheBackgroundHidden(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	near := workerProbe("mock-worker-x2", time.Second, "/x/2", "worker")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	mm := stagedManager(t, nil, []*models.Mock{near, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	mm.ServeExactWorker([]string{near.Name})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})

	if got, miss := httpMissCall(t, mm, "/x"); got != "own" {
		t.Fatalf("test-a's GET /x was answered %q (miss %+v); want its own", got, miss)
	}
	got, miss := httpMissCall(t, mm, "/x")
	if got != "" {
		t.Fatalf("test-a's second GET /x, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second GET /x missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if miss.ClosestMock == near.Name || miss.CandidateCount != 0 {
		t.Fatalf("the second GET /x was compared with the worker scope (closest %q, %d candidates); want no background recording compared", miss.ClosestMock, miss.CandidateCount)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if got, miss := httpMissCall(t, mm, "/x/2"); got != "worker" {
		t.Fatalf("the GET /x/2 between tests was answered %q (miss %+v); want the worker scope's, unspent", got, miss)
	}
}

// A call decided on a snapshot's read of the round (RoundSnapshot.CallRound:
// a call that waited for the store to open its test, and caught up) is held
// to the worker scope's rule as a call MarkCallRound marked is: in a test's
// view that shows a worker scope, under today's rules, both say the view
// shows the test a background (integrations.ShowsBackgroundIn), so neither
// lets the worker scope answer a call one more than the test recorded. A
// test's view with no worker scope, and the view between tests, show none.
// Before, the snapshot's CallRound kept no sequence view and said none, and
// a caught-up Mongo find one more than its test recorded was answered by the
// worker scope's copy of it.
func TestASnapshotsCallRoundShowsTheWorkerScopeAsTheMarkDoes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	worker := workerProbe("mock-worker-x", time.Second, "/x", "worker")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	for _, tc := range []struct {
		what    string
		workers []string
		test    bool
		want    bool
	}{
		{"a test's view with a worker scope", []string{worker.Name}, true, true},
		{"a test's view with no worker scope", nil, true, false},
		{"the view between tests", []string{worker.Name}, false, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			mm := stagedManager(t, nil, []*models.Mock{worker, own})
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
			if tc.workers != nil {
				mm.ServeExactWorker(tc.workers)
			}
			mm.ServeExactPool(models.PoolKeyNone, nil)
			if tc.test {
				mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})
			}
			if !mm.ExactServing() || integrations.S12Background(mm) {
				t.Fatal("precondition: the set is not served exactly, by today's rules")
			}
			snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
			if !ok {
				t.Fatal("precondition: the store reads no round")
			}
			marked := integrations.ShowsBackgroundIn(integrations.MarkCallRound(mm), mm)
			snapped := integrations.ShowsBackgroundIn(snap.CallRound(), mm)
			if marked != tc.want || snapped != tc.want {
				t.Fatalf("the view shows a background: marked %v, from the snapshot %v; want %v for both", marked, snapped, tc.want)
			}
		})
	}
}

// pairedRepeatRun stages, under the background rules, test-0's recording
// mock-own-0 and test-1's mock-own-1 of the same GET /dup, each in its test's
// recorded window and answered as its test was answered. baked says record's
// judging moved mock-own-1 into the set's worker scope, reached from the
// set's start, so test-1 lists nothing. The view is between tests.
func pairedRepeatRun(t *testing.T, baked bool) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	start := time.Now().Add(time.Second)
	own0 := r3Mock("mock-own-0", start.Add(100*time.Millisecond), "GET", "/dup", "", `{"test":0}`)
	own1 := r3Mock("mock-own-1", start.Add(1100*time.Millisecond), "GET", "/dup", "", `{"test":1}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)},
		{TestCase: "test-1", Start: start.Add(time.Second), End: start.Add(1900 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, []*models.Mock{own0, own1}, models.BaseTime, start.Add(time.Hour))
	var worker []string
	if baked {
		worker = []string{own1.Name}
	}
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, worker)
	mm.ServeExactWorker(worker)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, nil)
	return p, mm
}

// Under the proven switch, on a set served by the background rules, test-0
// recorded GET /dup once and test-1 recorded the identical call once. A
// test-0 that calls it twice:
//
//   - In record's full round the second call is paired with test-1's
//     recording: answered with it, a claim reported for judging
//     (models.AnswerProvisional, NotOwned), not consumed. This is the full
//     round's stated limit: the full round learns, its pairing gives no
//     verdict.
//   - Judging then voids the pairing when test-1 consumed its recording
//     itself (it stays test-1's), or else moves the recording into the set's
//     worker scope. Either way the confirming round, a plain replay, misses
//     the second call as one call more than test-0 recorded
//     (models.MissExtraCall, recorded 1), and the recording still answers
//     test-1's call: test-1's own, or the worker scope's, which the miss left
//     unspent.
//
// Answered by the worker scope's copy in the confirming round, a test that
// made its recorded call twice passed.
func TestACallPairedInTheFullRoundOneMoreThanTheTestRecordedMissesInTheConfirmingRound(t *testing.T) {
	t.Run("full round", func(t *testing.T) {
		p, mm := pairedRepeatRun(t, false)
		mm.SetPairing(true)
		h := httpint.New(zap.NewNop())
		opts := models.OutgoingOptions{Learn: true}
		mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0")})
		p.ContinueTestErrorCapture()
		if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/dup")); code != http.StatusOK || body != `{"test":0}` {
			t.Fatalf("test-0's GET /dup got %d %s; want its own", code, body)
		}
		if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/dup")); code != http.StatusOK || body != `{"test":1}` {
			t.Fatalf("test-0's second GET /dup got %d %s; want test-1's recording, paired", code, body)
		}
		calls, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].Answer != models.AnswerProvisional || !calls[0].NotOwned || calls[0].ClosestMock != "mock-own-1" {
			t.Fatalf("reported %+v; want the one pairing with mock-own-1, an answer for judging", calls)
		}
	})
	for _, tc := range []struct {
		judged string
		baked  bool
	}{
		{"pairing void: test-1 consumed its recording", false},
		{"recording moved into the worker scope", true},
	} {
		t.Run("confirming round, "+tc.judged, func(t *testing.T) {
			p, mm := pairedRepeatRun(t, tc.baked)
			h := httpint.New(zap.NewNop())
			mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0")})
			p.ContinueTestErrorCapture()
			if code, body, err := sendWithin(h, p, mm, httpGet("/dup")); err != nil || code != http.StatusOK || body != `{"test":0}` {
				t.Fatalf("test-0's GET /dup got %d %s %v; want its own", code, body, err)
			}
			code, body, err := sendWithin(h, p, mm, httpGet("/dup"))
			if err == nil && code == http.StatusOK {
				t.Fatalf("test-0's second GET /dup, one more than it recorded, was answered %s", body)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("test-0's second GET /dup: %v", err)
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != 1 || misses[0].ActualSummary != "GET /dup" || misses[0].Kind != models.MissExtraCall || misses[0].Recorded != 1 {
				t.Fatalf("misses %+v; want one extra_call miss of GET /dup, recorded 1", misses)
			}
			mm.ServeExactPool(models.PoolKeyNone, nil)
			var owned []*models.Mock
			if !tc.baked {
				owned = []*models.Mock{roundMock(mm, "mock-own-1")}
			}
			mm.ServeExactTest("test-1", owned)
			p.ContinueTestErrorCapture()
			if code, body, err := sendWithin(h, p, mm, httpGet("/dup")); err != nil || code != http.StatusOK || body != `{"test":1}` {
				t.Fatalf("test-1's GET /dup got %d %s %v; want mock-own-1", code, body, err)
			}
		})
	}
}

// Under the proven switch, two identical requests made at once, of a request
// the open test recorded once, while the background holds an identical
// recording (the worker scope's under today's rules, the pool's under the
// background rules): one is answered with the test's own recording, and the
// other misses as one call more than the test recorded, recorded once. The
// background's recording stays unspent. The interleave is fixed, not timed:
// the first request matches the test's recording, and before its consume
// settles the second runs to its end (OnConsume), so the first finds the
// recording taken and is matched again. Decided on what the test had
// answered so far, both requests read the background, and the first was
// answered with the background's copy every run: a test whose app sent a
// recorded call twice at once (a double submit, a client retry) passed.
func TestTwoIdenticalRequestsAtOnceOfARequestTheTestRecordedOnceAnswerOnlyOne(t *testing.T) {
	for _, scope := range []string{"worker", "pool"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			bg := workerProbe("mock-"+scope+"-x", time.Second, "/x", scope)
			own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
			mm := stagedManager(t, nil, []*models.Mock{bg, own})
			mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
			pool := []string(nil)
			if scope == "worker" {
				mm.ServeExactWorker([]string{bg.Name})
			} else {
				pool = []string{bg.Name}
				mm.ServeExactBackground(models.RecordedWithS12Routing, pool, nil)
			}
			mm.ServeExactPool(models.PoolKeyNone, pool)
			mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})
			if !mm.ExactServing() || integrations.S12Background(mm) != (scope == "pool") {
				t.Fatalf("precondition: test-a is not served exactly, by the %s's rules", scope)
			}

			armed := true
			var second string
			var secondMiss *models.MockMismatchReport
			mm.OnConsume(func(name string) {
				if name != own.Name || !armed {
					return
				}
				armed = false
				second, secondMiss = httpMissCall(t, mm, "/x")
			})
			first, miss := httpMissCall(t, mm, "/x")
			mm.OnConsume(nil)
			if armed {
				t.Fatalf("precondition: the first GET /x never came to consume test-a's recording (answered %q, miss %+v)", first, miss)
			}
			if second != "own" {
				t.Fatalf("the GET /x that ran while the first was consuming was answered %q (miss %+v); want test-a's own", second, secondMiss)
			}
			if first != "" {
				t.Fatalf("the first GET /x, whose recording the second took, was answered %q: two calls of a call recorded once both passed", first)
			}
			if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
				t.Fatalf("the first GET /x missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
			}

			mm.ServeExactPool(models.PoolKeyNone, pool)
			if got, miss := httpMissCall(t, mm, "/x"); got != scope {
				t.Fatalf("the GET /x between tests was answered %q (miss %+v); want the %s's, unspent", got, miss, scope)
			}
		})
	}
}

// The generic parser's row of the same: two identical exchanges sent at once,
// of an exchange the open test recorded once, on a set whose pool holds an
// identical recording. One is answered with the test's own recording, the
// other misses as one exchange more than the test recorded, and the pool's
// recording stays unspent.
func TestTwoIdenticalExchangesAtOnceOfAnExchangeTheTestRecordedOnceAnswerOnlyOne(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool}, models.BaseTime, now.Add(time.Hour))
	poolNames := []string{pool.Name}
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}

	armed := true
	var second string
	var secondMiss *models.MockMismatchReport
	mm.OnConsume(func(name string) {
		if name != own.Name || !armed {
			return
		}
		armed = false
		second, secondMiss = genericCall(t, mm, "PING")
	})
	first, miss := genericCall(t, mm, "PING")
	mm.OnConsume(nil)
	if armed {
		t.Fatalf("precondition: the first PING never came to consume test-0's recording (answered %q, miss %+v)", first, miss)
	}
	if second != "OWN1" {
		t.Fatalf("the PING sent while the first was consuming was answered %q (miss %+v); want test-0's own OWN1", second, secondMiss)
	}
	if first != "" {
		t.Fatalf("the first PING, whose recording the second took, was answered %q: two exchanges of one recorded once both passed", first)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the first PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}

	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	if got, miss := genericCall(t, mm, "PING"); got != "POOL" {
		t.Fatalf("the PING between tests was answered %q (miss %+v); want the pool's POOL, unspent", got, miss)
	}
}

// Under the proven switch, a call's held store that decided the call is one
// of the open test's own calls (HideBackgroundWhen, its decision true) hides
// the background at every door and walk a parser reaches it by: each consume
// door (DeleteUnFilteredMock, DeleteStartupMock) and the by-kind spend
// (SpendByKind) refuse a background recording the open test does not own and
// leave it unspent, and each walk (RangePerTestMocksInWindow,
// RangeSessionMocksWithKey) skips it; each still reaches the open test's own
// recording. Decided the other way, each door and walk reaches the
// background recording: the refusal is the decision's. The background here is
// the pool (a recording made between tests, in the reusable tiers) and the
// reached worker scope (a recording a worker made inside the test's window,
// in the per-test tier).
func TestACallDecidedTheTestsOwnIsRefusedTheBackgroundAtEveryDoorAndWalk(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	type staged struct {
		mm                *MockManager
		own, pool, worker *models.Mock
	}
	stage := func(t *testing.T) staged {
		t.Helper()
		now := time.Now()
		start := now.Add(time.Second)
		pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
		worker := ownRepeatExchange("mock-worker-ping", "PING", "WRKR", start.Add(50*time.Millisecond))
		own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
		// Staged in the test's window, the worker's recording is in the
		// per-test tier, the pool's in the reusable ones.
		mm.SetMocksWithWindow([]*models.Mock{worker, own}, []*models.Mock{pool}, start, start.Add(900*time.Millisecond))
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{pool.Name}, []string{worker.Name})
		mm.ServeExactWorker([]string{worker.Name})
		mm.ServeExactPool(models.PoolKeyNone, []string{pool.Name})
		mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
		if !mm.ExactServing() || !integrations.S12Background(mm) {
			t.Fatal("precondition: test-0 is not served exactly, by the background rules")
		}
		return staged{mm: mm, own: roundMock(mm, own.Name), pool: roundMock(mm, pool.Name), worker: roundMock(mm, worker.Name)}
	}
	walked := func(names []string, name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	oneKey := &integrations.MockIndex{Keys: func(*models.Mock) []string { return []string{"PING"} }}
	cases := []struct {
		door string
		// bg is the background recording the door or walk reaches.
		bg func(s staged) *models.Mock
		// reach is whether the door settles mk, or the walk gives mk.
		reach func(t *testing.T, db integrations.MockMemDb, mk *models.Mock) bool
		// settles says the door spends what it reaches: a refused one
		// stays unspent.
		settles bool
		// ownToo says the door reaches the open test's own recording.
		ownToo bool
	}{
		{"DeleteUnFilteredMock", func(s staged) *models.Mock { return s.pool },
			func(_ *testing.T, db integrations.MockMemDb, mk *models.Mock) bool {
				return db.DeleteUnFilteredMock(*mk)
			}, true, true},
		{"DeleteStartupMock", func(s staged) *models.Mock { return s.pool },
			func(_ *testing.T, db integrations.MockMemDb, mk *models.Mock) bool { return db.DeleteStartupMock(*mk) }, true, true},
		// By kind a test's own recording is never spent: it is no background
		// recording.
		{"SpendByKind", func(s staged) *models.Mock { return s.pool },
			func(_ *testing.T, db integrations.MockMemDb, mk *models.Mock) bool {
				return db.(integrations.ByKindBackground).SpendByKind(mk)
			}, true, false},
		{"RangePerTestMocksInWindow", func(s staged) *models.Mock { return s.worker },
			func(t *testing.T, db integrations.MockMemDb, mk *models.Mock) bool {
				var names []string
				if err := db.(integrations.PerTestRanger).RangePerTestMocksInWindow(func(m *models.Mock) bool {
					names = append(names, m.Name)
					return true
				}); err != nil {
					t.Fatal(err)
				}
				return walked(names, mk.Name)
			}, false, true},
		{"RangeSessionMocksWithKey", func(s staged) *models.Mock { return s.pool },
			func(t *testing.T, db integrations.MockMemDb, mk *models.Mock) bool {
				var names []string
				if err := db.(integrations.SessionKeyReader).RangeSessionMocksWithKey(oneKey, "PING", func(m *models.Mock) bool {
					names = append(names, m.Name)
					return true
				}); err != nil {
					t.Fatal(err)
				}
				return walked(names, mk.Name)
			}, false, true},
	}
	for _, c := range cases {
		t.Run(c.door, func(t *testing.T) {
			for _, owns := range []bool{false, true} {
				s := stage(t)
				bg := c.bg(s)
				if bg == nil || s.own == nil {
					t.Fatal("precondition: a recording was not staged")
				}
				held, _ := s.mm.HoldToRound(s.mm.MarkCallRound())
				hider := held.(integrations.BackgroundHider)
				decided := owns
				held = hider.HideBackgroundWhen(func() bool { return decided })
				if !c.settles && hider.HidesBackground() != owns {
					// A walk hides what a door's decision hid: no walk
					// asks the decision itself.
					t.Fatalf("owns %v: the held store's decision is not its call's", owns)
				}
				if got := c.reach(t, held, bg); got == owns {
					t.Fatalf("owns %v: %s reached the background's %s: %v; want %v", owns, c.door, bg.Name, got, !owns)
				}
				if c.settles && owns && s.mm.MockSpent(bg.Name) {
					t.Fatalf("%s refused the background's %s and spent it", c.door, bg.Name)
				}
				if c.ownToo && !c.reach(t, held, s.own) {
					t.Fatalf("owns %v: %s does not reach the open test's own %s", owns, c.door, s.own.Name)
				}
			}
		})
	}
}

// A connection's scoped view (scopedMockDb) held to a call passes on its held
// store's answers for the call, as the bare held store gives them. A
// recording of the worker scope the run reached is the background's, and the
// open test's own is not (InBackground: Postgres hides a statement's
// background copy by it, and Mongo's learn step reads it). A call decided the
// test's own is refused the worker scope's recording, never the test's own
// (RefusesAnswer: HTTP's variable pass asks it before it binds), and the view
// says it hides the background (OwnRepeat: a MySQL, gRPC or HTTP/2 miss asks
// it before it is reported). A call decided otherwise is refused nothing.
// Without the view passing any one of them on, on a connection opened before
// exact serving began (scopedFor decides once per connection), the
// background could answer a call one more than the test recorded, or be
// compared with its miss.
func TestAScopedViewHeldToACallPassesOnTheOwnCallDecision(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	worker := workerProbe("mock-worker-x", time.Second, "/x", "worker")
	own := workerProbe("mock-own-x", 4*time.Second, "/x", "own")
	mm := stagedManager(t, nil, []*models.Mock{worker, own})
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-a", Start: exactT0.Add(3 * time.Second), End: exactT0.Add(5 * time.Second)}})
	mm.ServeExactWorker([]string{worker.Name})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, own.Name)})
	worker, own = roundMock(mm, worker.Name), roundMock(mm, own.Name)
	if !mm.ExactServing() || worker == nil || own == nil {
		t.Fatal("precondition: test-a is not served exactly with both recordings staged")
	}

	for _, owns := range []bool{false, true} {
		scoped := &scopedMockDb{MockMemDb: mm}
		call := scoped.MarkCallRound()
		held, _ := scoped.HoldToRound(call)
		if _, ok := held.(*scopedMockDb); !ok {
			t.Fatalf("the scoped view held to a call is a %T; want the view over the held store", held)
		}
		if !integrations.InBackground(held, worker.Name) || integrations.InBackground(held, own.Name) {
			t.Fatalf("InBackground through the view: the worker scope's %v, test-a's own %v; want true, false",
				integrations.InBackground(held, worker.Name), integrations.InBackground(held, own.Name))
		}
		decided := owns
		held = integrations.OwnRepeatRead(held, call, func() bool { return decided })
		if got := integrations.RefusesAnswer(held, worker); got != owns {
			t.Fatalf("owns %v: the view refuses the worker scope's recording %v; want %v", owns, got, owns)
		}
		if integrations.RefusesAnswer(held, own) {
			t.Fatalf("owns %v: the view refuses test-a's own recording", owns)
		}
		if got := integrations.OwnRepeat(held); got != owns {
			t.Fatalf("owns %v: the view says it hides the background %v; want %v", owns, got, owns)
		}
		if mm.MockSpent(worker.Name) || mm.MockSpent(own.Name) {
			t.Fatalf("owns %v: a recording was spent by a call no recording answered", owns)
		}
	}
}

// An exchange one more than the test recorded that no background recording
// is about to answer asks whether it is one of the test's own when it misses
// (integrations.OwnRepeat), and is reported with the background hidden: its
// miss is compared with no background recording, as when the background
// holds a copy of it. The pool's PINGS, close to PING but not it, is never
// compared with the test's second PING, and still answers its own exchange.
func TestAnExchangeOneMoreThanTheTestRecordedIsDiagnosedWithTheBackgroundHidden(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	near := ownRepeatExchange("mock-pool-pings", "PINGS", "POOL", now)
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{near}, models.BaseTime, now.Add(time.Hour))
	poolNames := []string{near.Name}
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}

	if got, miss := genericCall(t, mm, "PING"); got != "OWN1" {
		t.Fatalf("test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	got, miss := genericCall(t, mm, "PING")
	if got != "" {
		t.Fatalf("test-0's second PING, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if miss.ClosestMock == near.Name || miss.CandidateCount != 0 {
		t.Fatalf("the second PING was compared with the pool (closest %q, %d candidates); want no background recording compared", miss.ClosestMock, miss.CandidateCount)
	}
	if got, miss := genericCall(t, mm, "PINGS"); got != "POOL" {
		t.Fatalf("test-0's PINGS, an exchange it never recorded, was answered %q (miss %+v); want the pool's POOL, unspent", got, miss)
	}
}
