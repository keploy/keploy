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

// simulateCommandPhase serves the client's commands from the mocks until the
// client leaves, or a command misses. dst is the upstream the client dialled,
// named in a miss's report ("" when unknown).
func simulateCommandPhase(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {

	// Log initial mock state at the start of command phase
	total, cfg, data := mockDb.GetMySQLCounts()
	logger.Debug("Command phase starting",
		zap.Int("total_mysql_mocks", total),
		zap.Int("config_mocks", cfg),
		zap.Int("data_mocks_available", data))

	// Shared mock-noise engine for this connection's command phase, built from
	// MySQL's own declared adapter — as HTTP builds it from h.MockNoise()
	// (http/match.go) and as this package's drift engine does (schema_noise.go).
	// Building it here, rather than reading one injected on ctx, keeps the
	// replayer self-contained: a caller that drives it directly (not through the
	// proxy's injection) still gets noise detection instead of a silent nil engine.
	noiseEngine := mocknoise.New(mysqlNoiseAdapter{}, opts.NoiseDetection(), opts.NoiseStrict())

	// User request-body noise from test.globalNoise.requestBody — the same
	// DEDICATED request-matching bucket HTTP consumes (see http/decode.go for
	// why the response "body" bucket must not soften request matching).
	userBodyNoise := requestBodyNoise(opts.NoiseConfig)

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

			if ce := logger.Check(zap.DebugLevel, "Successfully read command from client"); ce != nil {
				ce.Write(zap.Int("command_count", commandCount),
					zap.Int("command_size_bytes", len(command)))
			}

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
					// path cannot masquerade as an empty mock pool. It read no
					// pool's places, so it shows the command redacted whole.
					miss = &mockMiss{matchPhase: models.MatchPhaseExhausted, places: everyPlace}
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
				actualQuery := miss.liveCommand(req.Message, decodeCtx)
				closestQuery := sqlStatementIdentity(miss.closestQuery)
				diff := ""
				if actualQuery != "" || closestQuery != "" {
					diff = fmt.Sprintf("actual: %s\nclosest: %s", truncate(actualQuery, 200), truncate(closestQuery, 200))
				}
				if miss.why != "" {
					diff = strings.TrimSpace(diff + "\n" + miss.why)
				}
				// Under schemaNoiseStrict a rejection is a drift verdict, not a
				// missing recording — say WHICH fields drifted (FieldDiffs) and
				// how to mark them, instead of the generic re-record hint.
				nextSteps := "Re-record mocks if the SQL query has changed."
				switch {
				case miss.nextSteps != "":
					nextSteps = miss.nextSteps
				case miss.strictRejected > 0:
					nextSteps = fmt.Sprintf("schema-noise strict rejected %d candidate mock(s): the listed request fields drifted outside configured/learned noise. Mark them under test.globalNoise.requestBody, run once with --schema-noise-detection to learn them, or re-record.", miss.strictRejected)
				}
				report := &models.MockMismatchReport{
					At:            time.Now(),
					Protocol:      "MySQL",
					ActualSummary: strings.TrimSpace(fmt.Sprintf("%s %s", req.Header.Type, truncate(actualQuery, 160))),
					Destination:   dst,
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
				// The miss was decided where the command first read the
				// round: whether that read found a staging is told from it.
				integrations.StampMark(report, miss.call)
				if miss.extraCall > 0 {
					report.MissKind, report.Recorded = models.MissExtraCall, miss.extraCall
				}
				if miss.matchPhase == models.MatchPhaseNoMocks && !miss.restaged && miss.extraCall == 0 {
					// The pool held no data mock for the command phase to compare
					// against — at most handshake/config mocks survived the
					// lifetime filter, or the pool was empty. That is a
					// recording-side shortfall, not a content mismatch.
					//
					// This is asserted on the MEASURED phase, never on an empty
					// closest_mock: nothing scoring is the NORMAL shape of a query
					// that merely drifted from its recording, and reporting that as
					// a lost mock sends the reader hunting for the wrong bug.
					//
					// Nor is a command the replay staged a test set again under
					// (restaged): its matcher, held to its first read, read
					// nothing, and the miss says so.
					logger.Error("REPLAY-ORPHAN: TC failing — no data mock available for this query, so it was never recorded or was dropped at record time",
						zap.String("sql_query", liveCommandShape(req.Message, decodeCtx)),
						zap.String("request_type", req.Header.Type),
						zap.Int("commands_processed", commandCount),
						zap.String("hint", "check mappings.yaml: this test set has no MySQL data mock for the command phase — either the query was never recorded, or its mock was dropped at record time (teardown lag or memory pressure)"),
					)
				}
				baseErr := fmt.Errorf("error while simulating the command phase: %w", models.ErrNoMockMatched)
				missErr := models.NewMockMismatchError(baseErr, report)
				// next_step reuses the strict-aware guidance computed for the
				// mismatch report above — under schemaNoiseStrict the accurate
				// advice is "mark/learn the drifted fields", not "re-record".
				missFields := []zap.Field{
					zap.Int("commands_processed", commandCount),
					zap.String("request_type", req.Header.Type),
					zap.String("closest_mock", miss.closestMock),
					zap.Int("candidates", miss.candidateCount),
					zap.String("match_phase", miss.matchPhase),
					zap.Int("strict_rejected_candidates", miss.strictRejected),
					zap.String("next_step", nextSteps),
				}
				// The connection serves on after a miss: a statement the
				// client closed is forgotten whether or not its close matched.
				forgetClosedStatement(logger, commandPkt, decodeCtx)
				answered, err := answerMiss(ctx, logger, clientConn, req, decodeCtx, missErr)
				if !answered {
					logger.Error("Connection closing due to no matching mock found.", missFields...)
					return err
				}
				logger.Warn("No matching mock found for the command; answered it with a MySQL error and kept the connection open.", missFields...)
				continue
			}

			// The answer's kind, never the answer: it holds the recorded rows.
			if ce := logger.Check(zap.DebugLevel, "Matched the command with the mock"); ce != nil {
				ce.Write(zap.String("response_type", responseKind(resp)))
			}

			forgetClosedStatement(logger, commandPkt, decodeCtx)

			// We could have just returned before matching the command for no response commands.
			// But we need to remove the corresponding mock from the mockDb for no response commands.
			if wire.IsNoResponseCommand(commandPkt.Header.Type) {
				// No response for COM_STMT_CLOSE and COM_STMT_SEND_LONG_DATA
				// Its type and size, not its bytes: a SEND_LONG_DATA's are a
				// bound value, a secret wherever a recording redacted one.
				if ce := logger.Check(zap.DebugLevel, "No response for the command"); ce != nil {
					ce.Write(zap.String("command_type", commandPkt.Header.Type), zap.Int("command_size_bytes", len(command)))
				}
				continue
			}

			//Encode the matched resp
			buf, err := wire.EncodeToBinary(ctx, logger, &resp.PacketBundle, clientConn, decodeCtx)
			if err != nil {
				// The encoder's errors quote recorded values (a row's
				// column it cannot write): withheld.
				err = utils.WithholdInput(err)
				utils.LogError(logger, err, "failed to encode the response", zap.String("response_type", responseKind(resp)))
				return err
			}

			if ce := logger.Check(zap.DebugLevel, "About to write response to client"); ce != nil {
				ce.Write(zap.String("request_type", req.Header.Type),
					zap.String("response_type", resp.Header.Type),
					zap.Int("response_size_bytes", len(buf)),
					zap.Int("commands_processed", commandCount))
			}

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

			if ce := logger.Check(zap.DebugLevel, "successfully wrote the response to the client"); ce != nil {
				ce.Write(zap.Any("request", req.Header.Type),
					zap.String("response_type", resp.Header.Type),
					zap.Int("response_size_bytes", len(buf)),
					zap.Int("commands_processed", commandCount))
			}

			// Check connection state after writing large response
			if len(buf) > 1000 {
				if ce := logger.Check(zap.DebugLevel, "Large response written, checking connection state"); ce != nil {
					ce.Write(zap.Int("response_size_bytes", len(buf)),
						zap.String("request_type", req.Header.Type))
				}
			}

			if ce := logger.Check(zap.DebugLevel, "Response write completed, continuing to next iteration"); ce != nil {
				ce.Write(zap.Int("commands_processed", commandCount),
					zap.String("last_request", req.Header.Type))
			}
		}
	}
}

