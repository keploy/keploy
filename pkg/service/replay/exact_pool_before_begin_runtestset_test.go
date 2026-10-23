package replay

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// viewInstr is the partial-run harness's agent under exact serving, telling
// in order what the replayer asks of it: each view it installs ("stage",
// "pool", "test:<name>"; a call it fails is "<what>-failed"), each test's
// capture window opened ("begin", "continue": the agent's learn tracker
// begins the test there) and each one read ("end": GetMockErrors, where the
// tracker ends it). failPool fails the pool calls it answers true for, by
// their number (1 for the set's first).
type viewInstr struct {
	*prInstr
	failPool func(n int) bool

	mu     sync.Mutex
	pools  int
	events []string
}

func (v *viewInstr) note(e string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.events = append(v.events, e)
}

func (v *viewInstr) BeginTestErrorCapture(context.Context) error {
	v.note("begin")
	return nil
}

func (v *viewInstr) ContinueTestErrorCapture(context.Context) error {
	v.note("continue")
	return nil
}

func (v *viewInstr) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	v.note("end")
	return v.prInstr.GetMockErrors(ctx)
}

func (v *viewInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	what := "other"
	switch ex := params.Exact; {
	case ex == nil:
	case ex.Stage:
		what = "stage"
	case ex.Test != "":
		what = "test:" + ex.Test
	default:
		what = "pool"
		v.mu.Lock()
		v.pools++
		n := v.pools
		v.mu.Unlock()
		if v.failPool != nil && v.failPool(n) {
			v.note("pool-failed")
			return errors.New("the agent did not take the pool call")
		}
	}
	v.note(what)
	return v.prInstr.UpdateMockParams(ctx, params)
}

func (v *viewInstr) trace() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.events...)
}

// overATestView is the first test the trace begins (its capture window
// opened, the agent's tracker beginning it) while the agent may still serve
// a test's own view: one asked for since the latest pool call it took. ""
// when every test begins over the pool.
func overATestView(events []string) string {
	view := ""
	for i, e := range events {
		switch {
		case strings.HasPrefix(e, "test:"):
			view = strings.TrimPrefix(e, "test:")
		case e == "pool" || e == "stage":
			view = ""
		case e == "begin" || e == "continue":
			if view != "" {
				return "event " + strconv.Itoa(i) + " (" + e + ") over " + view + "'s view"
			}
		}
	}
	return ""
}

// unendedTest is the first test whose capture window is read ("end", the
// agent's tracker ending it) with no pool call made since its view was
// asked for: "" when every test asks for the pool first, as the main loop
// does once its test has its answer.
func unendedTest(events []string) string {
	open := ""
	for _, e := range events {
		switch {
		case strings.HasPrefix(e, "test:"):
			open = strings.TrimPrefix(e, "test:")
		case e == "pool" || e == "pool-failed":
			open = ""
		case e == "end":
			if open != "" {
				return open
			}
		}
	}
	return ""
}

// exactViewRun is a three-test set under the switch, test-1 owning mock-a,
// its agent viewInstr.
func exactViewRun(t *testing.T, failPool func(int) bool, streamed ...string) (*prRun, *viewInstr) {
	t.Helper()
	setSwitch(t, true)
	h := newPartialRunHarness(t, 3, 0)
	pinWindows(h)
	h.streaming(streamed...)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}, {Name: "mock-boot", Kind: models.HTTP}}
	mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
	vi := &viewInstr{prInstr: h.instr, failPool: failPool}
	h.replayer.instrumentation = vi
	return h, vi
}

// startDiffers answers test case name with a stream whose start (its status)
// differs from its recording's, and then nothing of its body: the streaming
// pass judges the start alone, a judged failure.
type startDiffers struct {
	prHooks
	name string
}

