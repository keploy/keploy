package agent

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// laxSessionMock is stageMock as an HTTP recorder that stated no lifetime
// wrote it, "mocks"-tagged, after DeriveLifetime's lax rule (pkg/models
// lifetime.go, KEPLOY_STRICT_MOCK_WINDOW unset) classified it: a session mock
// served as a cursor that saturates on its last recording. It is set as the
// rule sets it, so the row does not depend on this process's environment.
func laxSessionMock(name, path string, at time.Time) *models.Mock {
	m := stageMock(name, path, at)
	m.TestModeInfo = models.TestModeInfo{
		Lifetime:        models.LifetimeSession,
		Consume:         models.ConsumeCursorSaturate,
		LifetimeDerived: true,
	}
	return m
}

// A recording made before the HTTP recorder stated lifetimes, replayed with
// the default window mode: its "mocks"-tagged HTTP calls are session mocks.
// The pool with no test begun answers GET /q from the reusable tier, the
// first recording of the cursor first. A begin without a pid stages the
// test's mapping; a session mock is no test's own, so the begin leaves it
// where the window staging puts it, in the reusable tier, and the call is
// answered as with no begin. Before, the begin staged mock-b in the per-test
// tier, where no claim of a session mock looks, and the matcher retried the
// claim forever (99% CPU, no answer).
//
// The calls wait for the answer or the parser's return, never a timer: a hang
// ends in go test's own -timeout, which names this row.
func TestAPidlessBeginServesALaxSessionMockAsWithNoBegin(t *testing.T) {
	pool := func() ([]*models.Mock, []*models.Mock, map[string][]string) {
		return []*models.Mock{laxSessionMock("mock-a", "/q", stageT0), laxSessionMock("mock-b", "/q", stageT0.Add(3*time.Second))},
			[]*models.Mock{stageSessionMock("cfg", "/cfg")},
			map[string][]string{"TestA": {"mock-a"}, "TestB": {"mock-b"}}
	}
	served := func(t *testing.T, begin bool) []string {
		per, session, table := pool()
		a, p := newStageReplay(t, per, session, table)
		if begin {
			scopeBegin(t, a, "TestB")
			defer scopeEnd(t, a, "TestB")
		}
		var got []string
		for i := 0; i < 3; i++ {
			got = append(got, answer(t, p.mm, "/q"))
		}
		return got
	}
	want := served(t, false)
	if want[0] != "mock-a" || want[1] != "mock-b" || want[2] != "mock-b" {
		t.Fatalf("no begin: GET /q three times answered %q; want the cursor [mock-a mock-b mock-b]", want)
	}
	got := served(t, true)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pid-less begin of TestB: GET /q three times answered %q; want %q, as with no begin", got, want)
		}
	}
}
