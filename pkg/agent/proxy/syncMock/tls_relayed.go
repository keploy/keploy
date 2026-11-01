package manager

// NoteTLSRelayedConn counts a connection to the app whose client opened with
// a TLS handshake, which the ingress relayed to the app's own TLS without
// reading it (the incoming package's relayTLS): its calls are encrypted, so
// none of them was recorded as a test case. It counts once however many calls
// the connection carried. The ingress warns of the first to each app port as
// it comes; the recording's summary and its stream's end marker report the
// session's count (routes/record.go), so every session that lost calls this
// way says so, and a client attached to an agent it did not start says it
// too. Nil-safe.
func (m *SyncMockManager) NoteTLSRelayedConn() {
	if m == nil {
		return
	}
	m.tlsRelayedConns.Add(1)
}

// TLSRelayedConns returns how many connections NoteTLSRelayedConn has counted
// over the manager's life. The count only grows, so what it grew by over a
// recording session is that session's. Nil-safe.
func (m *SyncMockManager) TLSRelayedConns() uint64 {
	if m == nil {
		return 0
	}
	return m.tlsRelayedConns.Load()
}
