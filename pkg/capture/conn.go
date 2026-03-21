package capture

import (
	"io"
	"net"
	"sync"
	"time"
)

// CaptureConn wraps a net.Conn to capture all reads and writes into the capture file.
// It implements net.Conn and io.Reader so it can be used as a drop-in replacement.
type CaptureConn struct {
	net.Conn
	Reader io.Reader // optional override reader (for MultiReader patterns)

	writer       *Writer
	connectionID uint64
	readDir      Direction // direction to record for Read() calls
	writeDir     Direction // direction to record for Write() calls
	protocol     Protocol

	mu sync.Mutex
}

// NewCaptureConn creates a new capturing connection wrapper.
// readDir is the direction recorded when data is read from this connection.
// writeDir is the direction recorded when data is written to this connection.
func NewCaptureConn(conn net.Conn, reader io.Reader, connID uint64, readDir, writeDir Direction) *CaptureConn {
	cc := &CaptureConn{
		Conn:         conn,
		Reader:       reader,
		connectionID: connID,
		readDir:      readDir,
		writeDir:     writeDir,
	}
	return cc
}

// Read reads from the underlying connection and records the data.
func (cc *CaptureConn) Read(p []byte) (int, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	var n int
	var err error

	if cc.Reader != nil {
		n, err = cc.Reader.Read(p)
	} else {
		n, err = cc.Conn.Read(p)
	}

	if n > 0 && cc.writer != nil {
		// Best-effort capture: don't fail the read if capture fails
		pkt := &Packet{
			Timestamp:    time.Now(),
			ConnectionID: cc.connectionID,
			Type:         PacketTypeData,
			Direction:    cc.readDir,
			Protocol:     cc.protocol,
			Payload:      make([]byte, n),
		}
		copy(pkt.Payload, p[:n])
		// Fire and forget - capture errors don't affect the actual connection
		_ = cc.writer.WritePacket(pkt)
	}

	return n, err
}

// Write writes to the underlying connection and records the data.
func (cc *CaptureConn) Write(p []byte) (int, error) {
	n, err := cc.Conn.Write(p)

	if n > 0 {
		cc.mu.Lock()
		writer := cc.writer
		proto := cc.protocol
		cc.mu.Unlock()

		if writer != nil {
			pkt := &Packet{
				Timestamp:    time.Now(),
				ConnectionID: cc.connectionID,
				Type:         PacketTypeData,
				Direction:    cc.writeDir,
				Protocol:     proto,
				Payload:      make([]byte, n),
			}
			copy(pkt.Payload, p[:n])
			_ = writer.WritePacket(pkt)
		}
	}

	return n, err
}

// SetProtocol updates the protocol for future packets from this connection.
func (cc *CaptureConn) SetProtocol(proto Protocol) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.protocol = proto
}

// SetWriter sets or replaces the capture writer for this connection.
func (cc *CaptureConn) SetWriter(w *Writer) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.writer = w
}

// ConnectionID returns the connection's capture ID.
func (cc *CaptureConn) ConnectionID() uint64 {
	return cc.connectionID
}
