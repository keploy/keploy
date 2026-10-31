package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ownedPoolRun stages, under the background rules, four pool recordings
// on /pool/0 and /pool/1 (mock-pool-0 to -3, the even ones on /pool/0), one
// of them (mock-pool-2) in the per-test tier when tier says so and the rest
// in the session tier, and test-0's own recording mock-own-0-0 of /pool/0 in
// its recorded window, and serves exactly with the pool between tests.
func ownedPoolRun(t *testing.T, tier string, lg *zap.Logger) (*Proxy, *MockManager, []string) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	var staged, session []*models.Mock
	var poolNames []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("mock-pool-%d", i)
		mk := r3Mock(name, now.Add(time.Duration(i)*time.Microsecond), "GET", fmt.Sprintf("/pool/%d", i%2), "", fmt.Sprintf(`{"pool":%d}`, i))
		if tier == "perTest" && i == 2 {
			staged = append(staged, mk)
		} else {
			session = append(session, mk)
		}
		poolNames = append(poolNames, name)
	}
	start := now.Add(time.Second)
	staged = append(staged, r3Mock("mock-own-0-0", start.Add(100*time.Millisecond), "GET", "/pool/0", "", `{"own":"0-0"}`))
	mm := NewMockManager(nil, nil, lg)
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow(staged, session, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	return p, mm, poolNames
}

// callDeadline ends a call whose parser never answers, so a parser that
// reads the store without end fails the row that made the call rather than
// the package's timeout. The rows decide by what the call answered and how
// often it read the store, never by how long it took.
const callDeadline = time.Minute

// sendWithin is one call of request through h, ended at callDeadline: the
// status and body the app got, or the error that ended its read (the parser
// closing the connection on a miss, or the deadline).
func sendWithin(h integrations.Integrations, p *Proxy, mm *MockManager, request string) (int, string, error) {
	return callWithin(h, p, mm, request, true)
}

// answerWithin is sendWithin that returns once the app has its answer,
// without waiting for the parser to see the app hang up: the parser's next
// read of the closed connection retries an empty read a few times before it
// believes the close (util.ReadBytes), which a walk of hundreds of calls
// would wait out on every call.
func answerWithin(h integrations.Integrations, p *Proxy, mm *MockManager, request string) (int, string, error) {
	return callWithin(h, p, mm, request, false)
}

// callWithin is one call of request through h, ended at callDeadline, that
// returns once the parser returned too where untilParserEnds.
func callWithin(h integrations.Integrations, p *Proxy, mm *MockManager, request string, untilParserEnds bool) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	defer cancel()
	app, agentEnd := net.Pipe()
	// The deadline ends the app's read too, whatever the parser does.
	stop := context.AfterFunc(ctx, func() { _ = app.Close() })
	defer stop()
	ctx = models.WithMockMismatchReporter(ctx, p.sendMockNotFoundError)
	ctx = learn.WithTracker(ctx, p.learnTracker)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	// The app hangs up after its answer, and the parser, which waits for the
	// connection's next request, returns: the next call reads the manager
	// only once this one is over.
	defer func() {
		_ = app.Close()
		if untilParserEnds {
			<-done
		}
	}()
	if _, err := app.Write([]byte(request)); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", fmt.Errorf("no answer within %v: %w", callDeadline, ctx.Err())
		}
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), err
}

