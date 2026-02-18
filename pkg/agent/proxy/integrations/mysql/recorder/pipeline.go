package recorder

import (
	"bufio"
	"context"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ── Slab allocator ──────────────────────────────────────────────────────
// pktSlab batches per-packet []byte allocations into large slabs.
// Instead of calling make([]byte, n) per MySQL packet (~5-10 times per query),
// it carves slices from a single pre-allocated slab (default 256KB).
// This reduces allocation count by ~100-500x, dramatically lowering GC pressure.
//
// Slabs are referenced by packet slices until the decoder is done with them,
// at which point the entire slab becomes garbage for the GC to collect as
// ONE object instead of hundreds of small objects.
type pktSlab struct {
	buf []byte
	off int
}

const slabSize = 256 * 1024 // 256KB per slab

func newPktSlab() *pktSlab {
	return &pktSlab{buf: make([]byte, slabSize)}
}

// alloc returns a []byte of exactly n bytes carved from the slab.
// If the current slab can't fit n, a new slab is allocated.
// Oversized packets (> slabSize) get their own allocation.
func (s *pktSlab) alloc(n int) []byte {
	if n > slabSize {
		return make([]byte, n)
	}
	if s.off+n > len(s.buf) {
		s.buf = make([]byte, slabSize)
		s.off = 0
	}
	p := s.buf[s.off : s.off+n : s.off+n]
	s.off += n
	return p
}

// ── Merged pipeline ─────────────────────────────────────────────────────

// runRecordPipeline is the merged reassembler+decoder goroutine.
// It reads from the two TeeForwardConn ring buffers (via io.Reader),
// frames MySQL packets, decodes them INLINE, and sends mocks to the
// output channel.
//
// This eliminates:
//   - The rawMocksCh intermediate channel (and its synchronization)
//   - One goroutine per connection (decoder goroutine)
//   - RawMockEntry struct allocations for command-phase packets
//
// Packet memory is allocated from a slab allocator (~1 allocation per 256KB
// of packet data instead of ~1 per packet), dramatically reducing GC pressure.
func runRecordPipeline(
	ctx context.Context,
	logger *zap.Logger,
	clientSrc io.Reader, // typically *TeeForwardConn
	serverSrc io.Reader, // typically *TeeForwardConn
	mocks chan<- *models.Mock,
	opts models.OutgoingOptions,
	hs handshakeState,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered from panic in runRecordPipeline", zap.Any("panic", r))
		}
	}()

	clientReader := bufio.NewReaderSize(clientSrc, reassemblerBufSize)
	serverReader := bufio.NewReaderSize(serverSrc, reassemblerBufSize)
	slab := newPktSlab()

	// Per-connection decode context — seeded with the server greeting
	// from the handshake phase so the fast-path decoder can use it.
	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		LastOpValue:        wire.RESET,
		ServerGreeting:     hs.ServerGreeting,
		ClientCapabilities: hs.ClientCaps,
		ServerCaps:         hs.ServerCaps,
		PluginName:         hs.PluginName,
		UseSSL:             hs.UseSSL,
	}
	var connKey net.Conn

	connID := ""
	if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
		connID = v.(string)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		// ── 1. Read one command packet from the client ──
		cmdPacket, err := readMySQLPacketSlab(clientReader, slab)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("record pipeline: client read error", zap.Error(err))
			}
			return
		}

		reqTimestamp := time.Now()

		if len(cmdPacket) < 5 {
			logger.Warn("record pipeline: command packet too short", zap.Int("len", len(cmdPacket)))
			return
		}
		cmdType := cmdPacket[4]

		// ── 2. Handle no-response commands ──
		if isNoResponseCmd(cmdType) {
			entry := RawMockEntry{
				ReqPackets:   [][]byte{cmdPacket},
				CmdType:      cmdType,
				MockType:     "mocks",
				ReqTimestamp: reqTimestamp,
				ResTimestamp: time.Now(),
			}
			mock, err := decodeRawMockEntry(ctx, logger, entry, decodeCtx, connKey)
			if err != nil {
				logger.Debug("failed to decode no-response command", zap.Error(err))
				continue
			}
			setConnID(mock, connID)
			recordMockDirect(ctx, mock, mocks, opts)
			continue
		}

		// ── 3. Read the full response from the server ──
		respPackets, err := readFullResponseSlab(ctx, logger, serverReader, cmdType, hs, slab)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("record pipeline: server response error", zap.Error(err))
			}
			return
		}

		resTimestamp := time.Now()

		// ── 4. Decode inline (no intermediate channel) ──
		entry := RawMockEntry{
			ReqPackets:   [][]byte{cmdPacket},
			RespPackets:  respPackets,
			CmdType:      cmdType,
			MockType:     "mocks",
			ReqTimestamp: reqTimestamp,
			ResTimestamp: resTimestamp,
		}

		mock, err := decodeRawMockEntry(ctx, logger, entry, decodeCtx, connKey)
		if err != nil {
			logger.Debug("failed to decode mock entry", zap.Error(err))
			continue
		}
		setConnID(mock, connID)

		// ── 5. Send to final mocks channel ──
		recordMockDirect(ctx, mock, mocks, opts)

		// The packet bytes (from slab) and the RawMockEntry are now
		// unreferenced locals — they become garbage immediately.
		// The slab backing array stays alive until all slices from it
		// are collected, but that's ONE GC object per 256KB vs hundreds.
	}
}

