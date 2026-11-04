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

// markRepeatReads is owned with each recording a repeated read of its test
// names (reads) replaced by a copy carrying the read's name
// (models.TestModeInfo.RepeatRead), which exact serving honours when the
// recordings prove it; the store's recordings are not changed. A recording
// two reads name carries the later one's.
func markRepeatReads(owned []*models.Mock, reads []models.RepeatRead) []*models.Mock {
	if len(reads) == 0 {
		return owned
	}
	read := map[string]string{}
	for _, r := range reads {
		for _, n := range r.Mocks {
			read[n] = r.Name
		}
	}
	out := make([]*models.Mock, len(owned))
	for i, mk := range owned {
		out[i] = mk
		if mk == nil {
			continue
		}
		if r, ok := read[mk.Name]; ok {
			c := mk.ShallowCopy()
			c.TestModeInfo.RepeatRead = r
			out[i] = c
		}
	}
	return out
}
