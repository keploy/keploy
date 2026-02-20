package recorder

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

const reassemblerBufSize = 64 * 1024

// RunReassembler reads from two io.Readers (one per direction), frames MySQL
// packets, and groups them into request-response pairs using protocol-level
// knowledge.
//
// The readers are typically backed by TeeForwardConn ring buffers — meaning
// all reads come from pre-allocated memory with zero heap allocations for
// the read itself.  The only allocation per packet is the []byte that holds
// the framed packet data.
//
// It runs until an io.Reader returns EOF/error or the context is cancelled.
func RunReassembler(
	ctx context.Context,
	logger *zap.Logger,
	clientSrc io.Reader,
	serverSrc io.Reader,
	rawMocks chan<- RawMockEntry,
	hs handshakeState,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered from panic in reassembler", zap.Any("panic", r))
		}
	}()

	clientReader := bufio.NewReaderSize(clientSrc, reassemblerBufSize)
	serverReader := bufio.NewReaderSize(serverSrc, reassemblerBufSize)

	for {
		if ctx.Err() != nil {
			return
		}

		// ── Read one command packet from the client ──
		cmdPacket, err := readMySQLPacket(clientReader)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("reassembler: client read error", zap.Error(err))
			}
			return
		}

		reqTimestamp := time.Now()

		if len(cmdPacket) < 5 {
			logger.Warn("reassembler: command packet too short", zap.Int("len", len(cmdPacket)))
			return
		}
		cmdType := cmdPacket[4] // first payload byte

		// Commands that expect no server response.
		if isNoResponseCmd(cmdType) {
			select {
			case rawMocks <- RawMockEntry{
				ReqPackets:   [][]byte{cmdPacket},
				CmdType:      cmdType,
				MockType:     "mocks",
				ReqTimestamp: reqTimestamp,
				ResTimestamp: time.Now(),
			}:
			case <-ctx.Done():
				return
			}
			continue
		}

		// ── Read the full response from the server ──
		respPackets, err := readFullResponse(ctx, logger, serverReader, cmdType, hs)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("reassembler: server response error",
					zap.Error(err), zap.String("cmd", fmt.Sprintf("%#x", cmdType)))
			}
			return
		}

		select {
		case rawMocks <- RawMockEntry{
			ReqPackets:   [][]byte{cmdPacket},
			RespPackets:  respPackets,
			CmdType:      cmdType,
			MockType:     "mocks",
			ReqTimestamp: reqTimestamp,
			ResTimestamp: time.Now(),
		}:
		case <-ctx.Done():
			return
		}
	}
}

// readMySQLPacket reads a single complete MySQL wire packet (4-byte header +
// payload) from the buffered reader.  The returned slice owns its memory.
func readMySQLPacket(r *bufio.Reader) ([]byte, error) {
	// Read the 4-byte header.
	hdr, err := r.Peek(4)
	if err != nil {
		return nil, err
	}

	payloadLen := uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16
	totalLen := 4 + int(payloadLen)

	pkt := make([]byte, totalLen)
	if _, err := io.ReadFull(r, pkt); err != nil {
		return nil, err
	}
	return pkt, nil
}

// readFullResponse reads the complete server response for a given command
// type.  It returns all the packets that make up the response.
func readFullResponse(ctx context.Context, logger *zap.Logger, serverReader *bufio.Reader, cmdType byte, hs handshakeState) ([][]byte, error) {
	// Read the first response packet.
	first, err := readMySQLPacket(serverReader)
	if err != nil {
		return nil, err
	}

	if len(first) < 5 {
		return nil, fmt.Errorf("response packet too short: %d bytes", len(first))
	}

	marker := first[4] // first payload byte

	// OK (0x00) or ERR (0xFF): single-packet response.
	// EXCEPTION: COM_STMT_PREPARE_OK starts with 0x00 (same as mysql.OK)
	// but is followed by param/column definition packets — it must NOT
	// be treated as a simple single-packet response.
	if marker == mysql.ERR {
		return [][]byte{first}, nil
	}
	if marker == mysql.OK && cmdType != mysql.COM_STMT_PREPARE {
		return [][]byte{first}, nil
	}

	// EOF as a standalone response (e.g., after COM_PING in some versions).
	if marker == mysql.EOF && payloadLen(first) < 9 {
		return [][]byte{first}, nil
	}

	// Result set or prepared-statement response — protocol-specific framing.
	switch cmdType {
	case mysql.COM_QUERY:
		return readResultSetPackets(ctx, serverReader, first, hs, false)
	case mysql.COM_STMT_EXECUTE:
		return readResultSetPackets(ctx, serverReader, first, hs, true)
	case mysql.COM_STMT_PREPARE:
		return readStmtPrepareResponse(ctx, serverReader, first, hs)
	default:
		// Unknown — treat the first packet as the complete response.
		return [][]byte{first}, nil
	}
}

