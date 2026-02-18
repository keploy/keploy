package recorder

import (
"context"
"fmt"
"net"
"time"

mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
"go.keploy.io/server/v3/pkg/models"
"go.keploy.io/server/v3/pkg/models/mysql"
"go.uber.org/zap"
)

// ProcessRawMocks is the legacy async worker for the old pipeline.
// Kept for backward compatibility with existing tests.
func ProcessRawMocks(ctx context.Context, logger *zap.Logger, rawMocks <-chan *models.Mock, finalMocks chan<- *models.Mock) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic in ProcessRawMocks", zap.Any("panic", r))
		}
	}()

	for mock := range rawMocks {
		if mock == nil {
			continue
		}

		if err := processMock(ctx, logger, mock); err != nil {
			logger.Error("failed to process mock in async worker", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case finalMocks <- mock:
		}
	}
}

// ProcessRawMocksV2 is the decoder goroutine for the "Capture & Defer"
// architecture.  It receives RawMockEntry values (raw MySQL packet bytes),
// fully decodes them using the existing wire/* decoders, builds models.Mock
// objects, and sends them to the final mocks channel.
//
// This is the ONLY goroutine that creates rich Go structs / does string
// conversion.  It runs completely decoupled from the forwarding path.
func ProcessRawMocksV2(ctx context.Context, logger *zap.Logger, rawMocks <-chan RawMockEntry, finalMocks chan<- *models.Mock, opts models.OutgoingOptions) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered from panic in ProcessRawMocksV2", zap.Any("panic", r))
		}
	}()

	// Per-connection decode context for the decoder goroutine.
	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		LastOpValue:        wire.RESET,
	}

	// We use a nil net.Conn as the key for map-based state in the decode context.
	// This is fine because all packets for this connection flow through one goroutine.
	var connKey net.Conn

	connID := ""
	if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
		connID = v.(string)
	}

	for entry := range rawMocks {
		if ctx.Err() != nil {
			return
		}

		mock, err := decodeRawMockEntry(ctx, logger, entry, decodeCtx, connKey)
		if err != nil {
			logger.Error("failed to decode raw mock entry",
zap.Error(err), zap.String("mockType", entry.MockType))
			continue
		}

		// Set the connection ID in metadata.
		if mock.Spec.Metadata == nil {
			mock.Spec.Metadata = make(map[string]string)
		}
		mock.Spec.Metadata["connID"] = connID

		recordMockDirect(ctx, mock, finalMocks, opts)
	}
}

// decodeRawMockEntry fully decodes a RawMockEntry into a models.Mock.
func decodeRawMockEntry(ctx context.Context, logger *zap.Logger, entry RawMockEntry, decodeCtx *wire.DecodeContext, connKey net.Conn) (*models.Mock, error) {
	requests := make([]mysql.Request, 0, len(entry.ReqPackets))
	responses := make([]mysql.Response, 0, len(entry.RespPackets))

	var requestOperation string
	var responseOperation string

	// For the handshake config mock, we use the slow decoder since it handles
	// the full protocol state machine.  For command-phase mocks, we use the
	// fast decoder.
	isConfig := entry.MockType == "config"

	if isConfig {
		// Decode all handshake packets using the state-tracking decoder.
		for _, pkt := range entry.ReqPackets {
			decoded, err := wire.DecodePayload(ctx, logger, pkt, connKey, decodeCtx)
			if err != nil {
				logger.Warn("failed to decode handshake request packet", zap.Error(err))
				// Fall back to raw base64 representation.
				decoded = rawPacketBundle(pkt)
			}
			requests = append(requests, mysql.Request{PacketBundle: *decoded})
			requestOperation = decoded.Header.Type
		}
		for _, pkt := range entry.RespPackets {
			decoded, err := wire.DecodePayload(ctx, logger, pkt, connKey, decodeCtx)
			if err != nil {
				logger.Warn("failed to decode handshake response packet", zap.Error(err))
				decoded = rawPacketBundle(pkt)
			}
			responses = append(responses, mysql.Response{PacketBundle: *decoded})
			responseOperation = decoded.Header.Type

			// If this is a HandshakeV10, store the server greeting in context.
			if sg, ok := decoded.Message.(*mysql.HandshakeV10Packet); ok {
				decodeCtx.ServerGreetings.Store(connKey, sg)
				decodeCtx.ServerGreeting = sg
			}
		}
	} else {
		// Command-phase: decode the command and its response.
		requestOperation, responseOperation = decodeCommandPhase(ctx, logger, entry, decodeCtx, connKey, &requests, &responses)
	}

	mock := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Name:    entry.MockType,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":              entry.MockType,
				"requestOperation":  requestOperation,
				"responseOperation": responseOperation,
			},
			MySQLRequests:    requests,
			MySQLResponses:   responses,
			Created:          time.Now().Unix(),
			ReqTimestampMock: entry.ReqTimestamp,
			ResTimestampMock: entry.ResTimestamp,
		},
	}

	return mock, nil
}

