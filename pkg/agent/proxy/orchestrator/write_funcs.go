package orchestrator

import (
	"net"
)

// WriteFunc is a callback function for writing data.
// It replaces direct net.Conn.Write() calls to enable orchestrator-based async I/O.
type WriteFunc func(conn net.Conn, data []byte) error

// WriteFuncs holds the write functions for client and destination connections.
// WriteToClient/WriteToDestination are synchronous (block until write completes)
// and should be used for handshake/auth where strict ordering matters.
// ForwardToClient/ForwardToDestination are fire-and-forget (return immediately)
// and should be used in the command phase hot loop for pipelining performance.
type WriteFuncs struct {
	WriteToClient      WriteFunc
	WriteToDestination WriteFunc
	// Fire-and-forget variants for the command phase.
	// Data is queued and written asynchronously; errors are detected
	// by the read goroutines (EOF/connection reset).
	ForwardToClient      WriteFunc
	ForwardToDestination WriteFunc
}

// NewWriteFuncs creates WriteFuncs backed by the orchestrator's write channel.
// Sync variants (WriteToClient/WriteToDestination) block until the write completes —
// use these for handshake/auth where strict request-response ordering is required.
// Async variants (ForwardToClient/ForwardToDestination) queue the data and return
// immediately — use these in the command phase hot loop so the select loop can
// pipeline reads without waiting for each write syscall to finish.
func (o *Orchestrator) NewWriteFuncs() WriteFuncs {
	return WriteFuncs{
		WriteToClient: func(conn net.Conn, data []byte) error {
			if conn != nil {
				return o.WriteToConn(conn, data)
			}
			return o.WriteToClient(data)
		},
		WriteToDestination: func(conn net.Conn, data []byte) error {
			if conn != nil {
				return o.WriteToConn(conn, data)
			}
			return o.WriteToDestination(data)
		},
		ForwardToClient: func(conn net.Conn, data []byte) error {
			if conn != nil {
				o.WriteAsyncToConn(conn, data)
			} else {
				o.WriteAsync(TargetClient, data)
			}
			return nil
		},
		ForwardToDestination: func(conn net.Conn, data []byte) error {
			if conn != nil {
				o.WriteAsyncToConn(conn, data)
			} else {
				o.WriteAsync(TargetDestination, data)
			}
			return nil
		},
	}
}

// NewClientWriteFunc creates a WriteFunc for client-only writes (replay mode).
// Uses the synchronous WriteToConn to ensure proper ordering for SSL/Auth.
func (o *Orchestrator) NewClientWriteFunc() WriteFunc {
	return func(conn net.Conn, data []byte) error {
		if conn != nil {
			return o.WriteToConn(conn, data)
		}
		// Fallback to stored client conn if none provided
		return o.WriteToClient(data)
	}
}

// DirectWriteFuncs creates WriteFuncs that write directly to the connections (synchronous).
// Both sync and async variants perform the same synchronous write since there
// is no orchestrator channel to decouple them.
func DirectWriteFuncs(clientWrite, destWrite func([]byte) (int, error)) WriteFuncs {
	writeClient := func(conn net.Conn, data []byte) error {
		if conn != nil {
			_, err := conn.Write(data)
			return err
		}
		_, err := clientWrite(data)
		return err
	}
	writeDest := func(conn net.Conn, data []byte) error {
		if conn != nil {
			_, err := conn.Write(data)
			return err
		}
		_, err := destWrite(data)
		return err
	}
	return WriteFuncs{
		WriteToClient:        writeClient,
		WriteToDestination:   writeDest,
		ForwardToClient:      writeClient,
		ForwardToDestination: writeDest,
	}
}

// DirectClientWriteFunc creates a WriteFunc that writes directly to the connection (synchronous).
func DirectClientWriteFunc(write func([]byte) (int, error)) WriteFunc {
	return func(conn net.Conn, data []byte) error {
		if conn != nil {
			_, err := conn.Write(data)
			return err
		}
		_, err := write(data)
		return err
	}
}
