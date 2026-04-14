package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// mysqlDecodeItem carries a forwarded chunk to the async decode goroutine.
type mysqlDecodeItem struct {
	fromClient bool
	data       []byte
	ts         time.Time
}

// handleClientQueries handles the MySQL command phase with non-blocking
// forwarding. Raw bytes are relayed at wire speed in the main select loop.
// All packet reassembly, decoding, and mock creation is offloaded to a
// background goroutine via a buffered decode channel.
func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {
	// If recording is already paused, pure passthrough.
	if memoryguard.IsRecordingPaused() {
		logger.Debug("memory pressure detected, stopping MySQL recording and falling back to passthrough")
		done := make(chan struct{}, 2)
		cp := func(dst, src net.Conn) {
			_, _ = io.Copy(dst, src)
			done <- struct{}{}
		}
		go cp(destConn, clientConn)
		go cp(clientConn, destConn)
		<-done
		<-done
		return nil
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// Buffered channels let ReadBuffConn read ahead without waiting
	// for the main loop, preventing TCP flow-control throttling.
	clientBuffChan := make(chan []byte, 256)
	destBuffChan := make(chan []byte, 256)
	errChan := make(chan error, 1)

	// Read commands from client. stopOnRecordingPause is false: the main
	// loop skips sending to decodeChan when paused, so recording stops
	// while forwarding continues through the same goroutines.
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(clientBuffChan)
		pUtil.ReadBuffConn(ctx, logger, clientConn, clientBuffChan, errChan, false)
		return nil
	})

	// Read responses from destination.
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(destBuffChan)
		pUtil.ReadBuffConn(ctx, logger, destConn, destBuffChan, errChan, false)
		return nil
	})

	go func() {
		defer pUtil.Recover(logger, clientConn, destConn)
		err := g.Wait()
		if err != nil {
			logger.Debug("error group is returning an error", zap.Error(err))
		}
		close(errChan)
	}()

	// Async decode channel and background goroutine.
	decodeChan := make(chan mysqlDecodeItem, 512)
	decodeDone := make(chan struct{})
	go func() {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(decodeDone)
		asyncMySQLDecode(ctx, logger, decodeChan, mocks, decodeCtx, clientConn, opts)
	}()

	// Main loop: forward only, send copies for async decode.
	for {
		select {
		case <-ctx.Done():
			close(decodeChan)
			<-decodeDone
			return ctx.Err()

		case buffer, ok := <-clientBuffChan:
			if !ok {
				clientBuffChan = nil
				continue
			}
			if buffer == nil {
				continue
			}

			// Forward to destination immediately — critical path.
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write command to the server")
				close(decodeChan)
				<-decodeDone
				return err
			}

			// Non-blocking send to async decode.
			if !memoryguard.IsRecordingPaused() && len(decodeChan) < cap(decodeChan) {
				buf := make([]byte, len(buffer))
				copy(buf, buffer)
				select {
				case decodeChan <- mysqlDecodeItem{fromClient: true, data: buf, ts: time.Now()}:
				default:
				}
			}

		case buffer, ok := <-destBuffChan:
			if !ok {
				destBuffChan = nil
				continue
			}
			if buffer == nil {
				continue
			}

			// Forward to client immediately — critical path.
			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write response to the client")
				close(decodeChan)
				<-decodeDone
				return err
			}

			// Non-blocking send to async decode.
			if !memoryguard.IsRecordingPaused() && len(decodeChan) < cap(decodeChan) {
				buf := make([]byte, len(buffer))
				copy(buf, buffer)
				select {
				case decodeChan <- mysqlDecodeItem{fromClient: false, data: buf, ts: time.Now()}:
				default:
				}
			}

		case err, ok := <-errChan:
			if !ok || err == nil {
				close(decodeChan)
				<-decodeDone
				return nil
			}

			// Drain buffered data before exiting.
		drain:
			for {
				select {
				case buf, ok := <-clientBuffChan:
					if !ok {
						clientBuffChan = nil
						continue
					}
					if buf == nil {
						continue
					}
					_, _ = destConn.Write(buf)
				case buf, ok := <-destBuffChan:
					if !ok {
						destBuffChan = nil
						continue
					}
					if buf == nil {
						continue
					}
					_, _ = clientConn.Write(buf)
				default:
					break drain
				}
			}

			close(decodeChan)
			<-decodeDone
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// mysqlDecodeState tracks what the async decoder expects next.
type mysqlDecodeState int