// requestBodyNoise is the requestBody bucket of the user's noise
// (test.globalNoise) as the matcher compares it: lowercased, root-relative
// (parameters.0.value), each entry with its patterns (an entry with
// patterns excuses a field only when the recorded and the live value each
// match one; one with none, whatever it holds). A key may also
// be written as a miss lists the field, with its leading "body."
// (body.parameters.0.value): a MySQL command's body has no field named
// body, so the prefix only ever names the body itself. Without it, the
// field a miss lists, copied as listed, changed nothing.
func requestBodyNoise(cfg map[string]map[string][]string) map[string][]string {
	bn, ok := cfg["requestbody"]
	if !ok {
		return nil
	}
	out := make(map[string][]string, len(bn))
	for k, v := range bn {
		out[strings.TrimPrefix(strings.ToLower(k), "body.")] = append([]string{}, v...)
	}
	return out
}

// forgetClosedStatement drops the prepared statement a COM_STMT_CLOSE closes
// from the connection's decode state. It runs once the close has been
// matched, or has missed: the matcher reads the statement's query to match
// the close.
func forgetClosedStatement(logger *zap.Logger, commandPkt *mysql.PacketBundle, decodeCtx *wire.DecodeContext) {
	if commandPkt.Header.Type != mysql.CommandStatusToString(mysql.COM_STMT_CLOSE) {
		return
	}
	closePacket, ok := commandPkt.Message.(*mysql.StmtClosePacket)
	if !ok {
		return
	}
	delete(decodeCtx.PreparedStatements, closePacket.StatementID)
	if decodeCtx.StmtIDToQuery != nil {
		delete(decodeCtx.StmtIDToQuery, closePacket.StatementID)
	}
	if ce := logger.Check(zap.DebugLevel, "Cleaned up prepared statement"); ce != nil {
		ce.Write(zap.Uint32("StatementID", closePacket.StatementID))
	}
}

