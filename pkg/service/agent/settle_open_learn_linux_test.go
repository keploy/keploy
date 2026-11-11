//go:build linux

package agent

import (
	"bufio"
	"context"
	"io"
	"net"
	nethttp "net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// learningProxy is openingProxy with a learn tracker whose boundaries reach
// its mock manager (MockManager.LearnBoundary), as keploy's proxy routes
// its own, and which takes each test case's request (TestRequestTaker).
type learningProxy struct {
	openingProxy
	tracker *learn.Tracker
}

func (p *learningProxy) BeginTestCaseCapture() {
	p.mm.LearnBoundary(p.tracker, (*learn.Tracker).BeginAwaiting)
}
func (p *learningProxy) ContinueTestCaseCapture() { p.BeginTestCaseCapture() }
func (p *learningProxy) TakeTestRequest(req *models.TestRequest) {
	p.tracker.GiveRequest(req)
}

// learnCall sends GET path through the real HTTP matcher's learn step
// (record's full round), its connection holding work in the proxy's
// hands (settledProxy) until it is answered; marked already, where mark is
// false (a change of the work is the settle's progress). done closes once
// it is answered, with what the app got.
func (p *learningProxy) learnCall(t *testing.T, path string, mark bool) (done chan struct{}, got *string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	app, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	prox := <-acc
	_ = ln.Close()
	t.Cleanup(func() { _ = app.Close(); _ = prox.Close() })
	ctx := learn.WithTracker(models.WithMockMismatchReporter(context.Background(), p.noteMiss), p.tracker)
	go func() {
		_ = httpparser.New(zap.NewNop()).MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Port: 18093}, p.mm, models.OutgoingOptions{Learn: true, LearnPool: true})
	}()
	if mark {
		p.set(0, 1)
	}
	done, got = make(chan struct{}), new(string)
	go func() {
		defer close(done)
		_, err := io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1:18093\r\n\r\n")
		if err != nil {
			*got = err.Error()
			return
		}
		resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			*got = err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		*got = resp.Status + " " + string(b)
		p.set(0, 0) // answered: the connection holds no work
	}()
	return done, got
}

// keploy test's begin of a test case under exact serving: its capture window
// opens (the learn tracker begins test-1, awaiting its request) before its
// mock filter, and the filter gives the request, settles every call made
// before, and only then installs test-1's view (OpenTestCase). A learn call
// read in that gap is the pool's: the application gets test-1's request
// only after the install. It is decided at once in the round between tests,
// answered from the pool's recording, and the settle completes, whether the
// call read the round before the request was given (it waits for the
// request, which the filter gives before it settles) or while the agent
// settled. A call that waited for the store to open test-1 would hold the
// install it waits for: the settle's watchdog counts its wake-ups with no
// progress (event-counted, no clock) and fails the open, and the set's loop
// breaks.
func TestKeployTestOpensATestCaseWithAPoolCallReadInItsGap(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, given := range []bool{false, true} {
		t.Run(map[bool]string{false: "the call reads the round before the request is given", true: "the call reads the round while the agent settles"}[given], func(t *testing.T) {
			wake := manualWake(t)
			prev := settleStallLimit
			settleStallLimit = 3
			t.Cleanup(func() { settleStallLimit = prev })
			a, _ := wireAgent(t, models.MODE_TEST)
			p := &learningProxy{openingProxy: openingProxy{keepingProxy: keepingProxy{servingProxy: newServingProxy()}}, tracker: learn.NewTracker()}
			t.Cleanup(p.mm.Close)
			a.Proxy = p
			ctx := context.Background()
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("pool-1", "/pool", "pooled", t0), pingMock("own-1", "/own-1", "owned", t0)}))
			p.mm.LearnBoundary(p.tracker, (*learn.Tracker).NewSet)
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: t0.Add(1 << 40),
				Exact: &models.ExactServing{Stage: true, Pool: []string{"pool-1"}},
			}))
			p.BeginTestCaseCapture()
			waitedRequest, waitedRound := make(chan struct{}), make(chan struct{})
			p.tracker.OnAwait(func() { close(waitedRequest) })
			p.mm.OnAwaitRoundMove(func() { close(waitedRound) })
			t.Cleanup(func() { p.tracker.OnAwait(nil); p.mm.OnAwaitRoundMove(nil) })
			p.inWork = make(chan struct{})
			open := func() chan error {
				opened := make(chan error, 1)
				go func() {
					opened <- a.OpenTestCase(ctx, models.MockFilterParams{
						Exact:       &models.ExactServing{Test: "test-1", Owned: []string{"own-1"}},
						TestRequest: &models.TestRequest{Test: "test-1"},
					})
				}()
				return opened
			}
			var done chan struct{}
			var got *string
			var opened chan error
			if given {
				p.set(0, 1)
				opened = open()
				<-p.inWork // the request is given, and the settle waits on the call's connection
				done, got = p.learnCall(t, "/pool", false)
			} else {
				done, got = p.learnCall(t, "/pool", true)
				select {
				case <-waitedRequest:
				case <-waitedRound:
				case <-done:
					t.Fatalf("the call read before test-1's request was given was decided without it: %s", *got)
				}
				opened = open()
			}
			select {
			case <-done:
			case <-waitedRound:
				// The call waits for test-1's view, whose install waits
				// for the call: the watchdog wakes, with no progress, to
				// its count.
				<-p.inWork
				for i := 0; i < settleStallLimit; i++ {
					wake()
				}
			}
			err := <-opened
			require.NoError(t, err, "keploy test could not open test-1 with a pool call read in its gap")
			<-done
			require.Equal(t, "200 OK pooled", *got, "the pool's call read in test-1's gap")
			require.Equal(t, 1, p.opened, "test-1's view was not installed")
		})
	}
}