// decodeCommandPhase decodes command-phase packets (requests and responses)
// using the fast decoder and handles result-set assembly.
func decodeCommandPhase(
ctx context.Context,
logger *zap.Logger,
entry RawMockEntry,
decodeCtx *wire.DecodeContext,
connKey net.Conn,
requests *[]mysql.Request,
responses *[]mysql.Response,
) (requestOp, responseOp string) {
	// Start with a clean last-op for each command.
	decodeCtx.LastOpValue = wire.RESET

	// ── Decode request packets ──
	for _, pkt := range entry.ReqPackets {
		decoded, err := wire.DecodePayloadFast(ctx, logger, pkt, decodeCtx)
		if err != nil {
			logger.Warn("failed to decode command request", zap.Error(err))
			decoded = rawPacketBundle(pkt)
		}
		*requests = append(*requests, mysql.Request{PacketBundle: *decoded})
		requestOp = decoded.Header.Type
	}

	if len(entry.RespPackets) == 0 {
		responseOp = "NO Response Packet"
		return
	}

	// ── Decode response packets ──
	// The first response packet determines the response type.
	firstResp := entry.RespPackets[0]
	if len(firstResp) < 5 {
		responseOp = "UNKNOWN"
		return
	}
	marker := firstResp[4]

	switch {
	case marker == mysql.OK:
		if entry.CmdType == mysql.COM_STMT_PREPARE {
			// COM_STMT_PREPARE_OK + param/column definitions.
			responseOp = decodeStmtPrepareOkResponse(ctx, logger, entry, decodeCtx, responses)
		} else {
			// Simple OK response.
			decoded, err := wire.DecodePayloadFast(ctx, logger, firstResp, decodeCtx)
			if err != nil {
				logger.Warn("failed to decode OK response", zap.Error(err))
				decoded = rawPacketBundle(firstResp)
			}
			*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
			responseOp = decoded.Header.Type
		}

	case marker == mysql.ERR:
		decoded, err := wire.DecodePayloadFast(ctx, logger, firstResp, decodeCtx)
		if err != nil {
			logger.Warn("failed to decode ERR response", zap.Error(err))
			decoded = rawPacketBundle(firstResp)
		}
		*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
		responseOp = decoded.Header.Type

	case marker == mysql.EOF && payloadLen(firstResp) < 9:
		decoded, err := wire.DecodePayloadFast(ctx, logger, firstResp, decodeCtx)
		if err != nil {
			decoded = rawPacketBundle(firstResp)
		}
		*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
		responseOp = decoded.Header.Type

	default:
		// Result set — decode metadata + assemble columns/rows.
		switch entry.CmdType {
		case mysql.COM_QUERY:
			responseOp = decodeTextResultSetResponse(ctx, logger, entry, decodeCtx, responses)
		case mysql.COM_STMT_EXECUTE:
			responseOp = decodeBinaryResultSetResponse(ctx, logger, entry, decodeCtx, responses)
		default:
			// Unknown response type - use raw
			decoded, err := wire.DecodePayloadFast(ctx, logger, firstResp, decodeCtx)
			if err != nil {
				decoded = rawPacketBundle(firstResp)
			}
			*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
			responseOp = decoded.Header.Type
		}
	}

	// Reset last op after processing the response.
	decodeCtx.LastOpValue = wire.RESET

	return
}

