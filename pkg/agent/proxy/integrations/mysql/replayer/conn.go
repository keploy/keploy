package replayer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	pUtils "go.keploy.io/server/v3/pkg/agent/proxy/util"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type reqResp struct {
	req  []mysql.Request
	resp []mysql.Response
}

type handshakeRes struct {
	tlsClientConn net.Conn
	// refused is set when the recorded verdict served to the client was a
	// refusal of its login (an ERR): the connection ends there.
	refused bool
}

// Replay mode
func simulateInitialHandshake(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mocks []*models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions, dstCfg *models.ConditionalDstCfg) (handshakeRes, error) {
	// Find a mock whose response bundle opens with the server's
	// HandshakeV10 greeting. Before Phase 2 the session pool only held
	// handshake-bundle mocks (everything else was per-test via the
	// implicit kind-fallback), so mocks[0] was guaranteed to be the
	// handshake. Post-Phase-2 / lax kind-fallback the session pool can
	// also contain lax-promoted data queries (COM_PING, COM_QUERY, etc.)
	// whose bundles have no HandshakeV10 packet — picking mocks[0]
	// blindly then crashed the second connection in a multi-connection
	// replay when the sort-order happened to put one of those in front.
	//
	// A handshake mock is one replay can serve: its FIRST response is the
	// greeting (the one sent to the client below) and it holds the
	// client's HandshakeResponse41 (first, or after an SSLRequest). A mock
	// holding a greeting further in, or a greeting and nothing more (a
	// connection that closed once greeted), was chosen before and then
	// never served, ahead of a mock that could have been.
	isHandshakeMock := func(m *models.Mock) bool {
		if m == nil || len(m.Spec.MySQLResponses) == 0 {
			return false
		}
		if _, ok := m.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet); !ok {
			return false
		}
		return hr41At(m, 0) || hr41At(m, 1)
	}
	// dst is the upstream the client dialled, as a miss's report names it: ""
	// when unknown or fabricated by the capture layer. The recorder stamps
	// destAddr with the address it was handed, fabricated or not, so the
	// selection below compares destAddr with dstCfg.Addr itself.
	dst := destinationOf(dstCfg)
	dialled := ""
	if dstCfg != nil {
		dialled = dstCfg.Addr
	}
	forDest := func(m *models.Mock) bool { return dialled != "" && m.Spec.Metadata["destAddr"] == dialled }
	// The greeting comes from a handshake recorded for the destination the
	// client dialled, else from any handshake: the earliest recorded first,
	// as the pool is sorted by ReqTimestampMock.
	var initialHandshakeMock *models.Mock
	for _, m := range mocks {
		if isHandshakeMock(m) && forDest(m) {
			initialHandshakeMock = m
			logger.Debug("Selected initial handshake mock by destAddr",
				zap.String("destAddr", dialled),
				zap.String("conn_id", m.Spec.Metadata["connID"]))
			break
		}
	}
	if initialHandshakeMock == nil {
		for _, m := range mocks {
			if isHandshakeMock(m) {
				initialHandshakeMock = m
				break
			}
		}
	}

	res := handshakeRes{}
	reqIdx, respIdx := 0, 0

	// The client waits to be greeted. With no recorded handshake to greet it
	// from, the connection cannot be answered: this returned nil, and the
	// command phase then waited, with the client never greeted, for as long
	// as the client did.
	if initialHandshakeMock == nil {
		utils.LogError(logger, nil, "no mysql session mock holds a recorded handshake")
		return res, newConnectionMiss(dst, stepGreeting,
			fmt.Sprintf("none of the %d MySQL handshake (config) mocks holds a recorded server greeting and HandshakeResponse41", len(mocks)),
			models.MatchPhaseNoMocks, 0)
	}

	for i, mock := range mocks {
		if mock == initialHandshakeMock {
			logger.Debug("Using initial handshake mock", zap.Int("index", i), zap.String("mock_name", mock.Name), zap.String("conn_id", mock.Spec.Metadata["connID"]))
			continue
		}
		logger.Debug("Config mocks available", zap.Int("index", i), zap.String("mock_name", mock.Name), zap.String("conn_id", mock.Spec.Metadata["connID"]))
	}

	// Read the intial request and response for the handshake from the mocks
	resp := initialHandshakeMock.Spec.MySQLResponses
	req := initialHandshakeMock.Spec.MySQLRequests

	handshake := resp[0].Message.(*mysql.HandshakeV10Packet) // isHandshakeMock: the greeting opens resp

	// Store the server greetings
	decodeCtx.ServerGreetings.Store(clientConn, handshake)

	// Set the intial auth plugin
	decodeCtx.PluginName = handshake.AuthPluginName
	decodeCtx.ServerCaps = handshake.CapabilityFlags
	var err error

	// encode the response
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[respIdx].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode handshake packet")
		return res, err
	}

	// Write the initial handshake to the client
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write server greetings to the client")

		return res, err
	}

	respIdx++

	// Read the client request, (handshake response or ssl request)
	handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read handshake response from client")
		return res, err
	}

	// Decode the handshakeResponse or sslRequest
	pkt, err := wire.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode handshake response from client")
		return res, err
	}

	// NEW: holders for SSL-first-packet and SSL-matched candidates
	var sslFirstPacket *mysql.PacketBundle
	var sslMatchedMocks []*models.Mock

	// handle the SSL request
	if decodeCtx.UseSSL {
		_, ok := pkt.Message.(*mysql.SSLRequestPacket)
		if !ok {
			// The decoder sets UseSSL only on an SSLRequest, so this is a
			// decoder fault, not a miss. It returned nil, and Replay then
			// closed the connection without a word.
			utils.LogError(logger, nil, "failed to assert SSL request packet")
			return res, fmt.Errorf("mysql decoder: UseSSL is set but the client's packet is a %s, not an SSLRequest", packetType(pkt))
		}

		// NEW: Strictly collect all mocks whose requests[0] match this SSLRequest.
		cp := *pkt
		sslFirstPacket = &cp

		for _, m := range mocks {
			mReq := m.Spec.MySQLRequests
			if len(mReq) == 0 {
				continue
			}
			// NEW: direct constant, not ...ToString
			if mReq[0].Header.Type != mysql.SSLRequest {
				continue
			}
			// Only a handshake replay can serve is a candidate: an
			// SSLRequest-only capture would shadow the full handshakes the
			// fallback below compares.
			if !isHandshakeMock(m) {
				continue
			}
			if err := matchSSLRequest(ctx, logger, mReq[0].PacketBundle, *sslFirstPacket); err == nil {
				sslMatchedMocks = append(sslMatchedMocks, m)
			}
		}

		// When no MySQL mock has a recorded SSLRequest entry — which
		// happens whenever the recording side captured the traffic
		// without seeing the pre-TLS SSLRequest packet (e.g. TLS-aware
		// consumers that attach mid-handshake, or recordings done
		// against a plaintext endpoint then replayed against a
		// TLS-requesting client) — fall back to matching against all
		// mocks. We still do the TLS MITM upgrade below, then match the
		// post-TLS HandshakeResponse41 against the full mock set. The
		// post-TLS wire format is identical, so a plaintext-recorded
		// MySQL flow can still serve a TLS-requesting replay client.
		if len(sslMatchedMocks) == 0 {
			logger.Debug("no MySQL mock has an SSLRequest entry; " +
				"falling back to plaintext mock candidates for post-TLS matching")
			sslMatchedMocks = mocks
		}

		reqIdx++ // matched (logically) with the mock so increment the index

		// Upgrade the client connection to TLS
		reader := bufio.NewReader(clientConn)
		initialData := make([]byte, 5)
		// reading the initial data from the client connection to determine if the connection is a TLS handshake
		testBuffer, err := reader.Peek(len(initialData))
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				logger.Debug("received EOF, closing conn", zap.Error(err))
				return res, nil
			}
			utils.LogError(logger, err, "failed to peek the mysql request message in proxy")
			return res, err
		}

		multiReader := io.MultiReader(reader, clientConn)
		clientConn = &pUtils.Conn{
			Conn:   clientConn,
			Reader: multiReader,
			Logger: logger,
		}

		// handle the TLS connection and get the upgraded client connection
		isTLS := pTls.IsTLSHandshake(testBuffer)
		if isTLS {
			clientConn, _, err = pTls.HandleTLSConnection(ctx, logger, clientConn, opts.Backdate)
			if err != nil {
				utils.LogError(logger, err, "failed to handle TLS conn")
				return res, err
			}
		}

		// Update this tls connection information in the handshake result
		res.tlsClientConn = clientConn

		// Store (Reset) the last operation for the upgraded client connection, because after ssl request the client will send the handshake response packet again.
		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

		// Store the server greeting packet for the upgraded client connection
		decodeCtx.ServerGreetings.Store(clientConn, handshake)

		// read the actual handshake response packet
		handshakeResponseBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read handshake response from client")
			return res, err
		}

		// Decode the handshakeResponse
		pkt, err = wire.DecodePayload(ctx, logger, handshakeResponseBuf, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode handshake response from client")
			return res, err
		}
	}

	// At this point, pkt MUST be HandshakeResponse41 (either SSL path post-decrypt or non-SSL path).
	hr41, ok := pkt.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert actual handshake response packet")
		handshakes := 0
		for _, m := range mocks {
			if isHandshakeMock(m) {
				handshakes++
			}
		}
		return res, newConnectionMiss(dst, stepHandshakeResponse,
			fmt.Sprintf("the client answered the greeting with a %s, not a HandshakeResponse41, so none of the %d recorded handshakes can be compared with it", packetType(pkt), handshakes),
			models.MatchPhaseExhausted, handshakes)
	}
	decodeCtx.ClientCaps = hr41.CapabilityFlags // live client caps

	// NEW: Build a single candidate list:
	// - If SSL was used: match HR41 within sslMatchedMocks only.
	// - Else: match HR41 across all mocks.
	candidates := mocks
	if sslFirstPacket != nil {
		candidates = sslMatchedMocks
	}

	// NEW: Strictly find the first candidate whose HR41 matches (auth_response ignored in matcher).
	// Try HR41 at index 0 and 1 to be tolerant to recording layout.

	logger.Debug("matching handshake response", zap.Any("actual request", pkt))

	selectedIdx := -1
	hrIdx := -1
	// compared counts the candidates whose recorded HandshakeResponse41 was
	// compared. closest is why the closest of them did not match, for the
	// miss's diff when none does: the first recorded for the destination the
	// client dialled, else the first compared.
	//
	// Of the candidates that match, the earliest recorded serves, as it
	// always has, except that a verdict replay serves on (an OK, or the
	// AuthMoreData that starts caching_sha2_password) outranks a refusal
	// recorded as an ERR, and an ERR outranks a verdict replay does not serve
	// (verdictRank): a login refused with an ERR and then retried with
	// success is answered with the success.
	compared := 0
	closest, closestForDest := "", ""
	selectedRank := 0
