package manager

// NoteTLSRelayedConn counts a connection to the app port port whose client
// opened with a TLS handshake, which the ingress relayed to the app's own TLS
// without reading it (the incoming package's relayTLS): its calls are
// encrypted, so none of them was recorded as a test case. It counts once
// however many calls the connection carried, as the relay starts, so a
// connection kept open past its recording's end is that recording's. It
// returns whether it is the recording session's first to port
// (StartTLSRelayedSession), which the ingress says at WARN as it comes, the
// rest at Debug. The recording's summary and its stream's end marker report
// the session's count (routes/record.go), so every session that lost calls
// this way says so, and a client attached to an agent it did not start says
// it too. Nil-safe: a nil manager counts nothing and calls each the first.
func (m *SyncMockManager) NoteTLSRelayedConn(port uint16) (first bool) {
	if m == nil {
		return true
	}
	m.tlsRelayedMu.Lock()
	defer m.tlsRelayedMu.Unlock()
	m.tlsRelayedConns.Add(1)
	if _, said := m.tlsRelayedSaid[port]; said {
		return false
	}
	if m.tlsRelayedSaid == nil {
		m.tlsRelayedSaid = map[uint16]struct{}{}
	}
	m.tlsRelayedSaid[port] = struct{}{}
	return true
}

// StartTLSRelayedSession starts a recording session's count of the TLS
// connections relayed unread (NoteTLSRelayedConn): it returns the count so
// far, so the session's are what TLSRelayedConns grows by from here, and
// forgets the ports said, so the session's first TLS connection to each app
// port is said at WARN as it comes, whatever an earlier session said. A
// connection counted after it is the session's, and judged against the
// session's ports. Called as a recording's incoming stream opens
// (routes/record.go). Nil-safe.
func (m *SyncMockManager) StartTLSRelayedSession() (before uint64) {
	if m == nil {
		return 0
	}
	m.tlsRelayedMu.Lock()
	defer m.tlsRelayedMu.Unlock()
	m.tlsRelayedSaid = nil
	return m.tlsRelayedConns.Load()
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