// decodeTextResultSetResponse assembles a TextResultSet from raw packets.
func decodeTextResultSetResponse(ctx context.Context, logger *zap.Logger, entry RawMockEntry, decodeCtx *wire.DecodeContext, responses *[]mysql.Response) string {
	if len(entry.RespPackets) == 0 {
		return "UNKNOWN"
	}

	// First packet: column count.
	firstPkt := entry.RespPackets[0]
	decoded, err := wire.DecodePayloadFast(ctx, logger, firstPkt, decodeCtx)
	if err != nil {
		logger.Warn("failed to decode result set metadata", zap.Error(err))
		decoded = rawPacketBundle(firstPkt)
	}

	textRes, ok := decoded.Message.(*mysql.TextResultSet)
	if !ok {
		// Fallback: treat as a generic response.
		*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
		return decoded.Header.Type
	}

	colCount := textRes.ColumnCount
	pktIdx := 1 // start after the column-count packet

	// Decode column definitions.
	textRes.Columns = make([]*mysql.ColumnDefinition41, 0, colCount)
	for i := uint64(0); i < colCount && pktIdx < len(entry.RespPackets); i++ {
		col := safeDecodeColumn(ctx, logger, entry.RespPackets[pktIdx])
		if col != nil {
			textRes.Columns = append(textRes.Columns, col)
		}
		pktIdx++
	}

	// EOF after columns (if not CLIENT_DEPRECATE_EOF).
	hasEOF := false
	if pktIdx < len(entry.RespPackets) {
		pkt := entry.RespPackets[pktIdx]
		if len(pkt) >= 5 && pkt[4] == mysql.EOF && payloadLen(pkt) < 9 {
			textRes.EOFAfterColumns = pkt
			pktIdx++
			hasEOF = true
		}
	}
	_ = hasEOF

	// Decode rows until the final EOF/OK.
	textRes.Rows = make([]*mysql.TextRow, 0)
	for pktIdx < len(entry.RespPackets) {
		pkt := entry.RespPackets[pktIdx]
		pktIdx++

		// Check for final EOF.
		if len(pkt) >= 5 && pkt[4] == mysql.EOF && payloadLen(pkt) < 9 {
			textRes.FinalResponse = &mysql.GenericResponse{
				Data: pkt,
				Type: mysql.StatusToString(mysql.EOF),
			}
			break
		}

		// Check for final OK (CLIENT_DEPRECATE_EOF).
		if len(pkt) >= 5 && pkt[4] == mysql.OK && payloadLen(pkt) < 64 {
			textRes.FinalResponse = &mysql.GenericResponse{
				Data: pkt,
				Type: mysql.StatusToString(mysql.OK),
			}
			break
		}

		row := safeDecodeTextRow(ctx, logger, pkt, textRes.Columns)
		if row != nil {
			textRes.Rows = append(textRes.Rows, row)
		}
	}

	*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
	return decoded.Header.Type
}