const (
	stateExpectCommand           mysqlDecodeState = iota // waiting for a client command
	stateExpectResponse                                  // waiting for first response packet
	stateExpectColumns                                   // reading column definition packets
	stateExpectEOFAfterColumns                           // expecting EOF after column defs
	stateExpectRows                                      // reading row data packets
	stateExpectStmtParams                                // reading param defs for COM_STMT_PREPARE
	stateExpectEOFAfterParams                            // expecting EOF after param defs
	stateExpectStmtColumns                               // reading column defs for COM_STMT_PREPARE
	stateExpectEOFAfterStmtCols                          // expecting EOF after stmt column defs
)

// asyncMySQLDecode runs in a background goroutine and processes forwarded
// chunks in FIFO order. It reassembles MySQL packets, decodes them, pairs
// commands with responses, and records mocks — all off the forwarding path.
func asyncMySQLDecode(ctx context.Context, logger *zap.Logger, decodeChan <-chan mysqlDecodeItem, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, clientConn net.Conn, opts models.OutgoingOptions) {
	var clientReassembly mysqlReassemblyBuffer
	var destReassembly mysqlReassemblyBuffer
	var clientOverflowLogged, destOverflowLogged bool

	state := stateExpectCommand

	// Current command being processed.
	var (
		pendingCommand    *mysql.PacketBundle
		reqTimestamp      time.Time
		resTimestamp      time.Time
		pendingRespBundle *mysql.PacketBundle // accumulated response
		lastOp            byte                // the MySQL command type
		remainingCols     uint64              // columns left to read
		remainingParams   uint16              // params left to read (stmt prepare)
	)

	// Temporary storage for result set assembly.
	var (
		textResultSet   *mysql.TextResultSet
		binaryResultSet *mysql.BinaryProtocolResultSet
		stmtPrepareOk   *mysql.StmtPrepareOkPacket
	)

	flushMock := func() {
		if pendingCommand == nil || pendingRespBundle == nil {
			return
		}
		requests := []mysql.Request{{PacketBundle: *pendingCommand}}
		responses := []mysql.Response{{PacketBundle: *pendingRespBundle}}
		respOp := pendingRespBundle.Header.Type
		recordMock(ctx, requests, responses, "mocks", pendingCommand.Header.Type, respOp, mocks, reqTimestamp, resTimestamp, opts)
		pendingCommand = nil
		pendingRespBundle = nil
		state = stateExpectCommand
	}

	for item := range decodeChan {
		if item.fromClient {
			// --- Client command chunk ---
			clientReassembly.append(item.data)
			if clientReassembly.didOverflow() && !clientOverflowLogged {
				logger.Debug("MySQL client reassembly buffer exceeded limit")
				clientOverflowLogged = true
			}

			for {
				pkt := clientReassembly.extractCompletePacket()
				if pkt == nil {
					break
				}

				// If we had an incomplete exchange, flush it.
				if state != stateExpectCommand && pendingCommand != nil {
					flushMock()
				}

				reqTimestamp = item.ts

				commandPkt, err := wire.DecodePayload(ctx, logger, pkt, clientConn, decodeCtx)
				if err != nil {
					logger.Debug("failed to decode MySQL command in async decoder", zap.Error(err))
					state = stateExpectCommand
					pendingCommand = nil
					continue
				}

				pendingCommand = commandPkt

				// Handle no-response commands.
				if wire.IsNoResponseCommand(commandPkt.Header.Type) {
					pendingRespBundle = &mysql.PacketBundle{
						Header:  &mysql.PacketInfo{Type: "NO Response Packet"},
						Message: nil,
					}
					flushMock()
					continue
				}

				// Unknown/unrecognized packet types — treat as no-response.
				if strings.HasPrefix(commandPkt.Header.Type, "0x") {
					logger.Debug("Skipping unknown command packet to avoid stream desync",
						zap.String("type", commandPkt.Header.Type))
					pendingRespBundle = &mysql.PacketBundle{
						Header:  &mysql.PacketInfo{Type: "NO Response Packet"},
						Message: nil,
					}
					flushMock()
					continue
				}

				// Determine the command type for response handling.
				op, opOk := decodeCtx.LastOp.Load(clientConn)
				if opOk {
					lastOp = op
				}

				state = stateExpectResponse
			}

		} else {
			// --- Server response chunk ---
			destReassembly.append(item.data)
			if destReassembly.didOverflow() && !destOverflowLogged {
				logger.Debug("MySQL dest reassembly buffer exceeded limit")
				destOverflowLogged = true
			}

			for {
				pkt := destReassembly.extractCompletePacket()
				if pkt == nil {
					break
				}

				if state == stateExpectCommand {
					// Unexpected server data without a pending command.
					logger.Debug("received MySQL response with no pending command")
					continue
				}

				switch state {
				case stateExpectResponse:
					state = processFirstResponse(logger, pkt, decodeCtx, clientConn, lastOp,
						&pendingRespBundle, &textResultSet, &binaryResultSet, &stmtPrepareOk,
						&remainingCols, &remainingParams, &state)
					if state == stateExpectCommand {
						resTimestamp = item.ts
						flushMock()
					}

				case stateExpectColumns:
					col, _, err := rowscols.DecodeColumn(ctx, logger, pkt)
					if err != nil {
						logger.Debug("failed to decode column definition in async decoder", zap.Error(err))
						state = stateExpectCommand
						continue
					}
					if textResultSet != nil {
						textResultSet.Columns = append(textResultSet.Columns, col)
					} else if binaryResultSet != nil {
						binaryResultSet.Columns = append(binaryResultSet.Columns, col)
					}
					remainingCols--
					if remainingCols == 0 {
						if !decodeCtx.DeprecateEOF() {
							state = stateExpectEOFAfterColumns
						} else {
							state = stateExpectRows
						}
					}

				case stateExpectEOFAfterColumns:
					if !mysqlUtils.IsEOFPacket(pkt) {
						logger.Debug("expected EOF after columns, got something else")
					}
					if textResultSet != nil {
						textResultSet.EOFAfterColumns = pkt
					} else if binaryResultSet != nil {
						binaryResultSet.EOFAfterColumns = pkt
					}
					state = stateExpectRows

				case stateExpectRows:
					if mysqlUtils.IsResultSetTerminator(pkt, decodeCtx.DeprecateEOF()) {
						respType := mysql.StatusToString(mysql.EOF)
						if decodeCtx.DeprecateEOF() && mysqlUtils.IsOKReplacingEOF(pkt) {
							respType = mysql.StatusToString(mysql.OK)
						}
						finalResp := &mysql.GenericResponse{Data: pkt, Type: respType}
						if textResultSet != nil {
							textResultSet.FinalResponse = finalResp
							decodeCtx.LastOp.Store(clientConn, wire.RESET)
							pendingRespBundle = &mysql.PacketBundle{
								Header:  &mysql.PacketInfo{Type: respType},
								Message: textResultSet,
							}
						} else if binaryResultSet != nil {
							binaryResultSet.FinalResponse = finalResp
							decodeCtx.LastOp.Store(clientConn, wire.RESET)
							pendingRespBundle = &mysql.PacketBundle{
								Header:  &mysql.PacketInfo{Type: respType},
								Message: binaryResultSet,
							}
						}
						resTimestamp = item.ts
						flushMock()
					} else {
						// Row data.
						if textResultSet != nil {
							row, _, err := rowscols.DecodeTextRow(ctx, logger, pkt, textResultSet.Columns)
							if err != nil {
								logger.Debug("failed to decode text row in async decoder", zap.Error(err))
							} else {
								textResultSet.Rows = append(textResultSet.Rows, row)
							}
						} else if binaryResultSet != nil {
							row, _, err := rowscols.DecodeBinaryRow(ctx, logger, pkt, binaryResultSet.Columns)
							if err != nil {
								logger.Debug("failed to decode binary row in async decoder", zap.Error(err))
							} else {
								binaryResultSet.Rows = append(binaryResultSet.Rows, row)
							}
						}
					}

				case stateExpectStmtParams:
					col, _, err := rowscols.DecodeColumn(ctx, logger, pkt)
					if err != nil {
						logger.Debug("failed to decode param definition in async decoder", zap.Error(err))
						state = stateExpectCommand
						continue
					}
					if stmtPrepareOk != nil {
						stmtPrepareOk.ParamDefs = append(stmtPrepareOk.ParamDefs, col)
					}
					remainingParams--
					if remainingParams == 0 {
						if !decodeCtx.DeprecateEOF() {
							state = stateExpectEOFAfterParams
						} else {
							if stmtPrepareOk != nil && stmtPrepareOk.NumColumns > 0 {
								remainingCols = uint64(stmtPrepareOk.NumColumns)
								state = stateExpectStmtColumns
							} else {
								decodeCtx.LastOp.Store(clientConn, mysql.OK)
								resTimestamp = item.ts
								flushMock()
							}
						}
					}

				case stateExpectEOFAfterParams:
					if mysqlUtils.IsEOFPacket(pkt) && stmtPrepareOk != nil {
						stmtPrepareOk.EOFAfterParamDefs = pkt
					}
					if stmtPrepareOk != nil && stmtPrepareOk.NumColumns > 0 {
						remainingCols = uint64(stmtPrepareOk.NumColumns)
						state = stateExpectStmtColumns
					} else {
						decodeCtx.LastOp.Store(clientConn, mysql.OK)
						resTimestamp = item.ts
						flushMock()
					}

				case stateExpectStmtColumns:
					col, _, err := rowscols.DecodeColumn(ctx, logger, pkt)
					if err != nil {
						logger.Debug("failed to decode stmt column definition in async decoder", zap.Error(err))
						state = stateExpectCommand
						continue
					}
					if stmtPrepareOk != nil {
						stmtPrepareOk.ColumnDefs = append(stmtPrepareOk.ColumnDefs, col)
					}
					remainingCols--
					if remainingCols == 0 {
						if !decodeCtx.DeprecateEOF() {
							state = stateExpectEOFAfterStmtCols
						} else {
							decodeCtx.LastOp.Store(clientConn, mysql.OK)
							resTimestamp = item.ts
							flushMock()
						}
					}

				case stateExpectEOFAfterStmtCols:
					if mysqlUtils.IsEOFPacket(pkt) && stmtPrepareOk != nil {
						stmtPrepareOk.EOFAfterColumnDefs = pkt
					}
					decodeCtx.LastOp.Store(clientConn, mysql.OK)
					resTimestamp = item.ts
					flushMock()
				}
			}
		}
	}

	// Channel closed — flush any remaining exchange.
	flushMock()
}

