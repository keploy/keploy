package replayer

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func simulateCommandPhase(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {

	// Log initial mock state at the start of command phase
	total, cfg, data := mockDb.GetMySQLCounts()
	logger.Debug("Command phase starting",
		zap.Int("total_mysql_mocks", total),
		zap.Int("config_mocks", cfg),
		zap.Int("data_mocks_available", data))

	// Shared mock-noise engine for this connection's command phase. MySQL is
	// a client of the same engine HTTP uses — mysqlNoiseAdapter owns only the
	// MySQL-specific bit (canonicalizing command packets into diffable JSON).
	noiseEngine := mocknoise.New(mysqlNoiseAdapter{}, opts.MockNoiseDetection, opts.MockNoiseStrict)

	// User request-body noise from test.globalNoise.requestBody — the same
	// DEDICATED request-matching bucket HTTP consumes (see http/decode.go for
	// why the response "body" bucket must not soften request matching).
	// Lowercased, presence-only: request-body matching and drift detection are
	// path-based, a value-regex cannot gate here.
	var userBodyNoise map[string][]string
	if bn, ok := opts.NoiseConfig["requestbody"]; ok {
		userBodyNoise = make(map[string][]string, len(bn))
		for k := range bn {
			userBodyNoise[strings.ToLower(k)] = []string{}
		}
	}

	commandCount := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			commandCount++

			// logger.Debug("Starting new command iteration",
			// zap.Int("command_count", commandCount))

			// Set a read deadline on the client connection.
			// opts.SQLDelay is a time.Duration; multiplying by time.Second (the old
			// code) either produced 0 when the caller sent SQLDelay=0 or overflowed
			// int64 when the caller sent a real seconds-valued Duration — both cases
			// expired the deadline immediately and hot-looped at 50ms/iter forever.
			readTimeout := 2 * opts.SQLDelay
			if readTimeout < time.Second {
				readTimeout = 2 * time.Second
			}
			err := clientConn.SetReadDeadline(time.Now().Add(readTimeout))
			if err != nil {
				utils.LogError(logger, err, "failed to set read deadline on client conn")
				return err
			}

			// logger.Debug("About to read next command from client",
			// 	zap.Int("command_count", commandCount),
			// 	zap.Duration("read_timeout", readTimeout))

			// read the command from the client
			command, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Idle wait: keep the connection open and continue polling
					// logger.Debug("read timeout waiting for next client command; keeping connection open")
					// Optional: back off a bit to avoid hot loop
					time.Sleep(50 * time.Millisecond)
					// Clear deadline or set another future deadline, then keep looping
					_ = clientConn.SetReadDeadline(time.Now().Add(readTimeout))
					continue
				}
				if err == io.EOF {
					logger.Debug("client closed the connection (EOF)")
				} else {
					utils.LogError(logger, err, "failed to read command packet from client")
				}
				return err
			}

			logger.Debug("Successfully read command from client",
				zap.Int("command_count", commandCount),
				zap.Int("command_size_bytes", len(command)))

			// reset the read deadline
			err = clientConn.SetReadDeadline(time.Time{})
			if err != nil {
				utils.LogError(logger, err, "failed to reset read deadline on client conn")
				return err
			}

			// Decode the command
			commandPkt, err := wire.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}

			req := mysql.Request{
				PacketBundle: *commandPkt,
			}

			// Match the request with the mock
			resp, ok, miss, err := matchCommand(ctx, logger, req, mockDb, decodeCtx, noiseEngine, userBodyNoise)
			if err != nil {
				if err == io.EOF {
					logger.Debug("Connection closing due to EOF from matchCommand",
						zap.Int("commands_processed", commandCount),
						zap.String("request_type", req.Header.Type))
					return io.EOF
				}
				logger.Error("Connection closing due to match command error",
					zap.Error(err),
					zap.Int("commands_processed", commandCount),
					zap.String("request_type", req.Header.Type))
				utils.LogError(logger, err, "failed to match the command")
				return err
			}

			if !ok {
				// Build mismatch report for propagation
				if miss == nil {
					// Defensive: matchCommand always returns a miss alongside a
					// nil error. matchPhase is set explicitly so an unexpected
					// path cannot masquerade as an empty mock pool.
					miss = &mockMiss{matchPhase: models.MatchPhaseExhausted}
				}
				// Report the STATEMENT, not the wire text. A Datadog DBM /
				// sqlcommenter comment runs to a couple of hundred bytes in front
				// of the SQL, so every truncation below would otherwise clip
				// inside it and the report would name no SQL at all — which is how
				// this class of miss gets read as "the mock was never recorded"
				// instead of "this query drifted".
				//
				// The comment stripper is applied to SQL ONLY, never to the
				// rendered bound parameters appended below: those are arbitrary
				// user data, and a value containing "#" or "-- " would have the
				// rest of the parameter list silently deleted from the report.
				actualQuery := ""
				switch msg := req.Message.(type) {
				case *mysql.QueryPacket:
					actualQuery = sqlStatementIdentity(msg.Query)
				case *mysql.StmtPreparePacket:
					actualQuery = sqlStatementIdentity(msg.Query)
				case *mysql.StmtExecutePacket:
					// EXECUTE carries no SQL text of its own — resolve the
					// prepared query via the runtime stmtID map and show the
					// bound parameter values, so the report isn't a blank
					// "COM_STMT_EXECUTE".
					if msg != nil {
						if decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
							actualQuery = sqlStatementIdentity(decodeCtx.StmtIDToQuery[msg.StatementID])
						}
						actualQuery = strings.TrimSpace(actualQuery + " " + formatExecParams(msg.Parameters))
					}
				case *mysql.StmtSendLongDataPacket:
					if msg != nil {
						actualQuery = fmt.Sprintf("(param %d, %d streamed bytes)", msg.ParameterID, len(msg.Data))
					}
				}
				closestQuery := sqlStatementIdentity(miss.closestQuery)
				diff := ""
				if actualQuery != "" || closestQuery != "" {
					diff = fmt.Sprintf("actual: %s\nclosest: %s", truncate(actualQuery, 200), truncate(closestQuery, 200))
				}
				// Under schemaNoiseStrict a rejection is a drift verdict, not a
				// missing recording — say WHICH fields drifted (FieldDiffs) and
				// how to mark them, instead of the generic re-record hint.
				nextSteps := "Re-record mocks if the SQL query has changed."
				if miss.strictRejected > 0 {
					nextSteps = fmt.Sprintf("mock-noise strict rejected %d candidate mock(s): the listed request fields drifted outside configured/learned noise. Mark them under test.globalNoise.requestBody, run once with --schema-noise-detection to learn them, or re-record.", miss.strictRejected)
				}
				report := &models.MockMismatchReport{
					Protocol:      "MySQL",
					ActualSummary: strings.TrimSpace(fmt.Sprintf("%s %s", req.Header.Type, truncate(actualQuery, 160))),
					ClosestMock:   miss.closestMock,
					Diff:          diff,
					FieldDiffs:    miss.fieldDiffs,
					NextSteps:     nextSteps,
					MatchPhase:    miss.matchPhase,
					// Both of these used to be left at their zero value while the
					// log printed them as if measured, so every MySQL miss
					// reported "match_phase":"" and "candidates":0 — indistinguishable
					// from an empty mock pool.
					CandidateCount: miss.candidateCount,
				}
				if miss.matchPhase == models.MatchPhaseNoMocks {
					// The pool held no data mock for the command phase to compare
					// against — only handshake/config mocks survived the lifetime
					// filter. That is a recording-side shortfall, not a content
					// mismatch.
					//
					// This is asserted on the MEASURED phase, never on an empty
					// closest_mock: nothing scoring is the NORMAL shape of a query
					// that merely drifted from its recording, and reporting that as
					// a lost mock sends the reader hunting for the wrong bug.
					logger.Error("REPLAY-ORPHAN: TC failing — no data mock available for this query, so it was never recorded or was dropped at record time",
						zap.String("sql_query", truncate(actualQuery, 300)),
						zap.String("request_type", req.Header.Type),
						zap.Int("commands_processed", commandCount),
						zap.String("hint", "check mappings.yaml: this test set has no MySQL data mock for the command phase — either the query was never recorded, or its mock was dropped at record time (teardown lag or memory pressure)"),
					)
				}
				// next_step reuses the strict-aware guidance computed for the
				// mismatch report above — under schemaNoiseStrict the accurate
				// advice is "mark/learn the drifted fields", not "re-record".
				logger.Error("Connection closing due to no matching mock found.",
					zap.Int("commands_processed", commandCount),
					zap.String("request_type", req.Header.Type),
					zap.String("closest_mock", miss.closestMock),
					zap.Int("candidates", miss.candidateCount),
					zap.String("match_phase", miss.matchPhase),
					zap.Int("strict_rejected_candidates", miss.strictRejected),
					zap.String("next_step", nextSteps))
				baseErr := fmt.Errorf("error while simulating the command phase: %w", models.ErrNoMockMatched)
				return models.NewMockMismatchError(baseErr, report)
			}

			logger.Debug("Matched the command with the mock", zap.Any("mock", resp))

			// Handle prepared statement cleanup for COM_STMT_CLOSE
			if commandPkt.Header.Type == mysql.CommandStatusToString(mysql.COM_STMT_CLOSE) {
				if closePacket, ok := commandPkt.Message.(*mysql.StmtClosePacket); ok {
					delete(decodeCtx.PreparedStatements, closePacket.StatementID)
					if decodeCtx.StmtIDToQuery != nil {
						delete(decodeCtx.StmtIDToQuery, closePacket.StatementID)
					}
					logger.Debug("Cleaned up prepared statement", zap.Uint32("StatementID", closePacket.StatementID))
				}
			}

			// We could have just returned before matching the command for no response commands.
			// But we need to remove the corresponding mock from the mockDb for no response commands.
			if wire.IsNoResponseCommand(commandPkt.Header.Type) {
				// No response for COM_STMT_CLOSE and COM_STMT_SEND_LONG_DATA
				logger.Debug("No response for the command", zap.Any("command", command))
				continue
			}

			//Encode the matched resp
			buf, err := wire.EncodeToBinary(ctx, logger, &resp.PacketBundle, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to encode the response", zap.Any("response", resp))
				return err
			}

			logger.Debug("About to write response to client",
				zap.String("request_type", req.Header.Type),
				zap.String("response_type", resp.Header.Type),
				zap.Int("response_size_bytes", len(buf)),
				zap.Int("commands_processed", commandCount))

			// Write the response to the client
			_, err = clientConn.Write(buf)
			if err != nil {
				if ctx.Err() != nil {
					logger.Debug("context done while writing the response to the client", zap.Error(ctx.Err()))
					return ctx.Err()
				}
				logger.Error("Failed to write response to client",
					zap.Error(err),
					zap.String("request_type", req.Header.Type),
					zap.Int("commands_processed", commandCount))
				utils.LogError(logger, err, "failed to write the response to the client")
				return err
			}

			logger.Debug("successfully wrote the response to the client",
				zap.Any("request", req.Header.Type),
				zap.String("response_type", resp.Header.Type),
				zap.Int("response_size_bytes", len(buf)),
				zap.Int("commands_processed", commandCount))

			// Check connection state after writing large response
			if len(buf) > 1000 {
				logger.Debug("Large response written, checking connection state",
					zap.Int("response_size_bytes", len(buf)),
					zap.String("request_type", req.Header.Type))
			}

			// Add a small delay and log to see if connection is still alive
			logger.Debug("Response write completed, continuing to next iteration",
				zap.Int("commands_processed", commandCount),
				zap.String("last_request", req.Header.Type))
		}
	}
}
