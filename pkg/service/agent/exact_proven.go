package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// Ownership and exact consumption under the proven switch (design v14 §5.2,
// S12): the agent drives the proxy's exact serving (coreAgent.ExactServer),
// the one path that decides what a replay's parsers may answer with.
//
//   - A test owns exactly the test mocks its mapping lists: the data-plane
//     mocks among them. A test the mapping omits, or lists with none, owns
//     none, and any data-plane call it makes is a miss. A mapped
//     protocol-phase mock (an old mapping that listed a handshake) is not
//     owned: it stays in the pool, which serves it to every test.
//   - While a test is open, each of its test mocks answers at most one
//     request, in recorded order; one more identical request is a miss
//     (models.MissExtraCall). Each begin starts the test's consumption
//     afresh, so a retry replays from its first recorded call.
//   - While no test is open, the current key's pool slice answers, with
//     reuse or cursor-saturate as recorded. The stateful cursors start afresh
//     with the session and are each key's own.
//   - At its end, each test mock the test did not consume is evidence.
//
// The replay's mocks are staged on the proxy once per session, whole; exact
// serving narrows every read to what it shows, so nothing staged widens what
// a test is served.

// exactServer is the proxy's exact serving; an agent whose proxy has none
// cannot judge a test under the switch.
func (a *Agent) exactServer() (coreAgent.ExactServer, error) {
	es, ok := a.Proxy.(coreAgent.ExactServer)
	if !ok || a.Proxy == nil {
		return nil, errors.New("this keploy agent's proxy cannot serve each test exactly its own mocks")
	}
	return es, nil
}

// servePoolExactly serves the current claim's key's pool slice (before any
// claim, models.PoolKeyNone's) while no test is open. It stages the replay's
// mocks on the proxy the first time in a session, after exact serving is on,
// so no staged mock is ever served outside it. Until the CLI sent the pool
// slices, the slice is empty: no data-plane pool mock answers.
func (a *Agent) servePoolExactly(ctx context.Context) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	a.provenMu.Lock()
	key, slice, staged := a.proven.key, a.proven.pool, a.proven.staged
	a.provenMu.Unlock()
	if key == "" {
		key = models.PoolKeyNone
	}
	if err := es.ServeExactPool(key, append([]string(nil), slice[key]...)); err != nil {
		return err
	}
	if staged {
		return nil
	}
	staged, err = a.stageForExactServing(ctx)
	if err != nil || !staged {
		return err
	}
	a.provenMu.Lock()
	a.proven.staged = true
	a.provenMu.Unlock()
	return nil
}

// stageForExactServing stages every mock of the replay on the proxy, once:
// exact serving decides what each read shows. A new session's stateful
// cursors start afresh (two rounds never share one). It reports whether it
// staged: with no recording stored yet there is nothing to stage.
func (a *Agent) stageForExactServing(ctx context.Context) (bool, error) {
	if _, stored := a.clientMocks.Load(uint64(0)); !stored {
		// Nothing to stage yet: the CLI stores the recording before it
		// sends the table, and the next serve stages it.
		return false, nil
	}
	if r, ok := a.Proxy.(coreAgent.StatefulCursorResetter); ok {
		r.ResetStatefulCursors()
	}
	if err := a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}); err != nil {
		return false, fmt.Errorf("stage the recording's mocks: %w", err)
	}
	return true, nil
}

// serveTestExactly opens test id under exact serving: its test mocks, the
// data-plane mocks of its mapping, in recorded order. A mapping that names a
// mock the loaded recording does not have refuses: what the test owns cannot
// be told.
func (a *Agent) serveTestExactly(ctx context.Context, id string) error {
	es, err := a.exactServer()
	if err != nil {
		return err
	}
	a.provenMu.Lock()
	staged := a.proven.staged
	a.provenMu.Unlock()
	if !staged {
		// A begin before the CLI sent its table: nothing is staged, and the
		// pool serves nothing; the test owns what its mapping (none yet)
		// lists.
		if err := a.servePoolExactly(ctx); err != nil {
			return err
		}
	}
	a.scopeMu.Lock()
	names := append([]string(nil), a.scopeTable[id]...)
	a.scopeMu.Unlock()
	var owned []*models.Mock
	if len(names) > 0 {
		mocks, err := a.mocksByName(names)
		if err != nil {
			return err
		}
		owned = testMocks(names, mocks)
	}
	return es.ServeExactTest(id, owned)
}

// testMocks is the test mocks among a test's mapped names: the data-plane
// mocks, each once, in recorded order (by request time; a mapping lists a
// test's mocks in recorded order, which keeps ties, and an undated mock
// before the dated ones).
func testMocks(names []string, byName map[string]*models.Mock) []*models.Mock {
	out := make([]*models.Mock, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		mk := byName[n]
		if mk == nil || seen[n] || mk.IsProtocolPhase() {
			continue
		}
		seen[n] = true
		out = append(out, mk)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].Spec.ReqTimestampMock, out[j].Spec.ReqTimestampMock
		if ti.IsZero() || tj.IsZero() {
			return ti.IsZero() && !tj.IsZero()
		}
		return ti.Before(tj)
	})
	return out
}
