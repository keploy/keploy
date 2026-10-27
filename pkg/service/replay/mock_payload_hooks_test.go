package replay

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// wholeSetHooks still implement the hook a replay called with a test set's
// mocks loaded whole.
type wholeSetHooks struct{ prHooks }

func (wholeSetHooks) AfterGetMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return nil
}

func runTestSetErr(t *testing.T, h *prRun) (models.TestSetStatus, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	h.cancel = cancel
	return h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
}

// A set's mocks are no longer loaded whole, so hooks that still change them
// whole (AfterGetMocks) would no longer be called: a decryption among them
// would silently stop, and the agent be sent ciphertext. Such hooks fail the
// set, before the agent is sent anything, saying what to implement instead.
func TestRunTestSetRefusesHooksThatChangeTheWholeSet(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	useYAMLMocks(t, h, sessionHTTPMock("http://orders.internal/v1/orders"))
	h.replayer.hookImpl = wholeSetHooks{}

	status, err := runTestSetErr(t, h)
	if !errors.Is(err, errWholeSetMockMutator) {
		t.Fatalf("RunTestSet returned %v; want errWholeSetMockMutator", err)
	}
	if status == models.TestSetStatusPassed {
		t.Fatal("the set passed")
	}
	if h.instr.storedUnfiltered != nil || h.instr.storedFiltered != nil {
		t.Fatal("the agent was sent mocks the hooks did not see")
	}
}

// A payload hook may change a mock's payload only: one that moves the mock
// to another pool fails the set, as the mock is read back to be sent, rather
// than leave it in the pool the agent was told it is in.
func TestRunTestSetFailsAPayloadHookThatMovesAMock(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	useYAMLMocks(t, h, sessionHTTPMock("http://orders.internal/v1/orders"))
	h.replayer.hookImpl = mutatingHooks{
		prHooks: prHooks{},
		mutate: func(m *models.Mock) {
			if m.Kind == models.HTTP {
				m.Spec.Metadata["type"] = "mocks"
			}
		},
	}

	status, err := runTestSetErr(t, h)
	if !errors.Is(err, models.ErrPayloadMutatorChangedMock) {
		t.Fatalf("RunTestSet returned %v; want ErrPayloadMutatorChangedMock", err)
	}
	if status == models.TestSetStatusPassed {
		t.Fatal("the set passed")
	}
}

// Each mock goes through the hook once per send, as its payload is read back:
// the set is never mutated whole, and a mock the hook saw is one the agent got.
func TestRunTestSetChangesEachPayloadAsItIsSent(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	useYAMLMocks(t, h, sessionHTTPMock("http://orders.internal/v1/orders"), sessionHTTPMock("http://orders.internal/v1/items"))
	seen := map[string]int{}
	h.replayer.hookImpl = mutatingHooks{
		mutate: func(m *models.Mock) {
			seen[m.Name]++
			if m.Spec.HTTPResp != nil {
				m.Spec.HTTPResp.Body = `{"ok":true}`
			}
		},
	}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("the run reported %q; want PASSED", status)
	}
	got := map[string]bool{}
	for _, m := range append(append([]*models.Mock(nil), h.instr.storedFiltered...), h.instr.storedUnfiltered...) {
		got[m.Name] = true
		if m.PayloadOnDisk() || (m.Kind == models.HTTP && m.Spec.HTTPReq == nil) {
			t.Fatalf("the agent was sent mock %s without its payload", m.Name)
		}
	}
	for name := range got {
		if seen[name] != 1 {
			t.Fatalf("the hook saw mock %s %d times; want once, as it was sent", name, seen[name])
		}
	}
	if len(seen) != len(got) {
		t.Fatalf("the hook saw %v; the agent got %v", seen, got)
	}
}
