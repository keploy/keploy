package replay

import (
	"context"
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
