package replay

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// endReads is the harness's agent and hooks, noting in order each read the
// replay marks as an end: a read of the misses as "misses <test>" for the
// test whose end it names (none: "misses"), or "misses set end", and a read
// of what was consumed marked as the set's end as "consumed set end".
type endReads struct {
	*prInstr
	mu    sync.Mutex
	reads []string
}

func (r *endReads) note(what string) {
	r.mu.Lock()
	r.reads = append(r.reads, what)
	r.mu.Unlock()
}

func (r *endReads) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	switch test, ok := models.TestEndRead(ctx); {
	case models.SetEndRead(ctx):
		r.note("misses set end")
	case ok:
		r.note("misses " + test)
	default:
		r.note("misses")
	}
	return r.prInstr.GetMockErrors(ctx)
}

type endReadHooks struct {
	prHooks
	reads *endReads
}

func (h endReadHooks) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if models.SetEndRead(ctx) {
		h.reads.note("consumed set end")
	}
	return h.prHooks.GetConsumedMocks(ctx)
}

// namedReads notes the test each read of the misses names ("" for none).
type namedReads struct {
	Instrumentation
	mu    sync.Mutex
	tests []string
}

func (r *namedReads) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	test, _ := models.TestEndRead(ctx)
	r.mu.Lock()
	r.tests = append(r.tests, test)
	r.mu.Unlock()
	return r.Instrumentation.GetMockErrors(ctx)
}

// A streamed test's read of its misses names it too: it is made on a path of
// its own, after the stream is compared.
func TestRunTestSet_AStreamedTestsReadNamesItsTest(t *testing.T) {
	const line = `{"n":1}`
	app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, line+"\n")
		})
	})
	h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{
		tmplCase(1, app.addr, "GET", "/events", nil, "", models.HTTPResp{
			StatusCode: 200, StatusMessage: "OK",
			Header:     map[string]string{"Content-Type": "application/x-ndjson"},
			Body:       line,
			StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: line}}}},
		}),
	}, nil)
	reads := &namedReads{Instrumentation: h.r.instrumentation}
	h.r.instrumentation = reads
	h.run(t)
	if len(app.sent("GET", "/events")) != 1 {
		t.Fatal("precondition: the streamed test was not sent")
	}
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if !slices.Contains(reads.tests, "test-1") {
		t.Fatalf("the replay's reads named %q, want test-1", reads.tests)
	}
}

// A test's early exit reads its misses through attachMockErrors: that read
// names the test too.
func TestAttachMockErrors_NamesItsTest(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	reads := &namedReads{Instrumentation: h.instr}
	h.replayer.instrumentation = reads
	h.replayer.attachMockErrors(context.Background(), "test-set-0", "test-9", &models.TestResult{})
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if !slices.Equal(reads.tests, []string{"test-9"}) {
		t.Fatalf("the read named %q, want test-9", reads.tests)
	}
}

// Each test's read of its misses names the test whose end it is, with the
// proven switch on or off, so the agent moves the run's place to that end
// first. The set's end is read under the switch as its last read of the
// misses, and with the switch off as its read of what was consumed after
// the set's last test, made only where the run's mocks stand in for its
// dependencies.
func TestRunTestSet_EachTestsReadNamesItsTestAndTheSetsEndIsMarked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proven  bool
		mocking bool
		want    []string
	}{
		{"switch on", true, true, []string{"misses test-1", "misses test-2", "misses set end"}},
		{"switch off", false, true, []string{"misses test-1", "misses test-2", "consumed set end"}},
		{"switch off, no mocks", false, false, []string{"misses test-1", "misses test-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.proven)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.Mocking = tc.mocking
			reads := &endReads{prInstr: h.instr}
			h.replayer.instrumentation = reads
			h.replayer.hookImpl = endReadHooks{prHooks: h.replayer.hookImpl.(prHooks), reads: reads}

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s, want PASSED", status)
			}
			reads.mu.Lock()
			defer reads.mu.Unlock()
			if !slices.Equal(reads.reads, tc.want) {
				t.Fatalf("the replay's reads %q, want %q", reads.reads, tc.want)
			}
		})
	}
}
