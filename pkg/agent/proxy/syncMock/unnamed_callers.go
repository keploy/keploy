package manager

// NoteUnnamedCallerConn counts a connection to a listener of the run's own
// process tree whose caller the agent could not name in its PID namespace (the
// proxy's callerOf): it may carry a test's calls to the app the test started,
// and it was passed to the listener with none of its calls recorded as a test
// case, since the agent cannot tell whether a test made them. The proxy relays
// such a connection without reading its calls, so it counts once however many
// it carries. The proxy warns of the first as it comes; the recording's summary
// and its stream's end marker report the session's count (routes/record.go),
// so a client attached to an agent it did not start says it too. Nil-safe.
func (m *SyncMockManager) NoteUnnamedCallerConn() {
	if m == nil {
		return
	}
	m.unnamedCallerConns.Add(1)
}

// UnnamedCallerConns returns how many connections NoteUnnamedCallerConn has
// counted over the manager's life. The count only grows, so what it grew by
// over a recording session is that session's. Nil-safe.
func (m *SyncMockManager) UnnamedCallerConns() uint64 {
	if m == nil {
		return 0
	}
	return m.unnamedCallerConns.Load()
}