// decodeBinaryResultSetResponse assembles a BinaryProtocolResultSet from raw packets.
func decodeBinaryResultSetResponse(ctx context.Context, logger *zap.Logger, entry RawMockEntry, decodeCtx *wire.DecodeContext, responses *[]mysql.Response) string {
	if len(entry.RespPackets) == 0 {
		return "UNKNOWN"
	}

	firstPkt := entry.RespPackets[0]
	decoded, err := wire.DecodePayloadFast(ctx, logger, firstPkt, decodeCtx)
	if err != nil {
		logger.Warn("failed to decode binary result set metadata", zap.Error(err))
		decoded = rawPacketBundle(firstPkt)
	}

	binRes, ok := decoded.Message.(*mysql.BinaryProtocolResultSet)
	if !ok {
		*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
		return decoded.Header.Type
	}

	colCount := binRes.ColumnCount
	pktIdx := 1

	// Decode column definitions.
	binRes.Columns = make([]*mysql.ColumnDefinition41, 0, colCount)
	for i := uint64(0); i < colCount && pktIdx < len(entry.RespPackets); i++ {
		col := safeDecodeColumn(ctx, logger, entry.RespPackets[pktIdx])
		if col != nil {
			binRes.Columns = append(binRes.Columns, col)
		}
		pktIdx++
	}

	// EOF after columns.
	if pktIdx < len(entry.RespPackets) {
		pkt := entry.RespPackets[pktIdx]
		if len(pkt) >= 5 && pkt[4] == mysql.EOF && payloadLen(pkt) < 9 {
			binRes.EOFAfterColumns = pkt
			pktIdx++
		}
	}

	// Decode rows.
	binRes.Rows = make([]*mysql.BinaryRow, 0)
	for pktIdx < len(entry.RespPackets) {
		pkt := entry.RespPackets[pktIdx]
		pktIdx++

		if len(pkt) >= 5 && pkt[4] == mysql.EOF && payloadLen(pkt) < 9 {
			binRes.FinalResponse = &mysql.GenericResponse{
				Data: pkt,
				Type: mysql.StatusToString(mysql.EOF),
			}
			break
		}

		if len(pkt) >= 5 && pkt[4] == mysql.OK && payloadLen(pkt) < 64 {
			binRes.FinalResponse = &mysql.GenericResponse{
				Data: pkt,
				Type: mysql.StatusToString(mysql.OK),
			}
			break
		}

		row := safeDecodeBinaryRow(ctx, logger, pkt, binRes.Columns)
		if row != nil {
			binRes.Rows = append(binRes.Rows, row)
		}
	}

	*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
	return decoded.Header.Type
}

// decodeStmtPrepareOkResponse assembles a StmtPrepareOk response from raw packets.
func decodeStmtPrepareOkResponse(ctx context.Context, logger *zap.Logger, entry RawMockEntry, decodeCtx *wire.DecodeContext, responses *[]mysql.Response) string {
	if len(entry.RespPackets) == 0 {
		return "UNKNOWN"
	}

	firstPkt := entry.RespPackets[0]
	decoded, err := wire.DecodePayloadFast(ctx, logger, firstPkt, decodeCtx)
	if err != nil {
		logger.Warn("failed to decode stmt prepare ok", zap.Error(err))
		decoded = rawPacketBundle(firstPkt)
		*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
		return decoded.Header.Type
	}

	prepRes, ok := decoded.Message.(*mysql.StmtPrepareOkPacket)
	if !ok {
		*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
		return decoded.Header.Type
	}

	pktIdx := 1

	// Decode param definitions.
	if prepRes.NumParams > 0 {
		prepRes.ParamDefs = make([]*mysql.ColumnDefinition41, 0, prepRes.NumParams)
		for i := uint16(0); i < prepRes.NumParams && pktIdx < len(entry.RespPackets); i++ {
			col := safeDecodeColumn(ctx, logger, entry.RespPackets[pktIdx])
			if col != nil {
				prepRes.ParamDefs = append(prepRes.ParamDefs, col)
			}
			pktIdx++
		}
		// EOF after params.
		if pktIdx < len(entry.RespPackets) {
			pkt := entry.RespPackets[pktIdx]
			if len(pkt) >= 5 && pkt[4] == mysql.EOF {
				prepRes.EOFAfterParamDefs = pkt
				pktIdx++
			}
		}
	}

	// Decode column definitions.
	if prepRes.NumColumns > 0 {
		prepRes.ColumnDefs = make([]*mysql.ColumnDefinition41, 0, prepRes.NumColumns)
		for i := uint16(0); i < prepRes.NumColumns && pktIdx < len(entry.RespPackets); i++ {
			col := safeDecodeColumn(ctx, logger, entry.RespPackets[pktIdx])
			if col != nil {
				prepRes.ColumnDefs = append(prepRes.ColumnDefs, col)
			}
			pktIdx++
		}
		// EOF after columns.
		if pktIdx < len(entry.RespPackets) {
			pkt := entry.RespPackets[pktIdx]
			if len(pkt) >= 5 && pkt[4] == mysql.EOF {
				prepRes.EOFAfterColumnDefs = pkt
				pktIdx++
			}
		}
	}

	// Store prepared statement for future COM_STMT_EXECUTE decoding.
	decodeCtx.PreparedStatements[prepRes.StatementID] = prepRes

	*responses = append(*responses, mysql.Response{PacketBundle: *decoded})
	return decoded.Header.Type
}

