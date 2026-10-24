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
			if !strings.Contains(r.NextSteps, "staged a test set again while it decided this request") {
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

// A call of one test set held after its first read of the round while the
// replay stages the next set, whose open test owns a recording that is the
// call byte for byte, is never answered with that recording, nor diagnosed
// against it: its matcher and its miss read the store held to the call's
// first read (integrations.HoldToRound), which finds nothing once the
// staging moved. The call misses, saying the set was staged again under it,
// and the recording, never spent, answers the next set's own call. On the
// bare store and on a connection's view of it alike.
func TestACallHeldWhileTheNextSetIsStagedIsNeverAnsweredFromIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	type mode struct {
		name string
		opts models.OutgoingOptions
	}
	type view struct {
		name string
		of   func(*heldFirstRead) integrations.MockMemDb
	}
	for _, tc := range []mode{{"replay", models.OutgoingOptions{}}, {"record's full round", models.OutgoingOptions{Learn: true, LearnPool: true}}} {
		for _, vw := range []view{
			{"the store", func(h *heldFirstRead) integrations.MockMemDb { return h }},
			{"a connection's view", func(h *heldFirstRead) integrations.MockMemDb { return &scopedMockDb{MockMemDb: h} }},
		} {
			t.Run(tc.name+" on "+vw.name, func(t *testing.T) {
				now := time.Now()
				users := r3Mock("s1-users", now, "GET", "/users/a", "", `{"name":"a"}`)
				next := r3Mock("s2-jobs", now.Add(time.Second), "GET", "/jobs/1", "", `{"job":1}`)
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				mm.SetPairing(tc.opts.Learn)
				stage := func(own *models.Mock, test string) {
					mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, now.Add(time.Hour))
					if tc.opts.Learn {
						mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
					}
					mm.ServeExactTest(test, []*models.Mock{own})
				}
				stage(users, "T1")
				held := &heldFirstRead{MockManager: mm, moved: func() { stage(next, "T2") }}
				db := vw.of(held)
				tracker := learn.NewTracker()

				r := sendForMiss(t, db, tc.opts, tracker, jobsCall)
				if !held.fired.Load() {
					t.Fatal("precondition: the replay never staged the next set under the call")
				}
				if r == nil {
					t.Fatalf("a call of the set before was answered with the next set's %s", next.Name)
				}
				if r.NotOwned || r.ClosestMock == next.Name {
					t.Fatalf("a call of the set before was diagnosed against the next set's %s: %+v", next.Name, r)
				}
				if !strings.Contains(r.NextSteps, "staged a test set again while it decided this request, so keploy answered it with no recording") {
					t.Fatalf("the miss does not say the set was staged again under the call: %q", r.NextSteps)
				}

				// Never spent on the call before, it answers the next set's own.
				if r := sendForMiss(t, db, tc.opts, tracker, jobsCall); r != nil {
					t.Fatalf("the next set's own call missed after a call of the set before: %+v", r)
				}
			})
		}
	}
}

