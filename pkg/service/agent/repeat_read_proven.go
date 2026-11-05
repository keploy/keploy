package agent

import (
	"context"
	"slices"

	"go.keploy.io/server/v3/pkg/models"
)

// SetScopeRepeatReads installs, for a replay, each test's repeated reads
// (models.ScopeTableReq.RepeatReads), by test ID: a set's replace the
// previous set's, and none clears them.
func (a *Agent) SetScopeRepeatReads(_ context.Context, reads map[string][]models.RepeatRead) error {
	var held map[string][]models.RepeatRead
	for test, rs := range reads {
		for _, r := range rs {
			if held == nil {
				held = make(map[string][]models.RepeatRead, len(reads))
			}
			held[test] = append(held[test], models.RepeatRead{Name: r.Name, Mocks: slices.Clone(r.Mocks)})
		}
	}
	a.scopeMu.Lock()
	a.testRepeats = held
	a.scopeMu.Unlock()
	return nil
}