// ── Helper functions ─────────────────────────────────────────────────

// rawPacketBundle creates a PacketBundle from raw bytes when decoding fails.
func rawPacketBundle(pkt []byte) *mysql.PacketBundle {
	var hdr mysql.Header
	if len(pkt) >= 4 {
		hdr.PayloadLength = uint32(pkt[0]) | uint32(pkt[1])<<8 | uint32(pkt[2])<<16
		hdr.SequenceID = pkt[3]
	}
	return &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &hdr,
			Type:   fmt.Sprintf("RAW_%#x", safeMarker(pkt)),
		},
		Message: intgUtils.EncodeBase64(pkt),
	}
}

func safeMarker(pkt []byte) byte {
	if len(pkt) >= 5 {
		return pkt[4]
	}
	return 0
}

// processMock is the legacy decode function for the old pipeline.
func processMock(ctx context.Context, logger *zap.Logger, mock *models.Mock) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic in processMock", zap.Any("panic", r))
			err = getRecoverError(r)
		}
	}()

	for i := range mock.Spec.MySQLResponses {
		resp := &mock.Spec.MySQLResponses[i]
		bundle := &resp.PacketBundle

		if textRes, ok := bundle.Message.(*mysql.TextResultSet); ok {
			processTextResultSet(ctx, logger, textRes)
		}
		if binRes, ok := bundle.Message.(*mysql.BinaryProtocolResultSet); ok {
			processBinaryResultSet(ctx, logger, binRes)
		}
		if prepRes, ok := bundle.Message.(*mysql.StmtPrepareOkPacket); ok {
			processStmtPrepareOk(ctx, logger, prepRes)
		}
	}
	return nil
}

func processTextResultSet(ctx context.Context, logger *zap.Logger, textRes *mysql.TextResultSet) {
	if len(textRes.RawColumnData) > 0 {
		textRes.Columns = make([]*mysql.ColumnDefinition41, 0, len(textRes.RawColumnData))
		for _, data := range textRes.RawColumnData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				textRes.Columns = append(textRes.Columns, col)
			}
			mysqlUtils.PutPacketBuffer(data)
		}
		textRes.RawColumnData = nil
	}

	if len(textRes.RawEOFAfterColumns) > 0 {
		textRes.EOFAfterColumns = textRes.RawEOFAfterColumns
		mysqlUtils.PutPacketBuffer(textRes.RawEOFAfterColumns)
		textRes.RawEOFAfterColumns = nil
	}

	if len(textRes.RawRowData) > 0 {
		textRes.Rows = make([]*mysql.TextRow, 0, len(textRes.RawRowData))
		for _, data := range textRes.RawRowData {
			row := safeDecodeTextRow(ctx, logger, data, textRes.Columns)
			if row != nil {
				textRes.Rows = append(textRes.Rows, row)
			}
			mysqlUtils.PutPacketBuffer(data)
		}
		textRes.RawRowData = nil
	}
}