func (h startDiffers) SimulateRequest(ctx context.Context, tc *models.TestCase, id string) (interface{}, error) {
	if tc.Name != h.name {
		return h.prHooks.SimulateRequest(ctx, tc, id)
	}
	return &pkg.StreamingHTTPResponse{StatusCode: 503, Header: tc.HTTPResp.Header,
		Reader: io.NopCloser(iotest.ErrReader(&pkg.AnswerStallError{Target: "app", Answered: true})), StreamConfig: pkg.DetectHTTPStreamConfig(tc, nil)}, nil
}

// A streamed test case that fails ends with the pool served, on every exit
// the streaming pass has, before its capture window is read: the agent's
// learn tracker ends it and begins the next test case only over the pool.
// Before, these exits served no pool: the next test case's tracker began
// over the failed one's view, and the store answered the next test's calls
// from that view while the tracker filed what the failed one served as
// served before the next. A value one of its recordings held was learned as
// noise onto a recording of the failed test (integrations' Postgres and
// Mongo learn steps).
func TestAFailedStreamedTestEndsWithThePoolServed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks func() TestHooks
	}{
		{name: "the request fails", hooks: func() TestHooks {
			return prHooks{simErr: map[string]error{"test-1": errors.New("connection refused")}}
		}},
		{name: "the stream fails to read", hooks: func() TestHooks {
			return prHooks{streamErr: map[string]error{"test-1": errors.New("stream cut")}}
		}},
		{name: "the stream's start is judged a failure", hooks: func() TestHooks {
			return startDiffers{name: "test-1"}
		}},
		{name: "the answer is of another type", hooks: func() TestHooks {
			return prHooks{wrongType: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, vi := exactViewRun(t, nil, "test-1", "test-2", "test-3")
			h.replayer.hookImpl = tc.hooks()
			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Fatalf("precondition: the set reported %q; want FAILED (test-1 fails)", status)
			}
			events := vi.trace()
			if !hasEvent(events, "test:test-2") {
				t.Fatalf("precondition: test-2 never opened: %v", events)
			}
			if bad := unendedTest(events); bad != "" {
				t.Errorf("%s's capture window was read with its own view still served: %v", bad, events)
			}
			if bad := overATestView(events); bad != "" {
				t.Errorf("a test began over a test's view, %s: %v", bad, events)
			}
		})
	}
}

// A pool call the agent does not take, as a test case ends, is made again
// before the next one begins: the next test case's tracker begins only over
// the pool, on the main loop and on the streaming pass. Before, the next test
// began over the last one's view.
func TestAPoolCallTheAgentDidNotTakeIsMadeAgainBeforeTheNextTestBegins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// test-1's end is the set's first pool call.
			h, vi := exactViewRun(t, func(n int) bool { return n == 1 }, tc.streamed...)
			h.run(t)
			events := vi.trace()
			if !hasEvent(events, "pool-failed") || !hasEvent(events, "test:test-3") {
				t.Fatalf("precondition: the pool call did not fail once, or the set did not reach test-3: %v", events)
			}
			if bad := overATestView(events); bad != "" {
				t.Errorf("a test began over a test's view, %s: %v", bad, events)
			}
		})
	}
}

// When the agent takes no pool call at all after a test case, the set stops
// failing closed, before the next test case begins: an internal error, never
// a pass, and no later test case is asked of the agent.
func TestASetWhosePoolTheAgentNeverTakesStopsBeforeTheNextTestBegins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, vi := exactViewRun(t, func(int) bool { return true }, tc.streamed...)
			status := h.run(t)
			events := vi.trace()
			if status != models.TestSetStatusInternalErr {
				t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusInternalErr, events)
			}
			if hasEvent(events, "test:test-2") {
				t.Errorf("test-2 was asked of the agent with test-1's view never ended: %v", events)
			}
			if bad := overATestView(events); bad != "" {
				t.Errorf("a test began over a test's view, %s: %v", bad, events)
			}
		})
	}
}

func hasEvent(events []string, e string) bool {
	for _, x := range events {
		if x == e {
			return true
		}
	}
	return false
}