// readResultSetPackets reads a full MySQL result set: column defs + (optional
// EOF) + rows + final EOF/OK.
func readResultSetPackets(ctx context.Context, serverReader *bufio.Reader, firstPkt []byte, hs handshakeState, isBinary bool) ([][]byte, error) {
	_ = isBinary // both text and binary result sets have the same framing
	packets := [][]byte{firstPkt}

	// The first packet's payload is a length-encoded integer (column count).
	colCount := decodeLenEncInt(firstPkt[4:])

	// Read column definition packets.
	for i := uint64(0); i < colCount; i++ {
		pkt, err := readMySQLPacket(serverReader)
		if err != nil {
			return packets, err
		}
		packets = append(packets, pkt)
	}

	// Read EOF after column definitions (unless CLIENT_DEPRECATE_EOF).
	if !hs.DeprecateEOF {
		eof, err := readMySQLPacket(serverReader)
		if err != nil {
			return packets, err
		}
		packets = append(packets, eof)
	}

	// Read row data packets until EOF or OK.
	for {
		if ctx.Err() != nil {
			return packets, ctx.Err()
		}

		pkt, err := readMySQLPacket(serverReader)
		if err != nil {
			return packets, err
		}
		packets = append(packets, pkt)

		if len(pkt) >= 5 {
			marker := pkt[4]
			pLen := payloadLen(pkt)
			// EOF packet (marker=0xFE, payload < 9 bytes) signals end of rows.
			if marker == mysql.EOF && pLen < 9 {
				return packets, nil
			}
			// OK packet (marker=0x00) can also terminate rows when CLIENT_DEPRECATE_EOF is set.
			if hs.DeprecateEOF && marker == mysql.OK {
				// Need to distinguish OK-terminator from a row that starts with 0x00.
				// Binary rows start with 0x00 but always have a null-bitmap right after,
				// so payload length > column_count/8+2.  Text rows never start with 0x00
				// (they start with length-encoded strings).  For safety, we check the
				// the server status flags in the OK packet.
				// A proper OK packet after rows has the structure:
				//   [0x00][affected_rows LEI][last_insert_id LEI][status_flags 2B]...
				// We use a heuristic: if the packet is short enough to be an OK packet
				// (typically < 64 bytes) and starts with 0x00, treat it as the terminator.
				if pLen < 64 {
					return packets, nil
				}
			}
		}
	}
}

// readStmtPrepareResponse reads a COM_STMT_PREPARE_OK response: the OK packet
// + param definitions + column definitions + EOF markers.
func readStmtPrepareResponse(ctx context.Context, serverReader *bufio.Reader, firstPkt []byte, hs handshakeState) ([][]byte, error) {
	packets := [][]byte{firstPkt}

	// COM_STMT_PREPARE_OK payload structure:
	//   [0x00][stmt_id 4B][num_columns 2B][num_params 2B][reserved 1B][warning_count 2B]
	if len(firstPkt) < 16 { // 4 header + 12 payload minimum
		return packets, nil
	}

	payload := firstPkt[4:]
	numColumns := binary.LittleEndian.Uint16(payload[5:7])
	numParams := binary.LittleEndian.Uint16(payload[7:9])

	// Read param definition packets.
	if numParams > 0 {
		for i := uint16(0); i < numParams; i++ {
			if ctx.Err() != nil {
				return packets, ctx.Err()
			}
			pkt, err := readMySQLPacket(serverReader)
			if err != nil {
				return packets, err
			}
			packets = append(packets, pkt)
		}
		// EOF after params
		if !hs.DeprecateEOF {
			eof, err := readMySQLPacket(serverReader)
			if err != nil {
				return packets, err
			}
			packets = append(packets, eof)
		}
	}

	// Read column definition packets.
	if numColumns > 0 {
		for i := uint16(0); i < numColumns; i++ {
			if ctx.Err() != nil {
				return packets, ctx.Err()
			}
			pkt, err := readMySQLPacket(serverReader)
			if err != nil {
				return packets, err
			}
			packets = append(packets, pkt)
		}
		// EOF after columns
		if !hs.DeprecateEOF {
			eof, err := readMySQLPacket(serverReader)
			if err != nil {
				return packets, err
			}
			packets = append(packets, eof)
		}
	}

	return packets, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

// payloadLen extracts the 3-byte little-endian payload length from a MySQL
// wire packet.
func payloadLen(pkt []byte) uint32 {
	return uint32(pkt[0]) | uint32(pkt[1])<<8 | uint32(pkt[2])<<16
}

// decodeLenEncInt reads a MySQL length-encoded integer from the start of b.
// It returns the value.  This is a simplified version that only needs to handle
// the column-count field (typically a small number).
func decodeLenEncInt(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	switch {
	case b[0] < 0xfb:
		return uint64(b[0])
	case b[0] == 0xfc:
		if len(b) < 3 {
			return 0
		}
		return uint64(b[1]) | uint64(b[2])<<8
	case b[0] == 0xfd:
		if len(b) < 4 {
			return 0
		}
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16
	case b[0] == 0xfe:
		if len(b) < 9 {
			return 0
		}
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 |
			uint64(b[4])<<24 | uint64(b[5])<<32 | uint64(b[6])<<40 |
			uint64(b[7])<<48 | uint64(b[8])<<56
	default:
		return 0
	}
}

// isNoResponseCmd returns true for MySQL commands that the server does not
// respond to.
func isNoResponseCmd(cmd byte) bool {
	return cmd == mysql.COM_STMT_CLOSE || cmd == mysql.COM_STMT_SEND_LONG_DATA
}