func processBinaryResultSet(ctx context.Context, logger *zap.Logger, binRes *mysql.BinaryProtocolResultSet) {
	if len(binRes.RawColumnData) > 0 {
		binRes.Columns = make([]*mysql.ColumnDefinition41, 0, len(binRes.RawColumnData))
		for _, data := range binRes.RawColumnData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				binRes.Columns = append(binRes.Columns, col)
			}
			mysqlUtils.PutPacketBuffer(data)
		}
		binRes.RawColumnData = nil
	}

	if len(binRes.RawEOFAfterColumns) > 0 {
		binRes.EOFAfterColumns = binRes.RawEOFAfterColumns
		mysqlUtils.PutPacketBuffer(binRes.RawEOFAfterColumns)
		binRes.RawEOFAfterColumns = nil
	}

	if len(binRes.RawRowData) > 0 {
		binRes.Rows = make([]*mysql.BinaryRow, 0, len(binRes.RawRowData))
		for _, data := range binRes.RawRowData {
			row := safeDecodeBinaryRow(ctx, logger, data, binRes.Columns)
			if row != nil {
				binRes.Rows = append(binRes.Rows, row)
			}
			mysqlUtils.PutPacketBuffer(data)
		}
		binRes.RawRowData = nil
	}
}

func processStmtPrepareOk(ctx context.Context, logger *zap.Logger, prepRes *mysql.StmtPrepareOkPacket) {
	if len(prepRes.RawParamData) > 0 {
		prepRes.ParamDefs = make([]*mysql.ColumnDefinition41, 0, len(prepRes.RawParamData))
		for _, data := range prepRes.RawParamData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				prepRes.ParamDefs = append(prepRes.ParamDefs, col)
			}
			mysqlUtils.PutPacketBuffer(data)
		}
		prepRes.RawParamData = nil
	}

	if len(prepRes.RawEOFAfterParamDefs) > 0 {
		prepRes.EOFAfterParamDefs = prepRes.RawEOFAfterParamDefs
		mysqlUtils.PutPacketBuffer(prepRes.RawEOFAfterParamDefs)
		prepRes.RawEOFAfterParamDefs = nil
	}

	if len(prepRes.RawColumnDefData) > 0 {
		prepRes.ColumnDefs = make([]*mysql.ColumnDefinition41, 0, len(prepRes.RawColumnDefData))
		for _, data := range prepRes.RawColumnDefData {
			col := safeDecodeColumn(ctx, logger, data)
			if col != nil {
				prepRes.ColumnDefs = append(prepRes.ColumnDefs, col)
			}
			mysqlUtils.PutPacketBuffer(data)
		}
		prepRes.RawColumnDefData = nil
	}

	if len(prepRes.RawEOFAfterColumnDefs) > 0 {
		prepRes.EOFAfterColumnDefs = prepRes.RawEOFAfterColumnDefs
		mysqlUtils.PutPacketBuffer(prepRes.RawEOFAfterColumnDefs)
		prepRes.RawEOFAfterColumnDefs = nil
	}
}

// safeDecodeColumn wraps DecodeColumn with panic recovery.
func safeDecodeColumn(ctx context.Context, logger *zap.Logger, data []byte) *mysql.ColumnDefinition41 {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic during column decoding", zap.Any("panic", r))
		}
	}()
	col, _, err := rowscols.DecodeColumn(ctx, logger, data)
	if err != nil {
		logger.Error("failed to decode column", zap.Error(err))
		return nil
	}
	return col
}

// safeDecodeTextRow wraps DecodeTextRow with panic recovery.
func safeDecodeTextRow(ctx context.Context, logger *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) *mysql.TextRow {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic during text row decoding", zap.Any("panic", r))
		}
	}()
	row, _, err := rowscols.DecodeTextRow(ctx, logger, data, columns)
	if err != nil {
		logger.Error("failed to decode text row", zap.Error(err))
		return nil
	}
	return row
}

// safeDecodeBinaryRow wraps DecodeBinaryRow with panic recovery.
func safeDecodeBinaryRow(ctx context.Context, logger *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) *mysql.BinaryRow {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Recovered from panic during binary row decoding", zap.Any("panic", r))
		}
	}()
	row, _, err := rowscols.DecodeBinaryRow(ctx, logger, data, columns)
	if err != nil {
		logger.Error("failed to decode binary row", zap.Error(err))
		return nil
	}
	return row
}

func getRecoverError(r interface{}) error {
	if err, ok := r.(error); ok {
		return err
	}
	return nil
}