// The error a command with no matching mock is answered with:
// ER_UNKNOWN_ERROR, SQLSTATE HY000, the server's generic failure of one
// statement. A client library raises it as that statement's error and keeps
// its connection.
const (
	missErrCode    uint16 = 1105
	missErrState          = "HY000"
	missErrMessage        = "keploy: no recorded mock matches this command"
)

// answerMiss answers a command that matched no mock as a server answers a
// statement it cannot run: with an ERR packet, on a connection that stays
// open. It reports the miss first (models.ReportMockMismatch), so the miss is
// queued for the test that made it before the app can read the answer and
// end that test.
//
// Closing the connection instead, as the replay did, made one unmatched
// statement cost the app its connection. Inside a transaction the client may
// not reconnect, so every later statement of the request failed with it: a
// Rails request whose best-effort UPDATE drifted from its recording returned
// 500 (keploy/api-server#2514), and a pool such as Makara then blacklisted
// the connection for the requests after it.
//
// It returns answered false, and the miss as err, when the miss cannot be
// reported without returning it (no reporter on ctx): the connection ends as
// before, so no miss goes unreported. A command the client expects no answer
// to (COM_STMT_CLOSE, COM_STMT_SEND_LONG_DATA) is reported and left
// unanswered: an ERR packet there would be read as the answer to the client's
// next command. (COM_QUIT never misses: matchCommand ends the connection on
// it.)
// When the ERR packet cannot be written the connection ends: answered is
// false, and err is the miss marked reported (models.MismatchReported), so the
// proxy logs it and does not queue it a second time.
func answerMiss(ctx context.Context, logger *zap.Logger, clientConn net.Conn, req mysql.Request, decodeCtx *wire.DecodeContext, missErr error) (answered bool, err error) {
	if !models.ReportMockMismatch(ctx, missErr) {
		return false, missErr
	}
	if wire.IsNoResponseCommand(req.Header.Type) {
		return true, nil
	}
	// The answer's sequence id is one past the command's, as every answer's.
	var seq uint8 = 1
	if req.Header != nil && req.Header.Header != nil {
		seq = req.Header.Header.SequenceID + 1
	}
	errPkt := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{Type: mysql.StatusToString(mysql.ERR), Header: &mysql.Header{SequenceID: seq}},
		Message: &mysql.ERRPacket{
			Header:         mysql.ERR,
			ErrorCode:      missErrCode,
			SQLStateMarker: "#",
			SQLState:       missErrState,
			ErrorMessage:   missErrMessage,
		},
	}
	buf, err := wire.EncodeToBinary(ctx, logger, errPkt, clientConn, decodeCtx)
	if err == nil {
		_, err = clientConn.Write(buf)
	}
	if err != nil {
		logger.Debug("could not answer a command with no matching mock with an error", zap.Error(err))
		return false, models.MismatchReported{Err: missErr}
	}
	return true, nil
}
