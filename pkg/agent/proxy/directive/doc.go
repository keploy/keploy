// Package directive defines the parser → proxy control protocol.
//
// Parsers run against read-only [fakeconn.FakeConn] streams and
// cannot touch real sockets. To request mid-stream operations that
// require real-socket access (notably mid-stream TLS upgrades for
// Postgres SSLRequest and MySQL CLIENT_SSL), parsers send a
// [Directive] on a channel owned by the [supervisor], which forwards
// the request to the [relay], executes it on the real sockets, and
// returns a [DirectiveAck] to the parser.
//
// The choreography keeps parsers ignorant of TLS state, socket
// handles, and relay timing. Every parser uses the same directive
// vocabulary for mid-stream operations, so TLS failure handling,
// timeouts, and telemetry live in one place rather than being
// replicated in every parser.
//
// See PLAN.md at the repository root for the end-to-end flow.
package directive