// keploy test opens a test case only once every call made before it is
// answered, the pool's call that waits for the test case's request among
// them: its filter gives the request, settles, then installs. A call the
// request woke that the scheduler runs only later (a loaded host) still
// holds the open: it reads the round between tests when it runs, never
// test-1's view, and is answered from the pool, as at once. The settle
// counts its connection as holding work (the proxy's in-flight read, which
// TestACallWokenByItsTestsRequestHoldsTheSettleUntilAnswered pins on the
// real proxy). Should the settle give up on it (its watchdog wakes to its
// count with no progress), the open fails and installs nothing: keploy
// test's set stops there, its tests not run counted unverified, and the
// call, run late, is still the pool's.
func TestKeployTestHoldsAnOpenForAPoolCallItsRequestWokeLate(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, gaveUp := range []bool{false, true} {
		t.Run(map[bool]string{false: "the call is answered, then test-1 opens", true: "the settle gives up: nothing opens"}[gaveUp], func(t *testing.T) {
			w := stallWake(t)
			a, _ := wireAgent(t, models.MODE_TEST)
			p := &learningProxy{openingProxy: openingProxy{keepingProxy: keepingProxy{servingProxy: newServingProxy()}}, tracker: learn.NewTracker()}
			t.Cleanup(p.mm.Close)
			a.Proxy = p
			ctx := context.Background()
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("pool-1", "/pool", "pooled", t0), pingMock("own-1", "/own-1", "owned", t0)}))
			p.mm.LearnBoundary(p.tracker, (*learn.Tracker).NewSet)
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: t0.Add(1 << 40),
				Exact: &models.ExactServing{Stage: true, Pool: []string{"pool-1"}},
			}))
			p.BeginTestCaseCapture()
			// The call reaches the wait for test-1's request, then runs again
			// only once the row says: the request is given meanwhile.
			parked, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			p.tracker.OnAwait(func() {
				once.Do(func() {
					close(parked)
					<-release
				})
			})
			t.Cleanup(func() { p.tracker.OnAwait(nil) })
			released := false
			let := func() {
				if !released {
					released = true
					close(release)
				}
			}
			defer let()
			waitedRound := make(chan struct{})
			p.mm.OnAwaitRoundMove(func() { close(waitedRound) })
			t.Cleanup(func() { p.mm.OnAwaitRoundMove(nil) })
			p.inWork = make(chan struct{})
			done, got := p.learnCall(t, "/pool", true)
			select {
			case <-parked:
			case <-waitedRound:
				t.Fatal("the pool's call read before test-1's request waited for the store to open test-1, whose open waits for it")
			}
			openTest1 := func() error {
				return a.OpenTestCase(ctx, models.MockFilterParams{
					Exact:       &models.ExactServing{Test: "test-1", Owned: []string{"own-1"}},
					TestRequest: &models.TestRequest{Test: "test-1"},
				})
			}
			if gaveUp {
				_, err := w.run(openTest1, nil)
				require.ErrorIs(t, err, errNoProgress, "the settle gave up on the woken call")
				require.ErrorIs(t, err, models.ErrSettleGaveUp, "the agent's give-up is not said as keploy's own")
				// What the replay reports of a test case keploy's agent did
				// not open: the work its settle waited on.
				require.Contains(t, err.Error(), "nothing moved on connection 1", "the give-up does not name the connection it waited on")
				require.Equal(t, 0, p.opened, "test-1's view was installed though its open failed")
				let()
				<-done
				require.Equal(t, "200 OK pooled", *got, "the woken call, run after the open failed")
				return
			}
			opened := make(chan error, 1)
			go func() { opened <- openTest1() }()
			select {
			case <-p.inWork: // the request is given, and the settle waits on the call's connection
			case err := <-opened:
				let()
				<-done
				t.Fatalf("test-1 opened (%v) before the pool call its request woke ran; the call then got %s", err, *got)
			}
			let()
			<-done
			require.Equal(t, "200 OK pooled", *got, "the pool's call its request woke, run late")
			require.NoError(t, <-opened)
			require.Equal(t, 1, p.opened, "test-1's view was not installed")
		})
	}
}
