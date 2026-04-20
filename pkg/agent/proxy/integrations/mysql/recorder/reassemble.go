package recorder

// maxMySQLReassemblyBufferSize is the upper bound on how much data the
// reassembly buffer will hold before discarding partial messages.
// 16 MB is MySQL's max_allowed_packet default.
const maxMySQLReassemblyBufferSize = 16 * 1024 * 1024

// mysqlReassemblyBuffer accumulates raw TCP chunks and yields complete
// MySQL wire-protocol packets. A MySQL packet has a 4-byte header:
//
//	bytes 0-2: payload length (3 bytes, little-endian)
//	byte 3:    sequence ID
//
// Total packet size = payload_length + 4.
type mysqlReassemblyBuffer struct {
	buf      []byte
	overflow bool
}

// append adds raw bytes from a TCP read to the internal buffer.
func (r *mysqlReassemblyBuffer) append(data []byte) {
	if r.overflow {
		return
	}
	r.buf = append(r.buf, data...)
	if len(r.buf) > maxMySQLReassemblyBufferSize {
		r.buf = nil
		r.overflow = true
	}
}

// extractCompletePacket returns the first complete MySQL packet, or nil
// if not enough data has been accumulated yet. Each call removes the
// returned packet from the buffer.
func (r *mysqlReassemblyBuffer) extractCompletePacket() []byte {
	if len(r.buf) < 4 {
		return nil
	}
	// MySQL payload length is the first 3 bytes, little-endian.
	payloadLen := int(r.buf[0]) | int(r.buf[1])<<8 | int(r.buf[2])<<16
	totalLen := payloadLen + 4
	if totalLen > maxMySQLReassemblyBufferSize {
		// Invalid — skip header to avoid getting stuck.
		if len(r.buf) > 4 {
			r.buf = r.buf[4:]
		} else {
			r.buf = nil
		}
		return nil
	}
	if len(r.buf) < totalLen {
		return nil // partial packet
	}

	complete := make([]byte, totalLen)
	copy(complete, r.buf[:totalLen])

	remaining := copy(r.buf, r.buf[totalLen:])
	if remaining == 0 {
		r.buf = nil
	} else {
		r.buf = r.buf[:remaining]
	}
	return complete
}

// pending returns the number of bytes buffered but not yet extracted.
func (r *mysqlReassemblyBuffer) pending() int {
	return len(r.buf)
}

// didOverflow returns true if the buffer had to discard data.
func (r *mysqlReassemblyBuffer) didOverflow() bool {
	return r.overflow
}