// A replay's call of one test set held after its first read of the round
// while the replay stages the next set is never answered by what record's
// full round learned onto the next set's recording (learnedMatch, the first
// matcher a replay asks under the switch): the next set's open test owns a
// recording that learned the one header the call differs in, and the learned
// match reads the store held to the call's first read, which finds nothing
// once the staging moved. The call misses saying the set was staged again
// under it, and the recording, never spent, answers the next set's own call
// through its learned noise. On the bare store and on a connection's view.
func TestALearnedMatchHeldWhileTheNextSetIsStagedIsNeverAnsweredFromIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, vw := range []struct {
		name string
		of   func(*heldFirstRead) integrations.MockMemDb
	}{
		{"the store", func(h *heldFirstRead) integrations.MockMemDb { return h }},
		{"a connection's view", func(h *heldFirstRead) integrations.MockMemDb { return &scopedMockDb{MockMemDb: h} }},
	} {
		t.Run(vw.name, func(t *testing.T) {
			now := time.Now()
			users := r3Mock("s1-users", now, "GET", "/users/a", "", `{"name":"a"}`)
			next := r3Mock("s2-jobs", now.Add(time.Second), "GET", "/jobs/1", "", `{"job":1}`)
			next.Spec.HTTPReq.Header["X-Req-Id"] = "recorded"
			next.Spec.ReqBodyNoise = map[string][]string{"header.x-req-id": {}}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			stage := func(own *models.Mock, test string) {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, now.Add(time.Hour))
				mm.ServeExactTest(test, []*models.Mock{own})
			}
			stage(users, "T1")
			held := &heldFirstRead{MockManager: mm, moved: func() { stage(next, "T2") }}
			db := vw.of(held)
			call := "GET /jobs/1 HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nX-Req-Id: live\r\n\r\n"

			r := sendForMiss(t, db, models.OutgoingOptions{}, learn.NewTracker(), call)
			if !held.fired.Load() {
				t.Fatal("precondition: the replay never staged the next set under the call")
			}
			if r == nil {
				t.Fatalf("a call of the set before was answered with what record's full round learned onto the next set's %s", next.Name)
			}
			if r.NotOwned || r.ClosestMock == next.Name {
				t.Fatalf("a call of the set before was diagnosed against the next set's %s: %+v", next.Name, r)
			}
			if !strings.Contains(r.NextSteps, "staged a test set again while it decided this request, so keploy answered it with no recording") {
				t.Fatalf("the miss does not say the set was staged again under the call: %q", r.NextSteps)
			}

			// Never spent on the call before, it answers the next set's own
			// call, which differs from it only in the learned header.
			if r := sendForMiss(t, db, models.OutgoingOptions{}, learn.NewTracker(), call); r != nil {
				t.Fatalf("the next set's own call missed after a call of the set before: %+v", r)
			}
		})
	}
}