candidateLoop:
	for i, m := range candidates {
		// Only a mock that opens with the greeting can answer the client
		// from here on: what follows reads the server's next packet at
		// responses[1]. isHandshakeMock also guarantees an HR41 to compare.
		if !isHandshakeMock(m) {
			continue
		}
		compared++
		for idx := 0; idx < 2; idx++ {
			if !hr41At(m, idx) {
				continue
			}
			err := matchHanshakeResponse41(ctx, logger, m.Spec.MySQLRequests[idx].PacketBundle, *pkt)
			if err != nil {
				if closest == "" {
					closest = err.Error()
				}
				if closestForDest == "" && forDest(m) {
					closestForDest = err.Error()
				}
				continue
			}
			if r := verdictRank(recordedVerdict(m)); selectedIdx == -1 || r < selectedRank {
				selectedIdx, hrIdx, selectedRank = i, idx, r
				if r == 0 {
					break candidateLoop
				}
			}
			continue candidateLoop
		}
	}

	// NEW: If nothing matched HR41 strictly, error out.
	if selectedIdx == -1 {
		why := "no mysql mocks matched the HandshakeResponse41"
		if sslFirstPacket != nil {
			why += " within SSL-selected mocks"
		}
		utils.LogError(logger, nil, why)
		if closestForDest != "" {
			closest = closestForDest
		}
		if closest != "" {
			why += " (closest candidate: " + closest + ")"
		}
		// compared is at least 1: candidates is mocks, which holds the
		// greeting's handshake mock, or a non-empty sslMatchedMocks, which
		// holds only handshake mocks.
		return res, newConnectionMiss(dst, stepHandshakeResponse, why, models.MatchPhaseExhausted, compared)
	}

	// NEW: We have a strict HR41 match at candidates[selectedIdx], request index hrIdx.
	handshakeMatchedIdx := selectedIdx
	handshakeMock := candidates[handshakeMatchedIdx]

	// Update both responses and requests from the ultimately picked mock
	resp = handshakeMock.Spec.MySQLResponses
	req = handshakeMock.Spec.MySQLRequests

	// Once successful match of handshakeResponse41 is done the `reqIdx` can continue to increment just like how it is getting incremented.
	reqIdx = hrIdx + 1 // we have consumed HR41 at hrIdx
	respIdx = 1        // next server packet after HandshakeV10 is at responses[1]

	logger.Debug("picked mock for mysql handshake", zap.String("mock_name", handshakeMock.Name))

	// Get the handshake response from the mock (we have advanced past HR41, so look back one)
	if reqIdx-1 < 0 || reqIdx-1 >= len(req) {
		utils.LogError(logger, nil, "handshake response index out of range after selection")
		return res, matchedHandshakeMiss(dst, stepHandshakeResponse, "the matched handshake mock's HandshakeResponse41 cannot be read")
	}
	hrec, ok := req[reqIdx-1].Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		utils.LogError(logger, nil, "failed to assert mock handshake response packet")
		return res, matchedHandshakeMiss(dst, stepHandshakeResponse, "the matched handshake mock's HandshakeResponse41 cannot be read")
	}
	decodeCtx.RecordedClientCaps = hrec.CapabilityFlags

	// Get the next response in order to find the auth mechanism
	if len(resp) < respIdx+1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth mechanism")
		return res, matchedHandshakeMiss(dst, stepHandshakeResponse, "the matched handshake mock records no answer to it")
	}

	// Get the next packet to decide the auth mechanism or auth switching
	// For Native password: next packet is Ok/Err
	// For CachingSha2 password: next packet is AuthMoreData

	if resp[respIdx].Header == nil {
		utils.LogError(logger, nil, "the recorded answer to the handshake response has no header")
		return res, matchedHandshakeMiss(dst, stepHandshakeResponse, "the recorded answer to it cannot be read")
	}
	authDecider := resp[respIdx].Header.Type
	// answered is the client packet the verdict below answers: the
	// HandshakeResponse41, or the AuthSwitchResponse after a switch.
	answered := stepHandshakeResponse

	// Check if the next packet is AuthSwitchRequest
	// Server sends AuthSwitchRequest when it wants to switch the auth mechanism
	if authDecider == mysql.AuthStatusToString(mysql.AuthSwitchRequest) {
		logger.Debug("Auth switch request found, switching the auth mechanism")

		// Get the AuthSwitchRequest packet
		authSwithReqPkt, ok := resp[respIdx].Message.(*mysql.AuthSwitchRequestPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert auth switch request packet")
			return res, matchedHandshakeMiss(dst, stepHandshakeResponse, "the recorded AuthSwitchRequest cannot be read")
		}

		// Change the auth plugin name
		decodeCtx.PluginName = authSwithReqPkt.PluginName

		// Encode the AuthSwitchRequest packet
		buf, err = wire.EncodeToBinary(ctx, logger, &resp[respIdx].PacketBundle, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to encode auth switch request packet")
			return res, err
		}

		// Write the AuthSwitchRequest packet to the client
		_, err = clientConn.Write(buf)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write auth switch request to the client")
			return res, err
		}

		respIdx++

		// Read the auth switch response from the client
		authSwitchRespBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read auth switch response from the client")
			return res, err
		}

		// Get the packet from the buffer
		authSwitchRespPkt, err := mysqlUtils.BytesToMySQLPacket(authSwitchRespBuf)
		if err != nil {
			utils.LogError(logger, err, "failed to convert auth switch response to packet")
			return res, err
		}

		if len(req) < reqIdx+1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth switch response")
			return res, matchedHandshakeMiss(dst, stepAuthSwitchResponse, "no mysql mocks found for auth switch response")
		}

		// Get the auth switch response from the mock
		authSwitchRespMock := req[reqIdx].PacketBundle

		if authSwitchRespMock.Header.Type != mysql.AuthSwithResponse {
			utils.LogError(logger, nil, "expected auth switch response mock not found", zap.Any("found", authSwitchRespMock.Header.Type))
			return res, matchedHandshakeMiss(dst, stepAuthSwitchResponse, fmt.Sprintf("expected %s but found %s", mysql.AuthSwithResponse, authSwitchRespMock.Header.Type))
		}

		// Since auth switch response data can be different, we should just check the sequence number
		if authSwitchRespMock.Header != nil && authSwitchRespMock.Header.Header != nil &&
			authSwitchRespMock.Header.Header.SequenceID != authSwitchRespPkt.Header.SequenceID {
			utils.LogError(logger, nil, "sequence number mismatch for auth switch response", zap.Any("expected", authSwitchRespMock.Header.Header.SequenceID), zap.Any("actual", authSwitchRespPkt.Header.SequenceID))
			return res, matchedHandshakeMiss(dst, stepAuthSwitchResponse, "sequence number mismatch for auth switch response")
		}

		logger.Debug("auth mechanism switched successfully")

		reqIdx++

		// Get the next packet to decide the auth mechanism
		if len(resp) < respIdx+1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth mechanism after auth switch request")
			return res, matchedHandshakeMiss(dst, stepAuthSwitchResponse, "the matched handshake mock records no answer to it")
		}

		if resp[respIdx].Header == nil {
			utils.LogError(logger, nil, "the recorded answer to the auth switch response has no header")
			return res, matchedHandshakeMiss(dst, stepAuthSwitchResponse, "the recorded answer to it cannot be read")
		}
		authDecider = resp[respIdx].Header.Type
		answered = stepAuthSwitchResponse
	}

	// The verdicts this switch serves are the ones verdictRank ranks below 2.
	switch authDecider {
	case mysql.StatusToString(mysql.OK), mysql.StatusToString(mysql.ERR):
		var nativePassMocks reqResp
		nativePassMocks.resp = resp[respIdx:]

		// The recorded verdict, written as it was recorded: the server's OK,
		// or its refusal of the login (an ERR, such as access denied or an
		// unknown database). A refused login was answered nothing before:
		// no case ran, and the client waited for as long as it would.
		err := simulateNativePassword(ctx, logger, clientConn, nativePassMocks, handshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate native password")
			return res, err
		}
		// A refusal is the server's last word on the connection, which it
		// then closes. The replayer ends it too: a client is never left in a
		// command phase its server never opened.
		res.refused = authDecider == mysql.StatusToString(mysql.ERR)

	case mysql.AuthStatusToString(mysql.AuthMoreData):

		var cacheSha2PassMock reqResp
		cacheSha2PassMock.req = req[reqIdx:]
		cacheSha2PassMock.resp = resp[respIdx:]

		// It means we need to simulate the caching_sha2_password
		err := simulateCacheSha2Password(ctx, logger, clientConn, dst, cacheSha2PassMock, handshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate caching_sha2_password")
			return res, err
		}

	default:
		// An answer replay does not serve, such as a next authentication
		// factor. Before, no case ran and the handshake returned nil,
		// leaving the client to wait for an answer that never came.
		utils.LogError(logger, nil, "unsupported answer to the handshake response in the recording", zap.String("answer", authDecider))
		return res, matchedHandshakeMiss(dst, answered,
			fmt.Sprintf("the recording answers the %s with a %s, which replay does not serve", answered, authDecider))
	}

	return res, nil
}

func simulateNativePassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, nativePassMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	logger.Debug("final response for native password", zap.Any("response", nativePassMocks.resp[0].Header.Type))

	// Send the final response (OK/Err) to the client
	buf, err := wire.EncodeToBinary(ctx, logger, &nativePassMocks.resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for native password")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for native password to the client")
		return err
	}

	//update the config mock (since it can be reused in case of more connections compared to record mode)
	ok := updateMock(ctx, logger, initialHandshakeMock, mockDb, nil)
	if !ok {
		utils.LogError(logger, nil, "failed to update the mock unfiltered mock during native password")
	}

	logger.Debug("native password completed successfully")

	return nil
}

func simulateCacheSha2Password(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, cacheSha2PassMock reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	resp := cacheSha2PassMock.resp

	// Get the AuthMoreData
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data")
		return matchedHandshakeMiss(dst, stepCachingSha2, "no mysql mocks found for auth more data")
	}

	// Get the auth more data packet
	pkt, ok := resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet")
		return matchedHandshakeMiss(dst, stepCachingSha2, fmt.Sprintf("the recorded answer is a %T, not an AuthMoreData packet", resp[0].Message))
	}

	var mechanismString string
	CachingSha2PasswordMechanism := pkt.Data

	logger.Debug("[DEBUG] CachingSha2PasswordMechanism CachingSha2PasswordMechanism", zap.String("mechanism", CachingSha2PasswordMechanism), zap.Binary("mechanismBytes", []byte(CachingSha2PasswordMechanism)))

	if len(CachingSha2PasswordMechanism) == 1 {
		// CachingSha2PasswordMechanism single byte -> map to symbolic string
		b := CachingSha2PasswordMechanism[0]
		logger.Debug("[DEBUG] CachingSha2PasswordMechanism byte value", zap.Uint8("mechanismByte", b))
		mechanismString = mysql.CachingSha2PasswordToString(mysql.CachingSha2Password(b))
	} else if len(CachingSha2PasswordMechanism) > 1 {
		// already symbolic
		mechanismString = CachingSha2PasswordMechanism
	} else {
		mechanismString = "UNKNOWN"
	}
	logger.Debug("[DEBUG] CachingSha2PasswordMechanism normalized", zap.String("mechanismString", mechanismString))

	authBuf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode auth more data packet")
		return err
	}

	// Write the AuthMoreData packet to the client
	_, err = clientConn.Write(authBuf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write auth more data or auth switch request to the client")
		return err
	}

	if len(cacheSha2PassMock.resp) < 2 {
		utils.LogError(logger, nil, "response mock not found for caching_sha2_password after auth more data")
		return matchedHandshakeMiss(dst, stepCachingSha2, "response mock not found for caching_sha2_password after auth more data")
	}

	//update the cacheSha2PassMock resp
	cacheSha2PassMock.resp = cacheSha2PassMock.resp[1:]

	//simulate the caching_sha2_password auth mechanism
	switch mechanismString {
	case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
		err := simulateFullAuth(ctx, logger, clientConn, dst, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate full auth")
			return err
		}
	case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
		err := simulateFastAuthSuccess(ctx, logger, clientConn, dst, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate fast auth success")
			return err
		}
	default:
		// return an error
		utils.LogError(logger, nil, "unknown caching_sha2_password mechanism", zap.String("mechanism", mechanismString))
		return matchedHandshakeMiss(dst, stepCachingSha2, "the recording answers with an unknown caching_sha2_password mechanism: "+mechanismString)
	}
	return nil
}

func simulateFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, fastAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {
	resp := fastAuthMocks.resp

	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for fast auth success")
		return matchedHandshakeMiss(dst, stepCachingSha2, "final response mock not found for fast auth success")
	}

	logger.Debug("final response for fast auth success", zap.Any("response", resp[0].Header.Type))

	// Send the final response (OK/Err) to the client
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for fast auth success")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for fast auth success to the client")
		return err
	}

	//update the config mock (since it can be reused in case of more connections compared to record mode)
	//TODO: need to check when updateMock is unsuccessful
	ok := updateMock(ctx, logger, initialHandshakeMock, mockDb, nil)
	if !ok {
		utils.LogError(logger, nil, "failed to update the mock unfiltered mock during fast auth success")
	}

	logger.Debug("fast auth success completed successfully")

	return nil
}

func simulateFullAuth(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	resp := fullAuthMocks.resp
	req := fullAuthMocks.req

	if decodeCtx.UseSSL {
		logger.Debug("This is an ssl request, simulating plain password in caching_sha2_password full auth")
		err := simulatePlainPassword(ctx, logger, clientConn, dst, fullAuthMocks, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate plain password in caching_sha2_password full auth")
			return err
		}
		return nil
	}

	// read the public key request from the client
	publicKeyRequestBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read public key request from client")
		return err
	}

	// decode the public key request
	pkt, err := wire.DecodePayload(ctx, logger, publicKeyRequestBuf, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to decode public key request from client")
		return err
	}

	publicKey, ok := pkt.Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert public key request packet")
		return matchedHandshakeMiss(dst, stepPublicKey, fmt.Sprintf("the client sent a %s, not the recorded public key request", packetType(pkt)))
	}

	// Get the public key response from the mock
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for public key response")
		return matchedHandshakeMiss(dst, stepPublicKey, "no mysql mocks found for public key response")
	}

	publicKeyMock, ok := req[0].Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert public key response packet")
		return matchedHandshakeMiss(dst, stepPublicKey, "the handshake mock records no public key request")
	}

	// Match the header of the public key request
	ok = matchHeader(*req[0].Header.Header, *pkt.Header.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for public key request", zap.Any("expected", req[0].Header.Header), zap.Any("actual", pkt.Header.Header))
		return matchedHandshakeMiss(dst, stepPublicKey, "header mismatch for public key request")
	}

	// Match the public key response from the client with the mock
	if publicKey != publicKeyMock {
		utils.LogError(logger, nil, "public key mismatch", zap.Any("actual", publicKey), zap.Any("expected", publicKeyMock))
		return matchedHandshakeMiss(dst, stepPublicKey, "public key mismatch")
	}

	// Get the AuthMoreData for sending the public key
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data (public key)")
		return matchedHandshakeMiss(dst, stepPublicKey, "no mysql mocks found for auth more data (public key)")
	}

	// Get the AuthMoreData packet
	_, ok = resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet (public key)")
		return matchedHandshakeMiss(dst, stepPublicKey, "the handshake mock records no public key to answer with")
	}

	// encode the public key response
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode public key response packet")
		return err
	}

	// Write the public key response to the client
	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write public key response to the client")
		return err
	}

	// Read the encrypted password from the client

	encryptedPasswordBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read encrypted password from client")
		return err
	}

	// Get the packet from the buffer
	encryptedPassPkt, err := mysqlUtils.BytesToMySQLPacket(encryptedPasswordBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert encrypted password to packet")
		return err
	}

	if len(req) < 2 {
		utils.LogError(logger, nil, "no mysql mocks found for encrypted password during full auth")
		return matchedHandshakeMiss(dst, stepEncryptedPassword, "no mysql mocks found for encrypted password during full auth")
	}

	// Get the encrypted password from the mock
	encryptedPassMock := req[1].PacketBundle

	if encryptedPassMock.Header.Type != mysql.EncryptedPassword {
		utils.LogError(logger, nil, "expected encrypted password mock not found", zap.Any("found", encryptedPassMock.Header.Type))
		return matchedHandshakeMiss(dst, stepEncryptedPassword, fmt.Sprintf("expected %s but found %s", mysql.EncryptedPassword, encryptedPassMock.Header.Type))
	}

	// Since encrypted password can be different, we should just check the sequence number
	if encryptedPassMock.Header != nil && encryptedPassMock.Header.Header != nil &&
		encryptedPassMock.Header.Header.SequenceID != encryptedPassPkt.Header.SequenceID {
		utils.LogError(logger, nil, "sequence number mismatch for encrypted password", zap.Any("expected", encryptedPassMock.Header.Header.SequenceID), zap.Any("actual", encryptedPassPkt.Header.SequenceID))
		return matchedHandshakeMiss(dst, stepEncryptedPassword, "sequence number mismatch for encrypted password")
	}

	//Now send the final response (OK/Err) to the client
	if len(resp) < 2 {
		utils.LogError(logger, nil, "final response mock not found for full auth")
		return matchedHandshakeMiss(dst, stepEncryptedPassword, "final response mock not found for full auth")
	}

	logger.Debug("final response for full auth", zap.Any("response", resp[1].Header.Type))

	// Get the final response (OK/Err) from the mock
	// Send the final response (OK/Err) to the client
	buf, err = wire.EncodeToBinary(ctx, logger, &resp[1].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for full auth")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for full auth to the client")
		return err
	}

	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
	// Afterwards only fast auth success is expected. So, we can delete this.
	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
	// TODO: need to check what to do in this case
	if !ok {
		utils.LogError(logger, nil, "failed to delete unfiltered mock during full auth")
	}

	logger.Debug("full auth completed successfully")

	return nil
}

func simulatePlainPassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) error {

	req := fullAuthMocks.req
	resp := fullAuthMocks.resp

	// read the plain password from the client
	plainPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read plain password from client")
		return err
	}

	// Get the packet from the buffer
	plainPassPkt, err := mysqlUtils.BytesToMySQLPacket(plainPassBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert plain password to packet")
		return err
	}

	plainPass := string(intgUtils.EncodeBase64(plainPassPkt.Payload))

	// Get the plain password from the mock
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for plain password")
		return matchedHandshakeMiss(dst, stepPlainPassword, "no mysql mocks found for plain password")
	}

	plainPassMock, ok := req[0].Message.(string)
	if !ok {
		utils.LogError(logger, nil, "failed to assert plain password packet")
		return matchedHandshakeMiss(dst, stepPlainPassword, "the handshake mock records no plain password")
	}

	// Match the header of the plain password
	ok = matchHeader(*req[0].Header.Header, plainPassPkt.Header)
	if !ok {
		utils.LogError(logger, nil, "header mismatch for plain password", zap.Any("expected", req[0].Header.Header), zap.Any("actual", plainPassPkt.Header))
		return matchedHandshakeMiss(dst, stepPlainPassword, "header mismatch for plain password")
	}

	// Match the plain password from the client with the mock
	if plainPass != plainPassMock {
		// Neither password is logged: both are the plaintext credential
		// (base64 of the cleartext the client sent over TLS).
		utils.LogError(logger, nil, "plain password mismatch")
		return matchedHandshakeMiss(dst, stepPlainPassword, "plain password mismatch")
	}

	//Now send the final response (OK/Err) to the client
	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for full auth (plain password)")
		return matchedHandshakeMiss(dst, stepPlainPassword, "final response mock not found for full auth (plain password)")
	}

	logger.Debug("final response for full auth(plain password)", zap.Any("response", resp[0].Header.Type))

	// Get the final response (OK/Err) from the mock
	// Send the final response (OK/Err) to the client
	buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode final response packet for full auth (plain password)")
		return err
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to write final response for full auth (plain password) to the client")
		return err
	}

	// FullAuth mechanism only comes for the first time unless COM_CHANGE_USER is called (that is not supported for now).
	// Afterwards only fast auth success is expected. So, we can delete this.
	ok = mockDb.DeleteUnFilteredMock(*initialHandshakeMock)
	// TODO: need to check what to do in this case
	if !ok {
		utils.LogError(logger, nil, "failed to delete unfiltered mock during full auth (plain password) in ssl request")
	}

	logger.Debug("full auth (plain-password) in ssl request completed successfully")
	return nil
}

