package recorder

// This file previously contained handleClientQueries, handleQueryResponse,
// handleTextResultSet, handleBinaryResultSet, and handlePreparedStmtResponse.
//
// All of that logic has been replaced by the "Capture & Defer" architecture:
//
//   - Packet framing is now done by the reassembler (reassembler.go) which
//     reads raw bytes from capture channels and groups them into
//     request-response pairs using byte-level protocol knowledge.
//
//   - Full packet decoding is done by ProcessRawMocksV2 (packet_worker.go)
//     in a background goroutine, completely decoupled from forwarding.
//
// See capture.go for the architecture overview.
