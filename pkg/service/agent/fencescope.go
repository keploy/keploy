package agent

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Agent side of the pending-write fence (integrations/http/fence.go).
//
// A per-test read is consumed in recorded order. When the app asks for it more
// often than the recording did, the next copy in line can be one recorded AFTER
// a write this test has not performed yet: a reload after "request changes" was
// handed the branch as it looked after the NEXT "request review"
// (branch-review-flow:172), and the page moved on to a state the test had not
// reached. The proxy can only refuse those copies if it knows which mocks are
// this test's own writes, so the agent tags them when it stages the test.

// setFenceScope records the test scope being staged and the names it owns.
func (a *Agent) setFenceScope(scope string, own []string) {
	set := make(map[string]struct{}, len(own))
	for _, n := range own {
		set[n] = struct{}{}
	}
	a.fenceMu.Lock()
	a.fenceScope, a.fenceOwn, a.fenceSetup = scope, set, ""
	a.fenceMu.Unlock()
}

// clearFenceScope drops every fence tag source: nothing staged is a test or a
// spec's setup. A setup pool is staged with setFenceSetup (setupslot.go).
func (a *Agent) clearFenceScope() {
	a.fenceMu.Lock()
	a.fenceScope, a.fenceOwn, a.fenceSetup = "", nil, ""
	a.fenceMu.Unlock()
}

// markFenceScope tags every mock staged with a test with the test's scope name,
// the test's own GraphQL mutations that are not spec-shared as fence writes,
// and every other
// per-test copy recorded AFTER the test's own last recorded mock as a later
// copy. Every other mock is cleared, so a tag can never outlive its test.
//
// LATER COPIES. A test also sees its spec's suite recording, which holds one
// copy of the setup traffic per beforeAll. When a test asks for something more
// often than its own copies cover, it used to fall through to those suite
// copies and use up the ones recorded for a LATER describe's beforeAll, which
// then found nothing. Measured on filtering.spec.ts: two tests consumed the
// main suite-list copies recorded for the second and third beforeAll, and the
// third beforeAll fell back to an empty list. A copy captured after the test's
// own recording ended cannot belong to the test, so the proxy holds it back
// (integrations/http/fence.go).
func (a *Agent) markFenceScope(mocks []*models.Mock) {
	a.fenceMu.RLock()
	scope, own, setup := a.fenceScope, a.fenceOwn, a.fenceSetup
	pos := a.specPos[setup]
	a.fenceMu.RUnlock()
	var ownEnd time.Time
	for _, m := range mocks {
		if m == nil {
			continue
		}
		m.TestModeInfo.FenceScope, m.TestModeInfo.FenceWrite, m.TestModeInfo.FenceLater = "", false, false
		if _, ok := own[m.Name]; ok && m.Spec.ReqTimestampMock.After(ownEnd) {
			ownEnd = m.Spec.ReqTimestampMock
		}
	}
	if setup != "" {
		a.markSetupLater(mocks, setup, pos)
		return
	}
	if scope == "" {
		return
	}
	writes, later := 0, 0
	for _, m := range mocks {
		if m == nil {
			continue
		}
		// Every mock staged with the test carries its scope, not just its own:
		// the proxy reads the staged test from the pool, and a test that has used
		// up its own copies must still be recognised (repeat-last needs it).
		m.TestModeInfo.FenceScope = scope
		if _, ok := own[m.Name]; ok {
			if !m.TestModeInfo.SharedInSpec && isGraphQLMutation(m) {
				m.TestModeInfo.FenceWrite = true
				writes++
			}
			continue
		}
		if !ownEnd.IsZero() && !m.TestModeInfo.SharedInSpec && m.Kind == models.HTTP &&
			m.Spec.ReqTimestampMock.After(ownEnd) {
			m.TestModeInfo.FenceLater = true
			later++
		}
	}
	a.logger.Info("KPFENCE scope staged", zap.String("scope", scope), zap.Int("own_writes", writes), zap.Int("later_copies", later))
}
