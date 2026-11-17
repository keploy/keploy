package proxy

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// HasUnservedRecording implements integrations.UnservedRecordingFinder: the
// recordings of kind the trees hold (stagedOfKind), then those the agent
// keeps on disk (SetRecordedOfKind), each once by name, until match accepts
// one that has answered no request this session (answeredIn: spent in the
// view in force, or servedThisSession: what every consume door counts, and
// under exact serving each take from the step that settles it, before its
// count; the session's reset empties both). A recording on disk that
// answered is passed over undecoded; the others are decoded one at a time,
// and kept by no one. One on disk the trees do not hold is no recording of
// the staging, which no view says is spent: servedThisSession alone reads it.
func (m *MockManager) HasUnservedRecording(kind models.Kind, match func(*models.Mock) bool) (bool, error) {
	if match == nil {
		return false, nil
	}
	// The view is read before any note of a take (answeredIn).
	v := m.exactView()
	seen := map[string]bool{}
	for _, mk := range m.stagedOfKind(kind) {
		seen[mk.Name] = true
		if !m.answeredIn(v, mk.Name) && match(mk) {
			return true, nil
		}
	}
	m.exact.ofKind.mu.Lock()
	src := m.exact.ofKind.src
	m.exact.ofKind.mu.Unlock()
	if src == nil {
		return false, nil
	}
	found := false
	var loadErr error
	err := src(kind, func(name string, load func() (*models.Mock, error)) bool {
		if seen[name] || m.servedThisSession(name) {
			return true
		}
		mk, err := load()
		if err != nil {
			loadErr = fmt.Errorf("read the recording %s kept on disk: %w", name, err)
			return false
		}
		if mk != nil && match(mk) {
			found = true
			return false
		}
		return true
	})
	if found {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, loadErr
}

// answeredIn reports whether the staged mock named name answered a request
// this session, read against v, the view in force when the caller's read
// began: spent in v (spentNow, which MockSpent reads), or counted or taken
// (servedThisSession). A take marks the recording spent in its view a moment
// before it notes it taken (takeLocked), both in the one step that holds the
// round lock, and a caller that read it spent (a parser's index of what the
// view shows, as a Redis held pop's bound check reads it) and then asks
// must find it answered, never unserved, or the caller waits for a window
// change no test may bring. Read in this order the two cannot both miss
// it: v still says it is spent, or the mark was given back, or v was read
// once the view that holds it was replaced, each a change that takes the
// round lock, so the step that took it, and its note, ended first.
func (m *MockManager) answeredIn(v *exactView, name string) bool {
	if v != nil && v.spentNow(name) {
		return true
	}
	if f := m.exact.round.onRead; f != nil {
		f("answered " + name)
	}
	return m.servedThisSession(name)
}

// servedThisSession reports whether the mock named name answered a request
// this session: counted (servedCount), or taken holding the round lock
// (noteTakenLocked), which a take is before its count.
func (m *MockManager) servedThisSession(name string) bool {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	return m.servedCount[name] > 0 || m.takenThisSession[name]
}

var _ integrations.UnservedRecordingFinder = (*MockManager)(nil)
