package manager

// NoteUnnamedCaller counts a call to a listener of the run's own process tree
// whose caller the agent could not name in its PID namespace (the proxy's
// callerOf): it may be a test's call to the app the test started, and it was
// passed to the listener without being recorded as a test case, since the
// agent cannot tell whether a test made it. The proxy warns of the first as it
// comes; the recording's summary and its stream's end marker report the
// session's count (routes/record.go), so a client attached to an agent it did
// not start says it too. Nil-safe.
func (m *SyncMockManager) NoteUnnamedCaller() {
	if m == nil {
		return
	}
	m.unnamedCallers.Add(1)
}

// UnnamedCallers returns how many calls NoteUnnamedCaller has counted over the
// manager's life. The count only grows, so what it grew by over a recording
// session is that session's. Nil-safe.
func (m *SyncMockManager) UnnamedCallers() uint64 {
	if m == nil {
		return 0
	}
	return m.unnamedCallers.Load()
}
