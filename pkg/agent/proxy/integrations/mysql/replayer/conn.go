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
		if m == nil || len(m.Spec.MySQLResponses) == 0 || m.Spec.MySQLResponses[0].Header == nil {
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
	// client dialled, else from any handshake: the first in pool order.
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
		return res, unencodableMiss(dst, stepGreeting, "server greeting", err)
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
			if mReq[0].Header == nil || mReq[0].Header.Type != mysql.SSLRequest {
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

	// Strictly match the client's HR41 (auth_response ignored in matcher),
	// at request index 0 and 1 to be tolerant to recording layout.

	// Not the packet: its auth response is the cleartext password under
	// mysql_clear_password (PAM, LDAP, IAM tokens).
	logger.Debug("matching handshake response", zap.String("user", hr41.Username), zap.String("database", printable(hr41.Database)), zap.String("auth_plugin", hr41.AuthPluginName))

	// compared counts the candidates whose recorded HandshakeResponse41 was
	// compared. closest is why the closest of them did not match, for the
	// miss's diff when none does: the first recorded for the destination the
	// client dialled, else the first compared.
	//
	// Of the candidates that match, the one served is, in this order of
	// preference: recorded for the server the client dialled; one whose
	// final verdict lets the login in (an OK, directly or at the end of its
	// caching_sha2_password exchange) ahead of a refusal recorded as an ERR;
	// first in pool order. A refusal is served only when nothing recorded for that
	// server lets the login in. The replayer cannot tell the password a
	// client sent (no step compares a credential), so it never refuses a
	// login the recording holds a success for: a login refused and then
	// retried with success, as the recording holds it, is let in. A candidate
	// whose verdict replay cannot serve (servableVerdict) is chosen only when
	// nothing else matches, so the miss names what it holds. Whether a login
	// is let in or refused depends on nothing a connection changes in the
	// pool: it is the same at boot, in every test window, and for
	// connections made at once.
	compared := 0
	closest, closestForDest := "", ""
	type pick struct{ idx, hrIdx int }
	// best[rank] is the first matching candidate of each rank, lowest best:
	// 0 lets in, for the dialled server; 1 refuses, for it; 2 lets in,
	// another server; 3 refuses, another server; 4 unservable.
	var best [5]*pick
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
			rank := 4
			if servable, refuses := servableVerdict(m, decodeCtx.UseSSL); servable {
				rank = 0
				if refuses {
					rank = 1
				}
				if !forDest(m) {
					rank += 2
				}
			}
			if best[rank] == nil {
				best[rank] = &pick{i, idx}
			}
			break
		}
		if best[0] != nil {
			break
		}
	}
	var selected *pick
	for _, p := range best {
		if p != nil {
			selected = p
			break
		}
	}
	selectedIdx, hrIdx := -1, -1
	if selected != nil {
		selectedIdx, hrIdx = selected.idx, selected.hrIdx
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
			return res, unencodableMiss(dst, stepHandshakeResponse, "AuthSwitchRequest", err)
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

		if authSwitchRespMock.Header == nil || authSwitchRespMock.Header.Type != mysql.AuthSwithResponse {
			found := packetType(&authSwitchRespMock)
			utils.LogError(logger, nil, "expected auth switch response mock not found", zap.String("found", found))
			return res, matchedHandshakeMiss(dst, stepAuthSwitchResponse, fmt.Sprintf("expected %s but found %s", mysql.AuthSwithResponse, found))
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

	// The verdicts this switch serves are the ones servableVerdict accepts.
	switch authDecider {
	case mysql.StatusToString(mysql.OK), mysql.StatusToString(mysql.ERR):
		// The recorded verdict, written as it was recorded: the server's OK,
		// or its refusal of the login (an ERR, such as access denied or an
		// unknown database). A refused login was answered nothing before:
		// no case ran, and the client waited for as long as it would.
		refused, err := serveVerdict(ctx, logger, clientConn, dst, answered, &resp[respIdx].PacketBundle, handshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate native password")
			return res, err
		}
		res.refused = refused

	case mysql.AuthStatusToString(mysql.AuthMoreData):

		var cacheSha2PassMock reqResp
		cacheSha2PassMock.req = req[reqIdx:]
		cacheSha2PassMock.resp = resp[respIdx:]

		// It means we need to simulate the caching_sha2_password
		refused, err := simulateCacheSha2Password(ctx, logger, clientConn, dst, cacheSha2PassMock, handshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate caching_sha2_password")
			return res, err
		}
		res.refused = refused

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

// serveVerdict writes the server's recorded verdict on a login, verdict (its
// OK, or its refusal as an ERR), to the client, and reports whether it was a
// refusal. A verdict that cannot be read, or one that is neither, is a miss of
// the client's connection to dst at step, the step the verdict answers, and
// nothing is written for it. The handshake mock it comes from stays in the pool for the next
// connection (updateMock), as more connections may open than were recorded.
func serveVerdict(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst, step string, verdict *mysql.PacketBundle, handshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (bool, error) {
	if verdict.Header == nil {
		utils.LogError(logger, nil, "the recorded verdict on the login has no header")
		return false, matchedHandshakeMiss(dst, step, "the recorded verdict on the login cannot be read")
	}
	// Only an OK or an ERR ends a login. Anything else (a next
	// authentication factor, after a caching_sha2_password first factor) is
	// an answer replay does not serve, as on the native path.
	if t := verdict.Header.Type; t != mysql.StatusToString(mysql.OK) && t != mysql.StatusToString(mysql.ERR) {
		utils.LogError(logger, nil, "unsupported final answer on the login in the recording", zap.String("answer", t))
		return false, matchedHandshakeMiss(dst, step, fmt.Sprintf("the recording answers the %s with a %s, which replay does not serve", step, t))
	}
	refused := verdict.Header.Type == mysql.StatusToString(mysql.ERR)
	logger.Debug("the recorded verdict on the login", zap.Bool("refused", refused))

	buf, err := wire.EncodeToBinary(ctx, logger, verdict, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the recorded verdict on the login")
		return false, unencodableMiss(dst, step, "verdict on the login", err)
	}

	_, err = clientConn.Write(buf)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write the recorded verdict on the login to the client")
		return false, err
	}

	if !updateMock(ctx, logger, handshakeMock, mockDb, nil) {
		utils.LogError(logger, nil, "failed to update the handshake mock after serving its verdict")
	}
	return refused, nil
}

func simulateCacheSha2Password(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, cacheSha2PassMock reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (bool, error) {
	resp := cacheSha2PassMock.resp

	// Get the AuthMoreData
	if len(resp) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for auth more data")
		return false, matchedHandshakeMiss(dst, stepCachingSha2, "no mysql mocks found for auth more data")
	}

	// Get the auth more data packet
	pkt, ok := resp[0].Message.(*mysql.AuthMoreDataPacket)
	if !ok {
		utils.LogError(logger, nil, "failed to assert auth more data packet")
		return false, matchedHandshakeMiss(dst, stepCachingSha2, fmt.Sprintf("the recorded answer is a %T, not an AuthMoreData packet", resp[0].Message))
	}

	mechanismString := cachingSha2Mechanism(pkt)
	logger.Debug("caching_sha2_password mechanism", zap.String("mechanism", mechanismString))

	authBuf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
	if err != nil {
		utils.LogError(logger, err, "failed to encode auth more data packet")
		return false, unencodableMiss(dst, stepCachingSha2, "AuthMoreData", err)
	}

	// Write the AuthMoreData packet to the client
	_, err = clientConn.Write(authBuf)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		utils.LogError(logger, err, "failed to write auth more data or auth switch request to the client")
		return false, err
	}

	if len(cacheSha2PassMock.resp) < 2 {
		utils.LogError(logger, nil, "response mock not found for caching_sha2_password after auth more data")
		return false, matchedHandshakeMiss(dst, stepCachingSha2, "response mock not found for caching_sha2_password after auth more data")
	}

	//update the cacheSha2PassMock resp
	cacheSha2PassMock.resp = cacheSha2PassMock.resp[1:]

	//simulate the caching_sha2_password auth mechanism
	switch mechanismString {
	case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
		refused, err := simulateFullAuth(ctx, logger, clientConn, dst, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate full auth")
			return false, err
		}
		return refused, nil
	case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
		refused, err := simulateFastAuthSuccess(ctx, logger, clientConn, dst, cacheSha2PassMock, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate fast auth success")
			return false, err
		}
		return refused, nil
	default:
		// return an error
		utils.LogError(logger, nil, "unknown caching_sha2_password mechanism", zap.String("mechanism", mechanismString))
		return false, matchedHandshakeMiss(dst, stepCachingSha2, "the recording answers with an unknown caching_sha2_password mechanism: "+mechanismString)
	}
}

func simulateFastAuthSuccess(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, fastAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (bool, error) {
	resp := fastAuthMocks.resp

	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for fast auth success")
		return false, matchedHandshakeMiss(dst, stepCachingSha2, "final response mock not found for fast auth success")
	}

	logger.Debug("final response for fast auth success", zap.String("response", packetType(&resp[0].PacketBundle)))

	return serveVerdict(ctx, logger, clientConn, dst, stepCachingSha2, &resp[0].PacketBundle, initialHandshakeMock, mockDb, decodeCtx)
}

func simulateFullAuth(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (bool, error) {

	resp := fullAuthMocks.resp
	req := fullAuthMocks.req

	if decodeCtx.UseSSL {
		logger.Debug("This is an ssl request, simulating plain password in caching_sha2_password full auth")
		refused, err := simulatePlainPassword(ctx, logger, clientConn, dst, fullAuthMocks, initialHandshakeMock, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate plain password in caching_sha2_password full auth")
			return false, err
		}
		return refused, nil
	}

	// The client asks for the server's public key, or, holding the key
	// already, sends its password encrypted with it at once
	// (mysqlUtils.IsPublicKeyRequest). The recording holds one of the two
	// exchanges: its first request is the public key request or the
	// encrypted password. Nothing the client sends here is logged: the
	// encrypted password is the server's private key away from the password.
	clientAuthBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read the public key request or the encrypted password from client")
		return false, err
	}
	recordedAtOnce := len(req) > 0 && req[0].Header != nil && req[0].Header.Type == mysql.EncryptedPassword

	encryptedPasswordBuf := clientAuthBuf
	if mysqlUtils.IsPublicKeyRequest(clientAuthBuf) {
		if recordedAtOnce {
			utils.LogError(logger, nil, "the client asked for the server's public key, which the recording does not hold")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "the client asked for the server's public key; the recording holds none, as the client it was recorded with had the key already and sent its password encrypted at once")
		}
		pkt, err := wire.DecodePayload(ctx, logger, clientAuthBuf, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to decode public key request from client")
			return false, err
		}

		if len(req) < 1 {
			utils.LogError(logger, nil, "no mysql mocks found for public key response")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "no mysql mocks found for public key response")
		}
		if req[0].Header != nil && req[0].Header.Type != mysql.CachingSha2PasswordToString(mysql.RequestPublicKey) {
			found := packetType(&req[0].PacketBundle)
			utils.LogError(logger, nil, "the recording holds no public key request where the client sent one", zap.String("found", found))
			return false, matchedHandshakeMiss(dst, stepPublicKey, fmt.Sprintf("the client asked for the server's public key; the recording holds a %s there", found))
		}
		if _, ok := req[0].Message.(string); !ok {
			utils.LogError(logger, nil, "failed to assert public key response packet")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "the handshake mock records no public key request")
		}

		// Match the header of the public key request
		if req[0].Header == nil || req[0].Header.Header == nil || pkt.Header == nil || pkt.Header.Header == nil ||
			!matchHeader(*req[0].Header.Header, *pkt.Header.Header) {
			utils.LogError(logger, nil, "header mismatch for public key request")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "header mismatch for public key request")
		}

		// Get the AuthMoreData for sending the public key
		if len(resp) < 1 {
			utils.LogError(logger, nil, "no mysql mocks found for auth more data (public key)")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "no mysql mocks found for auth more data (public key)")
		}

		// Get the AuthMoreData packet
		_, ok := resp[0].Message.(*mysql.AuthMoreDataPacket)
		if !ok {
			utils.LogError(logger, nil, "failed to assert auth more data packet (public key)")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "the handshake mock records no public key to answer with")
		}
		if resp[0].Header == nil {
			utils.LogError(logger, nil, "the recorded public key has no header")
			return false, matchedHandshakeMiss(dst, stepPublicKey, "the recorded public key cannot be read")
		}

		// encode the public key response
		buf, err := wire.EncodeToBinary(ctx, logger, &resp[0].PacketBundle, clientConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to encode public key response packet")
			return false, unencodableMiss(dst, stepPublicKey, "public key", err)
		}

		// Write the public key response to the client
		_, err = clientConn.Write(buf)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write public key response to the client")
			return false, err
		}
		req, resp = req[1:], resp[1:]

		// Read the encrypted password from the client
		encryptedPasswordBuf, err = mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
		if err != nil {
			utils.LogError(logger, err, "failed to read encrypted password from client")
			return false, err
		}
	} else if !recordedAtOnce {
		why := "the client sent its password encrypted with a public key it holds; the recording holds nothing for it"
		if len(req) > 0 {
			switch found := packetType(&req[0].PacketBundle); found {
			case mysql.CachingSha2PasswordToString(mysql.RequestPublicKey):
				why = "the client sent its password encrypted with a public key it holds; the recording asks the server for its key first"
			case mysql.PlainPassword:
				why = "the client sent its password encrypted, without TLS; the recording was made over TLS, where the client sends it in clear"
			default:
				why = fmt.Sprintf("the client sent its password encrypted with a public key it holds; the recording holds a %s there", found)
			}
		}
		utils.LogError(logger, nil, "the client's full authentication is not the recorded one", zap.String("why", why))
		return false, matchedHandshakeMiss(dst, stepPublicKey, why)
	}

	// Get the packet from the buffer
	encryptedPassPkt, err := mysqlUtils.BytesToMySQLPacket(encryptedPasswordBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert encrypted password to packet")
		return false, err
	}

	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for encrypted password during full auth")
		return false, matchedHandshakeMiss(dst, stepEncryptedPassword, "no mysql mocks found for encrypted password during full auth")
	}

	// Get the encrypted password from the mock
	encryptedPassMock := req[0].PacketBundle

	if encryptedPassMock.Header == nil || encryptedPassMock.Header.Type != mysql.EncryptedPassword {
		found := packetType(&encryptedPassMock)
		utils.LogError(logger, nil, "expected encrypted password mock not found", zap.String("found", found))
		return false, matchedHandshakeMiss(dst, stepEncryptedPassword, fmt.Sprintf("expected %s but found %s", mysql.EncryptedPassword, found))
	}

	// Since encrypted password can be different, we should just check the sequence number
	if encryptedPassMock.Header.Header != nil &&
		encryptedPassMock.Header.Header.SequenceID != encryptedPassPkt.Header.SequenceID {
		utils.LogError(logger, nil, "sequence number mismatch for encrypted password", zap.Any("expected", encryptedPassMock.Header.Header.SequenceID), zap.Any("actual", encryptedPassPkt.Header.SequenceID))
		return false, matchedHandshakeMiss(dst, stepEncryptedPassword, "sequence number mismatch for encrypted password")
	}

	//Now send the final response (OK/Err) to the client
	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for full auth")
		return false, matchedHandshakeMiss(dst, stepEncryptedPassword, "final response mock not found for full auth")
	}

	logger.Debug("final response for full auth", zap.String("response", packetType(&resp[0].PacketBundle)))

	// The handshake stays in the pool for a connection the recording holds no
	// other handshake for: the client authenticates in full again, as the
	// server asks. It was deleted, and the next connection of a recording that
	// held one handshake got none.
	return serveVerdict(ctx, logger, clientConn, dst, stepEncryptedPassword, &resp[0].PacketBundle, initialHandshakeMock, mockDb, decodeCtx)
}