// A call of one test set held after its first read of the round while the
// replay stages the next set, whose open test has already answered a call
// with the recording that is this call, is no call more than that test
// recorded: the miss's diagnosis reads the store held to the call's first
// read, which shows nothing of the next set's test. Under replay's matcher
// and in record's full round's learn step alike.
func TestACallHeldWhileTheNextSetIsStagedIsNoExtraCallOfItsTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name string
		opts models.OutgoingOptions
	}{{"replay", models.OutgoingOptions{}}, {"record's full round", models.OutgoingOptions{Learn: true, LearnPool: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			users := r3Mock("s1-users", now, "GET", "/users/a", "", `{"name":"a"}`)
			next := r3Mock("s2-jobs", now.Add(time.Second), "GET", "/jobs/1", "", `{"job":1}`)
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetPairing(tc.opts.Learn)
			stage := func(own *models.Mock, test string) {
				mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, now.Add(time.Hour))
				if tc.opts.Learn {
					mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
				}
				mm.ServeExactTest(test, []*models.Mock{own})
			}
			stage(users, "T1")
			answered := false
			db := &heldFirstRead{MockManager: mm, moved: func() {
				stage(next, "T2")
				// T2's own call, answered with its recording.
				mocks, _ := mm.GetPerTestMocksInWindow()
				session, _ := mm.GetSessionMocks()
				for _, mk := range append(mocks, session...) {
					if mk.Name == next.Name && !answered {
						c := *mk
						answered = mm.DeleteFilteredMock(c) || mm.UpdateUnFilteredMock(mk, &c)
					}
				}
			}}

			r := sendForMiss(t, db, tc.opts, learn.NewTracker(), jobsCall)
			if !db.fired.Load() || !answered {
				t.Fatalf("precondition: the replay staged the next set under the call (%v) and its test answered its own call (%v)", db.fired.Load(), answered)
			}
			if r == nil {
				t.Fatalf("a call of the set before was answered with the next set's %s", next.Name)
			}
			if r.MissKind == models.MissExtraCall {
				t.Fatalf("a call of the set before was told as one call more than the next set's test recorded: %+v", r)
			}
			if r.NotOwned || r.ClosestMock == next.Name {
				t.Fatalf("a call of the set before was diagnosed against the next set's %s: %+v", next.Name, r)
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

// marksNoHold is a store that marks a call's round and looks up recordings
// made elsewhere (integrations.StagedFinder) but does not hold a call to its
// round (no integrations.RoundHolder): a parser-side wrapper that forgot to
// forward it.
type marksNoHold struct {
	integrations.MockMemDb
	f integrations.StagedFinder
}

func (w marksNoHold) MarkCallRound() integrations.CallRound { return w.f.MarkCallRound() }

func (w marksNoHold) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	return w.f.FindStagedNotOwned(kind, call, match)
}

// A call marked by a store that cannot hold it to its round is never
// answered from that store: integrations.HoldToRound fails closed, so the
// call's matcher and its miss read nothing, and the miss says why. A call
// marked in one set and held while the replay stages the next, whose open
// test owns a recording that is the call itself, therefore misses, never
// answered with the next set's recording nor told it was staged again; so
// does a call whose staging never moved, since such a store cannot tell
// that it did. For HTTP and Generic, on the store and on a connection's view
// of it.
func TestACallMarkedByAStoreThatCannotHoldItIsNeverAnsweredFromIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, vw := range []struct {
		name string
		of   func(marksNoHold) integrations.MockMemDb
	}{
		{"the store", func(w marksNoHold) integrations.MockMemDb { return w }},
		{"a connection's view", func(w marksNoHold) integrations.MockMemDb { return &scopedMockDb{MockMemDb: w} }},
	} {
		for _, moves := range []bool{true, false} {
			name := vw.name
			if !moves {
				name += ", staging never moved"
			}
			t.Run("HTTP on "+name, func(t *testing.T) {
				now := time.Now()
				users := r3Mock("s1-users", now, "GET", "/users/a", "", `{"name":"a"}`)
				next := r3Mock("s2-jobs", now.Add(time.Second), "GET", "/jobs/1", "", `{"job":1}`)
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				stage := func(own *models.Mock, test string) {
					mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, now.Add(time.Hour))
					mm.ServeExactTest(test, []*models.Mock{own})
				}
				first := users
				if !moves {
					first = next
				}
				stage(first, "T1")
				held := &heldFirstRead{MockManager: mm}
				if moves {
					held.moved = func() { stage(next, "T2") }
				}
				r := sendForMiss(t, vw.of(marksNoHold{MockMemDb: held, f: held}), models.OutgoingOptions{}, learn.NewTracker(), jobsCall)
				if moves && !held.fired.Load() {
					t.Fatal("precondition: the replay never staged the next set under the call")
				}
				if r == nil {
					t.Fatalf("a call marked by a store that cannot hold it was answered with %s", next.Name)
				}
				if r.NotOwned || r.ClosestMock == next.Name {
					t.Fatalf("a call marked by a store that cannot hold it was diagnosed against %s: %+v", next.Name, r)
				}
				if !strings.Contains(r.NextSteps, integrations.UnheldNote) || strings.Contains(r.NextSteps, "staged a test set again") {
					t.Fatalf("the miss does not say its store could not hold it, alone: %q", r.NextSteps)
				}
				// The bare store holds: the recording, never spent, answers it.
				if r := sendForMiss(t, mm, models.OutgoingOptions{}, learn.NewTracker(), jobsCall); r != nil {
					t.Fatalf("the store that holds missed the call after: %+v", r)
				}
			})
			t.Run("Generic on "+name, func(t *testing.T) {
				mine := genericExchange("ex-mine", 0, "AUTH step2")
				next := genericExchange("s2-ex", 2*time.Second, "AUTH step4")
				first := mine
				if !moves {
					first = next
				}
				mm := stagedManager(t, nil, []*models.Mock{first})
				mm.ServeExactTest("T1", []*models.Mock{first})
				staged := false
				if moves {
					mm.exact.onMarked = func() {
						mm.exact.onMarked = nil
						mm.SetMocksWithWindow(nil, []*models.Mock{next}, models.BaseTime, time.Now())
						mm.ServeExactTest("S2-T1", []*models.Mock{next})
						staged = true
					}
				}
				r := genericServeIn(t, context.Background(), vw.of(marksNoHold{MockMemDb: mm, f: mm}), "AUTH step4")
				if moves && !staged {
					t.Fatal("precondition: the replay never staged the next set under the request")
				}
				if r == nil {
					t.Fatalf("a request marked by a store that cannot hold it was answered with %s", next.Name)
				}
				if r.NotOwned || r.ClosestMock == next.Name {
					t.Fatalf("a request marked by a store that cannot hold it was diagnosed against %s: %+v", next.Name, r)
				}
				if !strings.Contains(r.NextSteps, integrations.UnheldNote) || strings.Contains(r.NextSteps, "staged a test set again") {
					t.Fatalf("the miss does not say its store could not hold it, alone: %q", r.NextSteps)
				}
				if r := genericServe(t, mm, "AUTH step4"); r != nil {
					t.Fatalf("the store that holds missed the request after: %+v", r)
				}
			})
		}
	}
}
