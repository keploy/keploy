package recorder

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ── Memory ballast ────────────────────────────────────────────────────
// A memory ballast is a large pre-allocated byte slice that's never used.
// It tricks the GC into thinking the live heap is larger than it is, which
// reduces GC frequency (GC triggers at 2x live heap by default).
// 64MB ballast → GC won't trigger until heap reaches ~128MB.
//
//nolint:gochecknoglobals
var ballast = make([]byte, 64<<20) // 64 MB

// Keep ballast alive - the compiler can't optimize it away.
func init() { _ = ballast }

// ── sync.Pool for RawMockEntry ───────────────────────────────────
// Pool hot-path objects to reduce allocation rate.
//
//nolint:gochecknoglobals
var rawMockEntryPool = sync.Pool{
	New: func() any {
		return &RawMockEntry{
			ReqPackets:  make([][]byte, 0, 1),
			RespPackets: make([][]byte, 0, 16),
		}
	},
}

func acquireRawMockEntry() *RawMockEntry {
	e := rawMockEntryPool.Get().(*RawMockEntry)
	e.ReqPackets = e.ReqPackets[:0]
	e.RespPackets = e.RespPackets[:0]
	return e
}

func releaseRawMockEntry(e *RawMockEntry) {
	if e == nil {
		return
	}
	// Clear slices but keep capacity.
	e.ReqPackets = e.ReqPackets[:0]
	e.RespPackets = e.RespPackets[:0]
	e.CmdType = 0
	e.MockType = ""
	rawMockEntryPool.Put(e)
}

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

const slabSize = 1024 * 1024 // 1MB per slab — larger slab = fewer allocations

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

// ── peekReader interface ────────────────────────────────────────────────

// peekReader is satisfied by both *bufio.Reader and *orchestrator.TeeForwardConn.
// Using TeeForwardConn directly avoids double-buffering (TeeForwardConn already
// wraps its ring buffer in a 64 KB bufio.Reader).
type peekReader interface {
	io.Reader
	Peek(n int) ([]byte, error)
}

// ── Record pipeline ─────────────────────────────────────────────────────

// runRecordPipeline reads from the two TeeForwardConn ring buffers,
// frames MySQL packets using slab allocation, and hands them off to a
// separate decode goroutine.  This architecture ensures:
//
//   - The read loop is ALLOCATION-FREE (slab only) and runs in a tight loop
//   - Decode allocations happen ASYNCHRONOUSLY in a separate goroutine
//   - GC pressure from decode cannot slow the read loop or forwarding
//   - Constant memory footprint (no unbounded raw packet buffering)
//   - Mocks flow continuously to the consumer (InsertMock)
//
// The rawCh channel between read and decode goroutines provides back-pressure
// if decode falls behind, but is buffered (256) to absorb short bursts.
func runRecordPipeline(
	ctx context.Context,
	logger *zap.Logger,
	clientSrc peekReader, // *TeeForwardConn — avoids double-buffering
	serverSrc peekReader, // *TeeForwardConn — avoids double-buffering
	mocks chan<- *models.Mock,
	opts models.OutgoingOptions,
	hs handshakeState,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered from panic in runRecordPipeline", zap.Any("panic", r))
		}
	}()

	connID := ""
	if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
		connID = v.(string)
	}

	// ── Async decode goroutine ──────────────────────────────────────────
	// Buffered channel between read loop and decode goroutine.
	// The 1024 buffer absorbs burst traffic so read loop doesn't block.
	rawCh := make(chan *RawMockEntry, 1024)

	// Spawn decode goroutine — all decode allocations happen here,
	// completely decoupled from the read loop.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("recovered from panic in decode goroutine", zap.Any("panic", r))
			}
		}()

		// Per-connection decode context — seeded with handshake state.
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
		// Pre-populate the ServerGreetings map so DecodePayload can find it.
		if hs.ServerGreeting != nil {
			decodeCtx.ServerGreetings.Store(connKey, hs.ServerGreeting)
		}

		for entry := range rawCh {
			mock, err := decodeRawMockEntry(ctx, logger, *entry, decodeCtx, connKey)
			releaseRawMockEntry(entry) // Return to pool after decode
			if err != nil {
				logger.Debug("failed to decode mock entry", zap.Error(err))
				continue
			}
			setConnID(mock, connID)
			// Non-blocking send with select — if channel would block, try once more then skip.
			// Mocks channel is 50K buffered so this almost never happens.
			select {
			case mocks <- mock:
			default:
				// Channel full — try one more time with context check.
				select {
				case mocks <- mock:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// ── Read loop (allocation-free) ────────────────────────────────────
	slab := newPktSlab()

	for {
		if ctx.Err() != nil {
			close(rawCh)
			return
		}

		// ── 1. Read one command packet from the client ──
		cmdPacket, err := readMySQLPacketSlab(clientSrc, slab)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("record pipeline: client read error", zap.Error(err))
			}
			close(rawCh)
			return
		}

		reqTimestamp := time.Now()

		if len(cmdPacket) < 5 {
			logger.Warn("record pipeline: command packet too short", zap.Int("len", len(cmdPacket)))
			close(rawCh)
			return
		}
		cmdType := cmdPacket[4]

		// ── 2. Handle no-response commands ──
		if isNoResponseCmd(cmdType) {
			entry := acquireRawMockEntry()
			entry.ReqPackets = append(entry.ReqPackets, cmdPacket)
			entry.CmdType = cmdType
			entry.MockType = "mocks"
			entry.ReqTimestamp = reqTimestamp
			entry.ResTimestamp = time.Now()
			rawCh <- entry
			continue
		}

		// ── 3. Read the full response from the server ──
		respPackets, err := readFullResponseSlab(ctx, logger, serverSrc, cmdType, hs, slab)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("record pipeline: server response error", zap.Error(err))
			}
			close(rawCh)
			return
		}

		// ── 4. Send raw entry to decode goroutine (no decode here) ──
		// The read loop uses pool to avoid per-request allocations.
		entry := acquireRawMockEntry()
		entry.ReqPackets = append(entry.ReqPackets, cmdPacket)
		entry.RespPackets = append(entry.RespPackets, respPackets...)
		entry.CmdType = cmdType
		entry.MockType = "mocks"
		entry.ReqTimestamp = reqTimestamp
		entry.ResTimestamp = time.Now()
		rawCh <- entry
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
func readMySQLPacketSlab(r peekReader, slab *pktSlab) ([]byte, error) {
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
func readFullResponseSlab(ctx context.Context, logger *zap.Logger, serverReader peekReader, cmdType byte, hs handshakeState, slab *pktSlab) ([][]byte, error) {
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

func readResultSetPacketsSlab(ctx context.Context, serverReader peekReader, firstPkt []byte, hs handshakeState, slab *pktSlab) ([][]byte, error) {
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

func readStmtPrepareResponseSlab(ctx context.Context, serverReader peekReader, firstPkt []byte, hs handshakeState, slab *pktSlab) ([][]byte, error) {
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
