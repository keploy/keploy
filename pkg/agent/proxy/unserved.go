package proxy

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// HasUnservedRecording implements integrations.UnservedRecordingFinder: the
// recordings of kind the trees hold (stagedOfKind), then those the agent
// keeps on disk (SetRecordedOfKind), each once by name, until match accepts
// one that has answered no request this session (servedCount, which every
// consume door adds to, and the session's reset empties). A recording on
// disk that answered is passed over undecoded; the others are decoded one at
// a time, and kept by no one.
func (m *MockManager) HasUnservedRecording(kind models.Kind, match func(*models.Mock) bool) (bool, error) {
	if match == nil {
		return false, nil
	}
	seen := map[string]bool{}
	for _, mk := range m.stagedOfKind(kind) {
		seen[mk.Name] = true
		if !m.servedThisSession(mk.Name) && match(mk) {
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

// servedThisSession reports whether the mock named name answered a request
// this session.
func (m *MockManager) servedThisSession(name string) bool {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	return m.servedCount[name] > 0
}

var _ integrations.UnservedRecordingFinder = (*MockManager)(nil)
