package models

// MarkOwnListenerCall marks m, on the CLI side only, as the recording of a
// call the app made to a port it listens on itself (its health check of its
// own port, a call to its own API). keploy passes such a call through to the
// app at record and at replay, so a recording a keploy before that made of
// one is not a dependency's: no test and no worker scope requires it
// (OutsideTestScopes), and its going unused fails nothing. The mark is in
// memory only, no encoder carries it: the agent, which serves the recording
// to a call it cannot tell is the app's own, never reads it.
func (m *Mock) MarkOwnListenerCall() {
	if m != nil {
		m.ownListener = true
	}
}

// OwnListenerCall reports whether m is marked as the recording of a call the
// app made to its own port (MarkOwnListenerCall).
func (m *Mock) OwnListenerCall() bool {
	return m != nil && m.ownListener
}
