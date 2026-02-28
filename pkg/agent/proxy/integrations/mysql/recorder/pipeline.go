package recorder

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ── Slab allocator ──────────────────────────────────────────────────────
// pktSlab batches per-packet []byte allocations into large slabs.
// Instead of calling make([]byte, n) per MySQL packet (~5-10 times per query),
// it carves slices from a single pre-allocated slab (default 1MB).
// This reduces allocation count by ~100-500x, dramatically lowering GC pressure.
type pktSlab struct {
	buf     []byte
	off     int
	prevBuf []byte // Keep previous slab alive to prevent premature GC
}

const slabSize = 4 * 1024 * 1024 // 4MB per slab — larger slab = fewer allocations

func newPktSlab() *pktSlab {
	return &pktSlab{buf: make([]byte, slabSize)}
}

// alloc returns a []byte of exactly n bytes carved from the slab.
// If the current slab can't fit n, a new slab is allocated.
// Oversized packets (> slabSize) get their own allocation.
// The previous slab is kept alive for one more cycle to avoid
// premature GC of data still being referenced by the pipeline.
func (s *pktSlab) alloc(n int) []byte {
	if n > slabSize {
		return make([]byte, n)
	}
	if s.off+n > len(s.buf) {
		// Keep old buffer alive — slices carved from it may still be in use
		s.prevBuf = s.buf
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

// ── Decode concurrency gate ─────────────────────────────────────────────
//
// Global semaphore (cap=1): at most ONE goroutine system-wide does decode
// work at any instant.  The goroutine holds the sem during the 15ms sleep,
// so all other decode goroutines are fully parked.  This keeps Go's CPU
// usage at ~6% of one core (1ms decode + 15ms sleep = 16ms per cycle),
// preventing OS-level cache/scheduler contention with Rust's tokio workers.
//
// Throughput: 1000ms / 16ms ≈ 62 decodes/sec.
// Test load:  ~27 decodes/sec.  Headroom: ~2.3x.
var decodeSem = make(chan struct{}, 1)

// const decodeThrottleDelay = 7 * time.Millisecond

// MocksProduced is an atomic counter tracking how many mocks have been
// successfully sent to the mocks channel across all connections.
var MocksProduced atomic.Int64

// RawsCaptured is an atomic counter tracking how many raw request/response
// pairs have been captured (before decoding) across all connections.
var RawsCaptured atomic.Int64

// safeSendMock sends a mock to the channel, recovering from panics
// caused by sending on a closed channel during shutdown.
// Returns true if the send succeeded.
func safeSendMock(ch chan<- *models.Mock, m *models.Mock) (sent bool) {
	defer func() {
		if r := recover(); r != nil {
			sent = false
		}
	}()
	ch <- m
	return true
}

// ── Record pipeline ─────────────────────────────────────────────────────
//
// Two-stage design:
//   Stage 1 (capture): reads packets at full speed, builds RawMockEntry,
//           pushes into a buffered channel. Never throttled.
//   Stage 2 (decode):  drains the buffered channel, acquires the global
//           decodeSem, decodes, throttles, sends to mocks channel.
//
// This ensures all raw data is captured even when decode is throttled.

const rawEntryChanCap = 10000 // buffered channel between capture and decode

func runRecordPipeline(
	ctx context.Context,
	logger *zap.Logger,
	clientSrc peekReader,
	serverSrc peekReader,
	mocks chan<- *models.Mock,
	opts models.OutgoingOptions,
	hs handshakeState,
) {
	var mocksWritten int64
	var rawsCapturedLocal int64

	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered from panic in runRecordPipeline", zap.Any("panic", r))
		}
		logger.Info("record pipeline exiting",
			zap.Int64("raws_captured", rawsCapturedLocal),
			zap.Int64("mocks_written", mocksWritten),
			zap.Int64("mocks_pending_decode", rawsCapturedLocal-mocksWritten),
			zap.Int64("total_raws_captured_globally", RawsCaptured.Load()),
			zap.Int64("total_mocks_produced_globally", MocksProduced.Load()))
	}()

	connID := ""
	if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
		connID = v.(string)
	}

	// Buffered channel between capture and decode stages.
	rawCh := make(chan RawMockEntry, rawEntryChanCap)

	// ── Stage 1: Capture goroutine (runs at full speed) ──
	go func() {
		defer close(rawCh)

		slab := newPktSlab()

		for {
			if ctx.Err() != nil {
				return
			}

			cmdPacket, err := readMySQLPacketSlab(clientSrc, slab)
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					logger.Debug("record pipeline: client read error", zap.Error(err))
				}
				return
			}

			reqTimestamp := time.Now()

			if len(cmdPacket) < 5 {
				logger.Debug("record pipeline: command packet too short", zap.Int("len", len(cmdPacket)))
				return
			}
			cmdType := cmdPacket[4]

			var entry RawMockEntry
			if isNoResponseCmd(cmdType) {
				entry = RawMockEntry{
					ReqPackets:   [][]byte{cmdPacket},
					CmdType:      cmdType,
					MockType:     "mocks",
					ReqTimestamp: reqTimestamp,
					ResTimestamp: time.Now(),
				}
			} else {
				respPackets, err := readFullResponseSlab(ctx, logger, serverSrc, cmdType, hs, slab)
				if err != nil {
					if err != io.EOF && ctx.Err() == nil {
						logger.Debug("record pipeline: server response error", zap.Error(err))
					}
					return
				}
				entry = RawMockEntry{
					ReqPackets:   [][]byte{cmdPacket},
					RespPackets:  respPackets,
					CmdType:      cmdType,
					MockType:     "mocks",
					ReqTimestamp: reqTimestamp,
					ResTimestamp: time.Now(),
				}
			}

			RawsCaptured.Add(1)
			select {
			case rawCh <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()

	// ── Stage 2: Decode loop (throttled) ──
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
	if hs.ServerGreeting != nil {
		decodeCtx.ServerGreetings.Store(connKey, hs.ServerGreeting)
	}

	for entry := range rawCh {
		rawsCapturedLocal++

		// Acquire sem → decode → throttle → release sem
		decodeSem <- struct{}{}

		mock, err := decodeRawMockEntry(ctx, logger, entry, decodeCtx, connKey)

		// Sleep while HOLDING sem — parks all other decode goroutines.
		// // Skip after shutdown to drain remaining entries fast.
		// if ctx.Err() == nil {
		// 	time.Sleep(decodeThrottleDelay)
		// }
		<-decodeSem

		if err != nil {
			logger.Debug("failed to decode mock entry", zap.Error(err))
			continue
		}

		setConnID(mock, connID)

		// Use safeSendMock to avoid panic on closed channel during shutdown.
		if safeSendMock(mocks, mock) {
			mocksWritten++
			MocksProduced.Add(1)
		} else {
			logger.Warn("mocks channel closed, dropping mock",
				zap.String("conn_id", connID),
				zap.Int64("mocks_written_so_far", mocksWritten))
			return
		}
	}

	// rawCh closed — capture stage is done. Log remaining pending count.
	pending := rawsCapturedLocal - mocksWritten
	if pending > 0 {
		logger.Info("capture complete, finishing remaining decodes",
			zap.Int64("pending", pending), zap.String("conn_id", connID))
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

	// Simple single-packet responses: OK, ERR, or standalone EOF.
	// EXCEPTION: COM_STMT_PREPARE_OK starts with 0x00 (same as mysql.OK)
	// but is followed by param/column definition packets — it must NOT
	// be treated as a simple single-packet response.
	if marker == mysql.ERR {
		return [][]byte{first}, nil
	}
	if marker == mysql.OK && cmdType != mysql.COM_STMT_PREPARE {
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
