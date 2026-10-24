package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// heldFirstRead is the agent's store, with moved run once, right after the
// first read of the round a call makes (its mark, MarkCallRound, or a
// RoundSnapshot): the replay moving while the call is held after its first
// read.
type heldFirstRead struct {
	*MockManager
	fired atomic.Bool
	moved func()
}

func (h *heldFirstRead) MarkCallRound() integrations.CallRound {
	call := h.MockManager.MarkCallRound()
	if h.moved != nil && h.fired.CompareAndSwap(false, true) {
		h.moved()
	}
	return call
}

func (h *heldFirstRead) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	snap, ok := h.MockManager.RoundSnapshot(q)
	if h.moved != nil && h.fired.CompareAndSwap(false, true) {
		h.moved()
	}
	return snap, ok
}

// sendForMiss sends one request through the HTTP integration under opts and
// returns its miss's report, nil when it was answered.
func sendForMiss(t *testing.T, db integrations.MockMemDb, opts models.OutgoingOptions, tracker *learn.Tracker, request string) *models.MockMismatchReport {
	t.Helper()
	var reported atomic.Value
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported.Store(err) })
	if tracker != nil {
		ctx = learn.WithTracker(ctx, tracker)
	}
	app, agentEnd := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, db, opts)
		_ = agentEnd.Close()
	}()
	t.Cleanup(func() { _ = app.Close(); <-done })
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	err, _ = reported.Load().(error)
	if err == nil {
		return nil
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &withReport) {
		t.Fatalf("the miss was reported as %v, with no mismatch report", err)
	}
	return withReport.MismatchReport()
}

const jobsCall = "GET /jobs/1 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"

// A call of one test set held after its first read of the round while the
// replay stages the next set is never named as a recording of the next set
// made elsewhere: the miss says the set moved under it, and the next set's
// own call made elsewhere still names that recording (its one miss was not
// spent on the call before). Under replay's matcher and in record's full
// round's learn step alike (today's rules: no pairing).
func TestACallHeldWhileTheNextSetIsStagedNamesNoRecordingOfIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name string
		opts models.OutgoingOptions
	}{{"replay", models.OutgoingOptions{}}, {"record's full round", models.OutgoingOptions{Learn: true, LearnPool: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			set1 := []*models.Mock{r3Mock("s1-users", now, "GET", "/users/a", "", `{"name":"a"}`)}
			next := r3Mock("s2-jobs", now.Add(time.Second), "GET", "/jobs/1", "", `{"job":1}`)
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func(set []*models.Mock) {
				mm.SetMocksWithWindow(nil, set, models.BaseTime, now.Add(time.Hour))
				mm.ServeExactPool("k", nil)
			}
			stage(set1)
			db := &heldFirstRead{MockManager: mm, moved: func() { stage([]*models.Mock{next}) }}
			tracker := learn.NewTracker()

			r := sendForMiss(t, db, tc.opts, tracker, jobsCall)
			if r == nil {
				t.Fatal("a call of the set before was answered from the next set's recording")
			}
			if r.NotOwned {
				t.Fatalf("a call of the set before was named as the next set's %s made elsewhere: %+v", next.Name, r)
			}
			if !strings.Contains(r.NextSteps, "staged the next test set while it decided this request") {
				t.Fatalf("the miss does not say the set moved under the call: %q", r.NextSteps)
			}
			if !db.fired.Load() {
				t.Fatal("precondition: the replay never staged the next set under the call")
			}

			// The next set's own call made elsewhere names it.
			r = sendForMiss(t, db, models.OutgoingOptions{}, tracker, jobsCall)
			if r == nil || !r.NotOwned || r.ClosestMock != next.Name {
				t.Fatalf("the next set's own call made elsewhere is %+v; want it named as %s", r, next.Name)
			}
		})
	}
}

// A call whose first read of the round was in its test's view, held while
// the replay ends that test, is never named as its own test's recording made
// elsewhere, nor paired with it as another test's: the call is that test's,
// not one judging moves to the worker scope.
func TestACallWhoseTestEndedUnderItIsNotItsOwnRecordingMadeElsewhere(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name string
		opts models.OutgoingOptions
	}{{"replay", models.OutgoingOptions{}}, {"record's full round", models.OutgoingOptions{Learn: true, LearnPool: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			own := r3Mock("t-jobs", now, "GET", "/jobs/1", "", `{"job":1}`)
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetPairing(tc.opts.Learn)
			mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, now.Add(time.Hour))
			if tc.opts.Learn {
				mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
			}
			mm.ServeExactTest("T", []*models.Mock{own})
			db := &heldFirstRead{MockManager: mm, moved: func() { mm.ServeExactPool("k", nil) }}

			r := sendForMiss(t, db, tc.opts, learn.NewTracker(), jobsCall)
			if !db.fired.Load() {
				t.Fatal("precondition: the replay never ended the test under the call")
			}
			if r == nil {
				t.Fatalf("a call of a test the replay ended was answered from that test's recording as another test's")
			}
			if r.NotOwned {
				t.Fatalf("a call of a test the replay ended was named as that test's own %s made elsewhere: %+v", own.Name, r)
			}
		})
	}
}
