package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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

// genericCall sends req on a new connection through the generic parser and
// returns the four bytes it was answered with, or the miss's report when it
// was answered nothing.
func genericCall(t *testing.T, mm *MockManager, req string) (string, *models.MockMismatchReport) {
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
	if _, err := io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: deps\r\n\r\n"); err != nil {
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
		t.Fatalf("GET %s was answered no 200, and its miss has no report: %v", path, err)
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
