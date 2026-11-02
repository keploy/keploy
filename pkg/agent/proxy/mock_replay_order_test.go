package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy mock record's full round, with the recorded test windows keploy mock
// replay sends, run out of its recorded order: test-2 (recorded after test-1)
// runs before test-1, either first (tests run in another order) or in a run
// of test-1 begun again (go test -count=2, gotestsum --rerun-fails). A late
// call of test-2, exactly its recording mock-own2, lands in test-1. The
// windows say mock-own2 was recorded after test-1's window, so the learn step
// does not read it as another place of the call, and would pair the call
// with test-1's own mock-own1 and learn what it differs in as noise onto it:
// a false green. So it would where test-2's window touched test-3's, and
// keploy mock replay sends the two as one window no test names. The agent
// ends the round's learn step at test-1's begin out of order, before its
// window opens (pkg/service/agent keepRecordedOrder, ordered by when each
// test began in its recording, a merged window's tests included;
// Proxy.SetLearnEnded), and the call then misses, with nothing learned.
func TestAnOutOfOrderBeginEndsLearningSoALaterRecordedTestsLateCallMisses(t *testing.T) {
	for _, c := range []struct {
		name          string
		rerun, merged bool
	}{
		{"test-2, then test-1", false, false},
		{"test-1, test-2, then test-1 again", true, false},
		{"test-2 (its window merged with test-3's), then test-1", false, true},
	} {
		rerun := c.rerun
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxyLogging(t, 100, zap.NewNop())
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
			own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
			own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
			mocks := []*models.Mock{pool, own1, own2}
			mm := NewMockManager(nil, nil, zap.NewNop())
			// As keploy mock replay derives them from the set's mappings:
			// test-1 began at +0.5s (mock-own1 at +0.7s), test-2 at +1.0s
			// (mock-own2 at +1.5s).
			windows := []models.TestWindow{
				{TestCase: "test-1", Start: now.Add(500 * time.Millisecond), End: own1.Spec.ResTimestampMock},
				{TestCase: "test-2", Start: now.Add(time.Second), End: own2.Spec.ResTimestampMock},
			}
			if c.merged {
				// test-3 began at +1.5s, as test-2's recording was
				// answered: their windows touched, one no test names.
				own3 := r3Mock("mock-own3", now.Add(2100*time.Millisecond), "POST", "/orders", `{"item":"fig"}`, `{"order":3}`)
				mocks = append(mocks, own3)
				windows[1] = models.TestWindow{Start: now.Add(time.Second), End: own3.Spec.ResTimestampMock}
			}
			mm.SeedRecordedWindows(windows)
			mm.SetMocksWithWindow(mocks, nil, models.BaseTime, now.Add(time.Hour))
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			mm.SetPoolNames([]string{"mock-pool"})
			mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
			end := func() {
				mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
				if _, err := p.GetMockErrors(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if rerun {
				// In recorded order the learn step runs: test-1's own call
				// is answered from its own recording.
				mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
				p.ContinueTestErrorCapture()
				if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":1}` {
					t.Fatalf("precondition: test-1's own call got %d %s", code, got)
				}
				if mm.LearnEnded() {
					t.Fatal("precondition: in recorded order the learn step runs")
				}
				end()
			}
			// test-2 runs, and ends with its call still in flight.
			mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
			p.ContinueTestErrorCapture()
			end()
			// test-1 begins out of its recorded order: the agent ends the
			// learn step before its window opens.
			if err := p.SetLearnEnded(true); err != nil {
				t.Fatal(err)
			}
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
			p.ContinueTestErrorCapture()
			code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"plum"}`)
			if _, ok := learned["mock-own1"]; ok || got == `{"order":1}` {
				t.Fatalf("FALSE GREEN: test-2's late call (exactly mock-own2) was paired with test-1's mock-own1 and its item learned onto it: answered %d %s, learned %v", code, got, learned)
			}
			if code == http.StatusOK {
				t.Fatalf("answered %d %s; want a miss", code, got)
			}
			if len(misses) == 0 {
				t.Fatal("the late call left no miss")
			}
			if len(learned) > 0 {
				t.Fatalf("learned %v once the learn step ended", learned)
			}
			// test-1's own call is still answered from its recording.
			if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":1}` {
				t.Fatalf("test-1's own call got %d %s", code, got)
			}
		})
	}
}