// packetType names a decoded client packet for a miss's diff: its recorded
// type, or its Go type when it has no header.
func packetType(pkt *mysql.PacketBundle) string {
	if pkt == nil {
		return "nothing"
	}
	if pkt.Header != nil && pkt.Header.Type != "" {
		return pkt.Header.Type
	}
	return fmt.Sprintf("%T", pkt.Message)
}

// hr41At reports whether m records the client's HandshakeResponse41 as its
// request at idx: 0, or 1 after an SSLRequest.
func hr41At(m *models.Mock, idx int) bool {
	reqs := m.Spec.MySQLRequests
	return idx < len(reqs) && reqs[idx].Header != nil && reqs[idx].Header.Type == mysql.HandshakeResponse41
}

// recordedVerdict is the server's recorded verdict on m's handshake: its
// answer to the HandshakeResponse41 (responses[1], after the greeting), or,
// after a recorded AuthSwitchRequest, its answer to the AuthSwitchResponse
// (responses[2]). "" when the recording holds none.
func recordedVerdict(m *models.Mock) string {
	resp := m.Spec.MySQLResponses
	at := 1
	if len(resp) > at && resp[at].Header != nil && resp[at].Header.Type == mysql.AuthStatusToString(mysql.AuthSwitchRequest) {
		at = 2
	}
	if len(resp) <= at || resp[at].Header == nil {
		return ""
	}
	return resp[at].Header.Type
}

// verdictRank orders the recorded verdicts of the handshakes whose
// HandshakeResponse41 matches the client's, lowest first: one replay serves on
// (an OK, or the AuthMoreData that starts caching_sha2_password, whose full
// authentication may itself still end in a refusal), then a refusal recorded
// as an ERR, then a verdict replay does not serve, or none. The verdict switch
// in simulateInitialHandshake serves the ones ranked below 2, and must be kept
// in step with this.
func verdictRank(verdict string) int {
	switch verdict {
	case mysql.StatusToString(mysql.OK), mysql.AuthStatusToString(mysql.AuthMoreData):
		return 0
	case mysql.StatusToString(mysql.ERR):
		return 1
	}
	return 2
}
