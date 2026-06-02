// Package fakeconn provides read-only connection abstractions that decouple
// integration parsers from real network sockets.
//
// Role: Keploy's proxy relay owns the real net.Conn for both the client and
// destination peers. Integration parsers historically held those real
// handles, which meant a parser panic or bug could close the user's live
// TCP connection. This package defines the boundary types — Chunk and
// FakeConn — that let the relay hand already-read, timestamped byte chunks
// to parsers without exposing any writer capability. The relay remains the
// sole owner and writer of real sockets.
//
// This package is phase 1 scaffolding: it compiles and has unit tests but
// is not yet wired into any caller. See PLAN.md at the repository root for
// the full multi-phase refactor context.
package fakeconn