// A test that owns one of the pool's recordings (in either tier), whose own
// recordings of a path have all answered, and that calls the path once more,
// gets a miss that says it is one call more than the test recorded
// (models.MissExtraCall, recorded twice), after reading the store a few
// times: never the pool's other recording of the path, which the background
// rules never hand a call one more than the open test's own recordings of it
// (a duplicate of a recorded call would pass, answered by a call the
// application made between tests), and never a read of the pool's copy of its
// own recording, which the view refuses to hand out again, and so never the
// same read without end. The pool's recording the miss left is the
// background's still: it answers the path's call between tests. The HTTP
// parser keeps its indexes of the pool from an earlier test (one that owns
// none of it), so the call reads through them.
func TestACallPastWhatATestRecordedMissesWhenTheTestOwnsAPoolRecording(t *testing.T) {
	for _, tier := range []string{"session", "perTest"} {
		t.Run(tier, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			lg := zap.New(core)
			p, mm, poolNames := ownedPoolRun(t, tier, lg)
			h := httpint.New(lg)
			mm.ServeExactTest("test-a", nil)
			p.ContinueTestErrorCapture()
			for i, want := range []string{`{"pool":1}`, `{"pool":3}`} {
				if code, body, err := sendWithin(h, p, mm, httpGet("/pool/1")); err != nil || code != http.StatusOK || body != want {
					t.Fatalf("test-a's call %d of /pool/1 got %d %s %v; want %s", i, code, body, err, want)
				}
			}
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0-0"), roundMock(mm, "mock-pool-2")})
			p.ContinueTestErrorCapture()
			// Each call reads the store a few times at most; a parser that
			// reads one refused recording again and again reads it tens of
			// thousands of times before the deadline.
			const maxReads = 8
			for i, want := range []string{`{"own":"0-0"}`, `{"pool":2}`, ""} {
				before := logs.FilterMessage("mocks under consideration for match function").Len()
				code, body, err := sendWithin(h, p, mm, httpGet("/pool/0"))
				reads := logs.FilterMessage("mocks under consideration for match function").Len() - before
				t.Logf("call %d of /pool/0: %d %s err=%v after %d reads", i, code, body, err, reads)
				if reads > maxReads {
					t.Fatalf("call %d of /pool/0 read the store %d times: a recording the read shows is one the view refuses", i, reads)
				}
				if want != "" {
					if err != nil || code != http.StatusOK || body != want {
						t.Fatalf("call %d of /pool/0 got %d %s %v; want %s", i, code, body, err, want)
					}
					continue
				}
				// One call more than the test recorded: a miss, whatever
				// the pool holds of the path.
				if err == nil && code == http.StatusOK {
					t.Fatalf("call %d of /pool/0, one more than the test recorded, was answered %s", i, body)
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("call %d of /pool/0: %v", i, err)
				}
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			// The miss says where it was made and what it was (Kind; an
			// extra call says how often the test recorded it).
			if len(misses) != 1 || misses[0].Protocol != "HTTP" || misses[0].ActualSummary != "GET /pool/0" || misses[0].Kind == "" {
				t.Fatalf("misses %+v; want one HTTP miss of GET /pool/0 that says what it was", misses)
			}
			t.Logf("the miss: kind %s, recorded %d, reason %q", misses[0].Kind, misses[0].Recorded, misses[0].Reason)
			if misses[0].Kind != models.MissExtraCall || misses[0].Recorded != 2 {
				t.Fatalf("the miss is %s, recorded %d; want %s, recorded 2 (test-0's own two recordings of the path)", misses[0].Kind, misses[0].Recorded, models.MissExtraCall)
			}
			// The pool's recording the extra call was refused stays the
			// background's: between tests it answers the path's call.
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			if code, body, err := sendWithin(h, p, mm, httpGet("/pool/0")); err != nil || code != http.StatusOK || body != `{"pool":0}` {
				t.Fatalf("the call of /pool/0 between tests got %d %s %v; want {\"pool\":0}", code, body, err)
			}
		})
	}
}

// countNamed is how many of ms are named name.
func countNamed(ms []*models.Mock, name string) int {
	n := 0
	for _, mk := range ms {
		if mk.Name == name {
			n++
		}
	}
	return n
}