func setConnID(mock *models.Mock, connID string) {
	if mock.Spec.Metadata == nil {
		mock.Spec.Metadata = make(map[string]string)
	}
	mock.Spec.Metadata["connID"] = connID
}

// ── Slab-based packet reading ───────────────────────────────────────────

// readMySQLPacketSlab reads a complete MySQL wire packet using slab allocation.
func readMySQLPacketSlab(r *bufio.Reader, slab *pktSlab) ([]byte, error) {
	hdr, err := r.Peek(4)
	if err != nil {
		return nil, err
	}

	pLen := uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16
	totalLen := 4 + int(pLen)

	pkt := slab.alloc(totalLen)
	if _, err := io.ReadFull(r, pkt); err != nil {
		return nil, err
	}
	return pkt, nil
}

// readFullResponseSlab reads the complete server response using slab allocation.
func readFullResponseSlab(ctx context.Context, logger *zap.Logger, serverReader *bufio.Reader, cmdType byte, hs handshakeState, slab *pktSlab) ([][]byte, error) {
	first, err := readMySQLPacketSlab(serverReader, slab)
	if err != nil {
		return nil, err
	}

	if len(first) < 5 {
		return nil, io.ErrUnexpectedEOF
	}

	marker := first[4]

	if marker == mysql.OK || marker == mysql.ERR {
		return [][]byte{first}, nil
	}

	if marker == mysql.EOF && payloadLen(first) < 9 {
		return [][]byte{first}, nil
	}

	switch cmdType {
	case mysql.COM_QUERY:
		return readResultSetPacketsSlab(ctx, serverReader, first, hs, slab)
	case mysql.COM_STMT_EXECUTE:
		return readResultSetPacketsSlab(ctx, serverReader, first, hs, slab)
	case mysql.COM_STMT_PREPARE:
		return readStmtPrepareResponseSlab(ctx, serverReader, first, hs, slab)
	default:
		return [][]byte{first}, nil
	}
}

func readResultSetPacketsSlab(ctx context.Context, serverReader *bufio.Reader, firstPkt []byte, hs handshakeState, slab *pktSlab) ([][]byte, error) {
	colCount := decodeLenEncInt(firstPkt[4:])
	// Pre-allocate: 1 (metadata) + colCount (col defs) + 1 (EOF) + estimated rows.
	cap := int(colCount) + 16
	packets := make([][]byte, 1, cap)
	packets[0] = firstPkt

	for i := uint64(0); i < colCount; i++ {
		pkt, err := readMySQLPacketSlab(serverReader, slab)
		if err != nil {
			return packets, err
		}
		packets = append(packets, pkt)
	}

	if !hs.DeprecateEOF {
		eof, err := readMySQLPacketSlab(serverReader, slab)
		if err != nil {
			return packets, err
		}
		packets = append(packets, eof)
	}

	for {
		if ctx.Err() != nil {
			return packets, ctx.Err()
		}

		pkt, err := readMySQLPacketSlab(serverReader, slab)
		if err != nil {
			return packets, err
		}
		packets = append(packets, pkt)

		if len(pkt) >= 5 {
			m := pkt[4]
			pLen := payloadLen(pkt)
			if m == mysql.EOF && pLen < 9 {
				return packets, nil
			}
			if hs.DeprecateEOF && m == mysql.OK && pLen < 64 {
				return packets, nil
			}
		}
	}
}

func readStmtPrepareResponseSlab(ctx context.Context, serverReader *bufio.Reader, firstPkt []byte, hs handshakeState, slab *pktSlab) ([][]byte, error) {
	packets := make([][]byte, 1, 16)
	packets[0] = firstPkt

	if len(firstPkt) < 16 {
		return packets, nil
	}

	payload := firstPkt[4:]
	numColumns := uint16(payload[5]) | uint16(payload[6])<<8
	numParams := uint16(payload[7]) | uint16(payload[8])<<8

	if numParams > 0 {
		for i := uint16(0); i < numParams; i++ {
			if ctx.Err() != nil {
				return packets, ctx.Err()
			}
			pkt, err := readMySQLPacketSlab(serverReader, slab)
			if err != nil {
				return packets, err
			}
			packets = append(packets, pkt)
		}
		if !hs.DeprecateEOF {
			eof, err := readMySQLPacketSlab(serverReader, slab)
			if err != nil {
				return packets, err
			}
			packets = append(packets, eof)
		}
	}

	if numColumns > 0 {
		for i := uint16(0); i < numColumns; i++ {
			if ctx.Err() != nil {
				return packets, ctx.Err()
			}
			pkt, err := readMySQLPacketSlab(serverReader, slab)
			if err != nil {
				return packets, err
			}
			packets = append(packets, pkt)
		}
		if !hs.DeprecateEOF {
			eof, err := readMySQLPacketSlab(serverReader, slab)
			if err != nil {
				return packets, err
			}
			packets = append(packets, eof)
		}
	}

	return packets, nil
}