// keploy mock record's full round of a record that keeps test-1's recording
// from an earlier record (a partial record that did not run it, or test-1
// failed against real infra): test-2, recorded anew and so later than
// test-1's kept recording, runs first, and ends with its call still in
// flight. test-1's begin is in the order the round learns in (only the
// fresh tests order it), but the agent holds the learn step while test-1 is
// open (pkg/service/agent holdLearn; Proxy.SetLearnHeld): the round keeps no
// learning on test-1's recording, so nothing may be paired with it or
// learned onto it. test-2's late call, exactly its recording mock-own2,
// misses rather than being paired with test-1's mock-own1 and its item
// learned onto it (a false green), and so does test-1's own call that
// differs from its recording; its own call as recorded is answered. The
// hold serves the recordings as they are (it is no end: no kept-learning
// overlay), and its release takes the learn step back up: test-3, recorded
// anew, pairs its call that carries test-4's item with its own recording
// and learns it. The hold does not end the learn step: test-1's call as
// recorded is served by it and stated as served as recorded, as in any test,
// so test-3's learn step reads what F gave the application there. Before,
// the hold stood every learn step down, test-1's call was served by the
// replay's matcher with no view stated, and test-3's changed item was
// refused as a word that may stand in mock-own1 (a false red in every
// partial record whose later fresh test had a value to learn).
func TestAKeptTestsHeldLearnStepPairsNothingAndItsReleaseLearnsAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxyLogging(t, 100, zap.NewNop())
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	pool := r3Mock("mock-pool", now.Add(100*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":0}`)
	own1 := r3Mock("mock-own1", now.Add(700*time.Millisecond), "POST", "/orders", `{"item":"pear"}`, `{"order":1}`)
	own2 := r3Mock("mock-own2", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"plum"}`, `{"order":2}`)
	own3 := r3Mock("mock-own3", now.Add(2500*time.Millisecond), "POST", "/orders", `{"item":"fig"}`, `{"order":3}`)
	own4 := r3Mock("mock-own4", now.Add(3500*time.Millisecond), "POST", "/orders", `{"item":"kiwi"}`, `{"order":4}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-1", Start: now.Add(500 * time.Millisecond), End: own1.Spec.ResTimestampMock},
		{TestCase: "test-2", Start: now.Add(time.Second), End: own2.Spec.ResTimestampMock},
		{TestCase: "test-3", Start: now.Add(2 * time.Second), End: own3.Spec.ResTimestampMock},
		{TestCase: "test-4", Start: now.Add(3 * time.Second), End: own4.Spec.ResTimestampMock},
	})
	mm.SetMocksWithWindow([]*models.Mock{pool, own1, own2, own3, own4}, nil, models.BaseTime, now.Add(time.Hour))
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.SetPoolNames([]string{"mock-pool"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	end := func() {
		mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
		if _, err := p.GetMockErrors(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
	p.ContinueTestErrorCapture()
	end()

	if err := p.SetLearnHeld(true); err != nil {
		t.Fatal(err)
	}
	if !mm.LearnHeld() || !p.learnTracker.LearnHeld() || mm.LearnEnded() || p.learnTracker.LearnEnded() {
		t.Fatal("holding the learn step did not hold it, or ended it")
	}
	if mm.exact.learnEnded.Load() || mm.exact.kept.cur.Load() != nil {
		t.Fatal("holding the learn step ended it: the set's recordings would be served through the kept-learning overlay")
	}
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
	p.ContinueTestErrorCapture()
	code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"plum"}`)
	if _, ok := learned["mock-own1"]; ok || got == `{"order":1}` {
		t.Fatalf("FALSE GREEN: test-2's late call (exactly mock-own2) was paired with kept test-1's mock-own1: answered %d %s, learned %v", code, got, learned)
	}
	if code == http.StatusOK || len(misses) == 0 {
		t.Fatalf("answered %d %s with misses %v; want a miss", code, got, misses)
	}
	code, got, learned, misses = elsewhereSent(t, p, mm, `{"item":"quince"}`)
	if code == http.StatusOK || len(learned) > 0 || len(misses) == 0 {
		t.Fatalf("FALSE GREEN: kept test-1's call that differs from its recording got %d %s, learned %v (misses %v); want a miss, nothing learned", code, got, learned, misses)
	}
	if code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true}, roundPost(`{"item":"pear"}`)); code != http.StatusOK || got != `{"order":1}` {
		t.Fatalf("kept test-1's own call as recorded got %d %s", code, got)
	}
	end()

	if err := p.SetLearnHeld(false); err != nil {
		t.Fatal(err)
	}
	if mm.LearnHeld() || p.learnTracker.LearnHeld() {
		t.Fatal("the hold's release did not take the learn step back up")
	}
	// test-3's call carries the item test-4 was recorded with (a value that
	// drifted onto the next test's): paired with test-3's own recording,
	// and its item learned onto it, as in recorded order.
	mm.ServeExactTest("test-3", []*models.Mock{roundMock(mm, "mock-own3")})
	p.ContinueTestErrorCapture()
	code, got, learned, _ = elsewhereSent(t, p, mm, `{"item":"kiwi"}`)
	if _, ok := learned["mock-own3"]; code != http.StatusOK || got != `{"order":3}` || !ok {
		t.Fatalf("fresh test-3's call that differs from its recording got %d %s, learned %v; want it answered from mock-own3 with its item learned", code, got, learned)
	}
}

// A proxy with no mock manager has no learn step to hold: a hold is
// refused, a release is not (it releases the tracker's), and a session's
// start releases a hold (Proxy.Mock) as it takes an ended learn step back up.
func TestAProxyHoldsItsLearnStepOnlyWithAMockManager(t *testing.T) {
	p := &Proxy{learnTracker: learn.NewTracker()}
	if err := p.SetLearnHeld(true); err == nil {
		t.Fatal("a proxy with no mock manager held its learn step")
	}
	p.learnTracker.SetLearnHeld(true)
	if err := p.SetLearnHeld(false); err != nil || p.learnTracker.LearnHeld() {
		t.Fatalf("a release with no mock manager: %v, tracker still held %v", err, p.learnTracker.LearnHeld())
	}

	// A session's start: a hold the previous session's test left is
	// released on the mock store and on the tracker both.
	t.Setenv(provenswitch.Env, "1")
	p = New(zap.NewNop(), nil, &config.Config{})
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	p.setMockManager(mm)
	if err := p.SetLearnHeld(true); err != nil {
		t.Fatal(err)
	}
	if !mm.LearnHeld() || !p.learnTracker.LearnHeld() {
		t.Fatal("holding the learn step did not hold it")
	}
	_ = p.Mock(context.Background(), models.OutgoingOptions{Learn: true})
	if mm.LearnHeld() || p.learnTracker.LearnHeld() {
		t.Fatalf("a session's start left the previous session's hold: mock store held %v, tracker held %v", mm.LearnHeld(), p.learnTracker.LearnHeld())
	}
}

// Under proven routing record's full round answers a call the open test's
// view does not with another test's recording that call is exactly, which
// the test has yet to consume (the pairing, which claims it). A later replay
// pairs nothing, so it never answers that call. While a test that keeps its
// recording from an earlier record is open the learn step is held, and the
// pairing pairs nothing: the kept test's call misses as in the later replay,
// and the other test's recording stays unclaimed for that test. Paired, the
// kept test would pass only in the record, a false green that changes no
// value, so the classification's hold alone does not refuse it. Unheld (a
// fresh test) the same call is paired, as before.
func TestAHeldLearnStepPairsNoCallWithAnotherTestsRecording(t *testing.T) {
	now := time.Now()
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "a fresh test pairs", true: "a kept test pairs nothing"}[held], func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			mm := NewMockManager(nil, nil, zap.NewNop())
			mm.SetMocksWithWindow([]*models.Mock{
				r3Mock("mock-own", now.Add(100*time.Millisecond), "GET", "/status", "", `{"up":true}`),
				r3Mock("mock-x", now.Add(1500*time.Millisecond), "POST", "/orders", `{"item":"apple"}`, `{"order":1}`),
			}, nil, models.BaseTime, now.Add(time.Hour))
			mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
			mm.SetPairing(true)
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			if held {
				if err := p.SetLearnHeld(true); err != nil {
					t.Fatal(err)
				}
			}
			p.ContinueTestErrorCapture()
			mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own")})
			code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, roundPost(`{"item":"apple"}`))
			claimed := mm.exact.claimed.has(mm.StagingEpoch(), "mock-x")
			if !held {
				if code != http.StatusOK || got != `{"order":1}` || !claimed {
					t.Fatalf("precondition: a fresh test's call that is exactly mock-x got %d %s, mock-x claimed %v; want it paired", code, got, claimed)
				}
				return
			}
			if code == http.StatusOK || claimed {
				t.Fatalf("FALSE GREEN: the kept test's call that is exactly another test's mock-x got %d %s, mock-x claimed %v; held, the round pairs nothing, as a later replay", code, got, claimed)
			}
		})
	}
}