// processFirstResponse handles the first response packet of a MySQL
// command-response exchange. It returns the new decoder state.
func processFirstResponse(
	logger *zap.Logger,
	pkt []byte,
	decodeCtx *wire.DecodeContext,
	clientConn net.Conn,
	lastOp byte,
	pendingRespBundle **mysql.PacketBundle,
	textResultSet **mysql.TextResultSet,
	binaryResultSet **mysql.BinaryProtocolResultSet,
	stmtPrepareOk **mysql.StmtPrepareOkPacket,
	remainingCols *uint64,
	remainingParams *uint16,
	_ *mysqlDecodeState,
) mysqlDecodeState {
	// Try to decode the response packet.
	commandRespPkt, err := wire.DecodePayload(context.Background(), logger, pkt, clientConn, decodeCtx)
	if err != nil {
		logger.Debug("failed to decode MySQL response in async decoder", zap.Error(err))
		return stateExpectCommand
	}

	// Check if response is OK or ERR — simple single-packet response.
	if commandRespPkt.Header.Type == mysql.StatusToString(mysql.ERR) ||
		commandRespPkt.Header.Type == mysql.StatusToString(mysql.OK) {
		*pendingRespBundle = commandRespPkt
		return stateExpectCommand
	}

	// Guard: if response was decoded as a command packet, streams are desynced.
	if isCommandPacket(commandRespPkt.Message) {
		logger.Debug("Response decoded as command packet — stream desync detected",
			zap.String("responseType", commandRespPkt.Header.Type))
		decodeCtx.LastOp.Store(clientConn, wire.RESET)
		*pendingRespBundle = commandRespPkt
		return stateExpectCommand
	}

	// Multi-packet response — determine type based on lastOp.
	switch lastOp {
	case mysql.COM_QUERY:
		ts, ok := commandRespPkt.Message.(*mysql.TextResultSet)
		if !ok {
			logger.Debug("expected TextResultSet",
				zap.String("got", fmt.Sprintf("%T", commandRespPkt.Message)))
			*pendingRespBundle = commandRespPkt
			return stateExpectCommand
		}
		*textResultSet = ts
		*binaryResultSet = nil
		*stmtPrepareOk = nil
		*remainingCols = ts.ColumnCount
		*pendingRespBundle = commandRespPkt
		return stateExpectColumns

	case mysql.COM_STMT_PREPARE:
		sp, ok := commandRespPkt.Message.(*mysql.StmtPrepareOkPacket)
		if !ok {
			logger.Debug("expected StmtPrepareOkPacket",
				zap.String("got", fmt.Sprintf("%T", commandRespPkt.Message)))
			*pendingRespBundle = commandRespPkt
			return stateExpectCommand
		}
		*stmtPrepareOk = sp
		*textResultSet = nil
		*binaryResultSet = nil
		*pendingRespBundle = commandRespPkt
		if sp.NumParams > 0 {
			*remainingParams = sp.NumParams
			return stateExpectStmtParams
		}
		if sp.NumColumns > 0 {
			*remainingCols = uint64(sp.NumColumns)
			return stateExpectStmtColumns
		}
		// No params, no columns — done.
		decodeCtx.LastOp.Store(clientConn, mysql.OK)
		return stateExpectCommand

	case mysql.COM_STMT_EXECUTE:
		bs, ok := commandRespPkt.Message.(*mysql.BinaryProtocolResultSet)
		if !ok {
			logger.Debug("expected BinaryProtocolResultSet",
				zap.String("got", fmt.Sprintf("%T", commandRespPkt.Message)))
			*pendingRespBundle = commandRespPkt
			return stateExpectCommand
		}
		*binaryResultSet = bs
		*textResultSet = nil
		*stmtPrepareOk = nil
		*remainingCols = bs.ColumnCount
		*pendingRespBundle = commandRespPkt
		return stateExpectColumns

	default:
		logger.Debug("unsupported lastOp in async decoder",
			zap.String("op", fmt.Sprintf("%x", lastOp)))
		*pendingRespBundle = commandRespPkt
		return stateExpectCommand
	}
}

// isCommandPacket returns true if the decoded message is a client command type
// rather than a server response type. This is used to detect stream desync
// where DecodePayload misidentifies a server response as a client command.
func isCommandPacket(msg interface{}) bool {
	switch msg.(type) {
	case *mysql.QueryPacket,
		*mysql.StmtPreparePacket,
		*mysql.StmtExecutePacket,
		*mysql.StmtClosePacket,
		*mysql.StmtResetPacket,
		*mysql.StmtSendLongDataPacket,
		*mysql.QuitPacket,
		*mysql.InitDBPacket,
		*mysql.PingPacket,
		*mysql.StatisticsPacket,
		*mysql.DebugPacket,
		*mysql.ChangeUserPacket,
		*mysql.ResetConnectionPacket:
		return true
	default:
		return false
	}
}
