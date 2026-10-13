package models

// IsProtocolPhase reports whether a mock is protocol-phase rather than
// data-plane, by what was recorded and not by DeriveLifetime's tier:
//
//   - a mock its recorder tagged "config" or "connection" (handshakes, auth,
//     session settings, prepared-statement setup);
//   - a session-reusable command: a MySQL connection-alive command with an
//     input-independent answer (COM_PING, COM_STATISTICS, COM_DEBUG,
//     COM_RESET_CONNECTION), or what an out-of-tree parser declared through
//     RegisterSessionReusable;
//   - DNS.
//
// Every other mock is data-plane, whatever tier DeriveLifetime gives it: its
// kind fallback makes an untagged HTTP, SQL or Generic mock session-tier, and
// that tier says how the mock may be served, not whether a test that recorded
// it must make the call again. A connection failure is data-plane too (it is
// window-bound, never reusable).
//
// A test mock is a data-plane mock in a test's mapping: under the proven
// switch every one of them must be consumed by its test.
func (m *Mock) IsProtocolPhase() bool {
	if m == nil {
		return false
	}
	if m.Kind == DNS {
		return true
	}
	if WindowBound(m.Kind) {
		return false
	}
	switch m.Spec.Metadata["type"] {
	case "config", "connection":
		return true
	}
	if m.Kind == MySQL && mysqlIsSessionReusableCommand(m) {
		return true
	}
	return sessionReusableHooks.match(m)
}