// The real manager never puts one of the open test's own recordings in the
// shared part of its reads (integrations.SharedReads): with the test owning
// a pool recording, in either tier, its own and the shared part never
// overlap, the session and per-test reads hold each recording once, and once
// the recording answered as the test's it is in no read. A test that owns a
// pool recording reports a shared part of its own: SharedGeneration's View
// differs from that of the tests that own none, before and after it, so a
// parser never reads it through an index of theirs.
func TestTheManagerNeverShowsATestsOwnRecordingInTheSharedPartOfItsReads(t *testing.T) {
	for _, tier := range []string{"session", "perTest"} {
		t.Run(tier, func(t *testing.T) {
			_, mm, poolNames := ownedPoolRun(t, tier, zap.NewNop())
			view := func() uint64 {
				t.Helper()
				g, ok := mm.SharedGeneration("")
				if !ok {
					t.Fatal("no shared generation in a test")
				}
				return g.View
			}
			mm.ServeExactTest("test-a", nil)
			before := view()
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0-0"), roundMock(mm, "mock-pool-2")})
			owning := view()
			check := func(when string, wantPool2 int) {
				t.Helper()
				owned, ok := mm.OwnedUnanswered()
				pt, se, ok2 := mm.SharedMocks()
				if !ok || !ok2 {
					t.Fatalf("%s: no shared reads in a test", when)
				}
				for _, o := range owned {
					if c := countNamed(pt, o.Name) + countNamed(se, o.Name); c > 0 {
						t.Errorf("%s: the test's own %s is in the shared part %d time(s): per-test %v, session %v", when, o.Name, c, mockNames(pt), mockNames(se))
					}
				}
				for _, n := range []string{"mock-pool-2", "mock-own-0-0"} {
					if c := countNamed(pt, n) + countNamed(se, n); c > 0 {
						t.Errorf("%s: %s, the test's own, is in the shared part: per-test %v, session %v", when, n, mockNames(pt), mockNames(se))
					}
				}
				realPT, err := mm.GetPerTestMocksInWindow()
				if err != nil {
					t.Fatal(err)
				}
				realSE, err := mm.GetSessionMocks()
				if err != nil {
					t.Fatal(err)
				}
				for _, read := range []struct {
					name string
					ms   []*models.Mock
				}{{"per-test", realPT}, {"session", realSE}} {
					seen := map[string]bool{}
					for _, mk := range read.ms {
						if seen[mk.Name] {
							t.Errorf("%s: the %s read holds %s twice: %v", when, read.name, mk.Name, mockNames(read.ms))
						}
						seen[mk.Name] = true
					}
					if c := countNamed(read.ms, "mock-pool-2"); c != wantPool2 {
						t.Errorf("%s: the %s read holds mock-pool-2 %d time(s), want %d: %v", when, read.name, c, wantPool2, mockNames(read.ms))
					}
				}
			}
			check("before it answered", 1)
			if v := mm.exactConsume(roundMock(mm, "mock-pool-2"), true); v != exactServed {
				t.Fatalf("the test's own mock-pool-2 settled %v; want served", v)
			}
			check("after it answered", 0)
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			mm.ServeExactTest("test-b", nil)
			after := view()
			if owning == before || owning == after {
				t.Fatalf("SharedGeneration's View: %d before, %d in the test that owns a pool recording, %d after; want the owning test's apart", before, owning, after)
			}
		})
	}
}

// In record's full round (OutgoingOptions.Learn) too, a test that owns one
// of the pool's recordings and calls a path once more than its own
// recordings of it gets a miss: the learn step neither answers the call with
// the pool's other recording of the path nor pairs it, and the pool's
// recording still answers the path's call between tests.
func TestACallPastWhatATestRecordedMissesInRecordsFullRound(t *testing.T) {
	for _, tier := range []string{"session", "perTest"} {
		t.Run(tier, func(t *testing.T) {
			p, mm, poolNames := ownedPoolRun(t, tier, zap.NewNop())
			h := httpint.New(zap.NewNop())
			opts := models.OutgoingOptions{Learn: true}
			mm.ServeExactTest("test-a", nil)
			p.ContinueTestErrorCapture()
			for i, want := range []string{`{"pool":1}`, `{"pool":3}`} {
				if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/pool/1")); code != http.StatusOK || body != want {
					t.Fatalf("test-a's call %d of /pool/1 got %d %s; want %s", i, code, body, want)
				}
			}
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0-0"), roundMock(mm, "mock-pool-2")})
			p.ContinueTestErrorCapture()
			for i, want := range []string{`{"own":"0-0"}`, `{"pool":2}`} {
				if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/pool/0")); code != http.StatusOK || body != want {
					t.Fatalf("test-0's call %d of /pool/0 got %d %s; want %s", i, code, body, want)
				}
			}
			if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/pool/0")); code == http.StatusOK {
				t.Fatalf("test-0's call 2 of /pool/0, one more than the test recorded, was answered %s", body)
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != 1 || misses[0].ActualSummary != "GET /pool/0" || misses[0].Kind != models.MissExtraCall || misses[0].Recorded != 2 {
				t.Fatalf("misses %+v; want one extra_call miss of GET /pool/0, recorded 2", misses)
			}
			mm.ServeExactPool(models.PoolKeyNone, poolNames)
			if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/pool/0")); code != http.StatusOK || body != `{"pool":0}` {
				t.Fatalf("the call of /pool/0 between tests got %d %s; want {\"pool\":0}", code, body)
			}
		})
	}
}