func simulatePlainPassword(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dst string, fullAuthMocks reqResp, initialHandshakeMock *models.Mock, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (bool, error) {

	req := fullAuthMocks.req
	resp := fullAuthMocks.resp

	// read the plain password from the client
	plainPassBuf, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read plain password from client")
		return false, err
	}

	// Get the packet from the buffer
	plainPassPkt, err := mysqlUtils.BytesToMySQLPacket(plainPassBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to convert plain password to packet")
		return false, err
	}

	// The password itself is not compared, as no other authentication step
	// compares a credential (the HandshakeResponse41's auth response and the
	// encrypted password are not): an application replayed with another
	// password than it was recorded with is served the recorded verdict.

	// Get the plain password from the mock
	if len(req) < 1 {
		utils.LogError(logger, nil, "no mysql mocks found for plain password")
		return false, matchedHandshakeMiss(dst, stepPlainPassword, "no mysql mocks found for plain password")
	}

	if _, ok := req[0].Message.(string); !ok {
		utils.LogError(logger, nil, "failed to assert plain password packet")
		return false, matchedHandshakeMiss(dst, stepPlainPassword, "the handshake mock records no plain password")
	}

	// Only its sequence is checked, as the encrypted password's is: its
	// length is the password's.
	if req[0].Header == nil || req[0].Header.Header == nil || req[0].Header.Header.SequenceID != plainPassPkt.Header.SequenceID {
		utils.LogError(logger, nil, "sequence number mismatch for plain password")
		return false, matchedHandshakeMiss(dst, stepPlainPassword, "sequence number mismatch for plain password")
	}

	//Now send the final response (OK/Err) to the client
	if len(resp) < 1 {
		utils.LogError(logger, nil, "final response mock not found for full auth (plain password)")
		return false, matchedHandshakeMiss(dst, stepPlainPassword, "final response mock not found for full auth (plain password)")
	}

	logger.Debug("final response for full auth(plain password)", zap.String("response", packetType(&resp[0].PacketBundle)))

	return serveVerdict(ctx, logger, clientConn, dst, stepPlainPassword, &resp[0].PacketBundle, initialHandshakeMock, mockDb, decodeCtx)
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

// servableVerdict reports whether replay can serve m's recorded verdict on a
// login whose HandshakeResponse41 matches it, and whether that verdict is a
// refusal. The verdict is the server's final answer on the login: an OK, or a
// refusal recorded as an ERR. It answers the HandshakeResponse41 (responses[1],
// after the greeting), or, after a recorded AuthSwitchRequest, the
// AuthSwitchResponse (responses[2]); or it ends the caching_sha2_password
// exchange an AuthMoreData there starts: right after it for a fast
// authentication, or a full one over TLS; for a full one without TLS (as
// useSSL says), after the public key when the recorded client asked for it,
// and right after it when the client held the key and sent its password
// encrypted at once (fullAuthStart). A recording of the password in clear,
// made over TLS, serves no client without TLS. A full authentication that
// ends in an ERR is a refusal (a wrong password, on MySQL 8's default login
// method). The verdict switch in simulateInitialHandshake serves these, and
// must be kept in step with this. A mock that holds no verdict, one replay
// does not serve (such as a next authentication factor, or an AuthMoreData of
// another mechanism), or an exchange cut short does not shadow one that can
// be served.
func servableVerdict(m *models.Mock, useSSL bool) (servable, refuses bool) {
	resp := m.Spec.MySQLResponses
	at := 1
	if len(resp) > at && resp[at].Header != nil && resp[at].Header.Type == mysql.AuthStatusToString(mysql.AuthSwitchRequest) {
		at = 2
	}
	if len(resp) <= at || resp[at].Header == nil {
		return false, false
	}
	if resp[at].Header.Type == mysql.AuthStatusToString(mysql.AuthMoreData) {
		more, ok := resp[at].Message.(*mysql.AuthMoreDataPacket)
		if !ok {
			return false, false
		}
		switch cachingSha2Mechanism(more) {
		case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
			at++
		case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
			at++
			if !useSSL {
				// Without TLS the client sends its password encrypted:
				// after the public key it asked for, or at once with a
				// key it held. A recording of the password in clear was
				// made over TLS, and a client without TLS cannot be
				// served from it.
				switch fullAuthStart(m) {
				case mysql.CachingSha2PasswordToString(mysql.RequestPublicKey):
					at++ // the public key comes first
				case mysql.EncryptedPassword:
				default:
					return false, false
				}
			}
		default:
			return false, false
		}
		if len(resp) <= at || resp[at].Header == nil {
			return false, false
		}
	}
	// The verdict's message must be the packet its header names: the encoder
	// chooses by message type, so another one would be written as some other
	// packet, or fail to encode.
	switch resp[at].Header.Type {
	case mysql.StatusToString(mysql.OK):
		_, ok := resp[at].Message.(*mysql.OKPacket)
		return ok, false
	case mysql.StatusToString(mysql.ERR):
		_, ok := resp[at].Message.(*mysql.ERRPacket)
		return ok, ok
	}
	return false, false
}

// fullAuthStart is how the client of handshake mock m began a
// caching_sha2_password full authentication: the type of its first request
// that is a public key request, an encrypted password or a password in
// clear (recorded over TLS), or "" when it holds none.
func fullAuthStart(m *models.Mock) string {
	for _, r := range m.Spec.MySQLRequests {
		if r.Header == nil {
			continue
		}
		switch r.Header.Type {
		case mysql.CachingSha2PasswordToString(mysql.RequestPublicKey), mysql.EncryptedPassword, mysql.PlainPassword:
			return r.Header.Type
		}
	}
	return ""
}

// cachingSha2Mechanism is the caching_sha2_password mechanism an AuthMoreData
// packet names: its single status byte as its symbolic name, a name already
// symbolic, or "UNKNOWN".
func cachingSha2Mechanism(pkt *mysql.AuthMoreDataPacket) string {
	switch {
	case len(pkt.Data) == 1:
		return mysql.CachingSha2PasswordToString(mysql.CachingSha2Password(pkt.Data[0]))
	case len(pkt.Data) > 1:
		return pkt.Data
	}
	return "UNKNOWN"
}
