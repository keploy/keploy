package models

import "sync"

// StartupDeclarable reports whether a call of kind made in a test's window
// can be declared a call every test may make, so that a plain startup:
// recording of kind (BackgroundRecording.Startup: made before the first
// test, covered by no declared rule) is that test's own call under the
// proven switch, its miss saying how to declare it. HTTP and HTTP/2 always
// are: a keploy.yaml sessionCalls entry declares them
// (Mock.DeclaredSessionCall). Any other kind is only once a rule system that
// declares it registered it (RegisterStartupDeclarableKinds); until then a
// plain startup: recording of it is the set's background in a test's window
// too, as in v3.7.18, since its user has no way to declare the call.
func StartupDeclarable(kind Kind) bool {
	if kind == HTTP || kind == HTTP2 {
		return true
	}
	startupDeclarable.mu.RLock()
	defer startupDeclarable.mu.RUnlock()
	return startupDeclarable.kinds[kind] > 0
}

// RegisterStartupDeclarableKinds adds kinds to StartupDeclarable's: a rule
// system that lets a user declare a call of each of them (enterprise's
// startup: true rules) calls it once, at init, in every process that marks
// a set's background recordings: the agent marks them
// (agent.backgroundRecordingsOf). keploy's enterprise binary is its own
// agent (the agent it starts, natively or in its container, is the same
// binary), so a registration in its init reaches the agent too. unregister
// undoes this call alone (for tests).
func RegisterStartupDeclarableKinds(kinds ...Kind) (unregister func()) {
	startupDeclarable.mu.Lock()
	defer startupDeclarable.mu.Unlock()
	if startupDeclarable.kinds == nil {
		startupDeclarable.kinds = map[Kind]int{}
	}
	for _, k := range kinds {
		startupDeclarable.kinds[k]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			startupDeclarable.mu.Lock()
			defer startupDeclarable.mu.Unlock()
			for _, k := range kinds {
				if startupDeclarable.kinds[k]--; startupDeclarable.kinds[k] <= 0 {
					delete(startupDeclarable.kinds, k)
				}
			}
		})
	}
}

var startupDeclarable struct {
	mu    sync.RWMutex
	kinds map[Kind]int
}
