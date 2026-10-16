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
//   - DNS;
//   - a mock its recorder stated session or connection lifetime
//     (MetadataLifetimeKey): the MySQL recorder states a connection's setup
//     (SET, SHOW VARIABLES, USE), transaction control (BEGIN, COMMIT) and a
//     prepared statement's lifecycle so, and they belong to the connection,
//     not to the test whose window they were captured in. A mock stated
//     per-test is data-plane, whatever its type tag: a Generic exchange
//     after its connection's first included.
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
	if lt, ok := statedLifetime(m); ok {
		return lt != LifetimePerTest
	}
	// The Generic recorder states each exchange's lifetime too: a
	// connection's first exchange, its preamble, session; every later one
	// per-test (generic.exchangeLifetime), and under the proven switch the
	// first too when the connection lay wholly inside one test
	// (generic.preambleHold). Its type tag stays "config", and
	// serving does not read the statement (DeriveLifetime keeps every Generic
	// mock session, as before the recorder made it), so a per-test exchange is
	// a test mock here only: owned, and judged, under the proven switch.
	if m.Kind == GENERIC && m.Spec.Metadata[MetadataLifetimeKey] == MetadataLifetimePerTest {
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
