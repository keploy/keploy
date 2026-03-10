// Package recorder is used to record the MySQL traffic between the client and the server.
package recorder

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// Record records the MySQL traffic between the client and the server.
//
// Architecture: "TeeForward & Defer"
//
//  1. Handshake runs synchronously (once per connection, amortised by pools).
//  2. Two TeeForwardConns forward traffic at wire speed while buffering data
//     in pre-allocated ring buffers (zero heap allocs in forwarding path).
//  3. A reassembler goroutine reads from the ring buffers, frames MySQL
//     packets into request-response pairs (byte-level, no struct decode).
//  4. A decoder goroutine fully decodes the raw pairs into models.Mock.
//
// The forwarding path does zero heap allocations → identical latency to
// bare io.Copy (~12-13ms P50).
//
// Post-TLS mode: When the context contains PostTLSModeKey (set by JSSE/SSL
// uprobe capture), the data is decrypted plaintext starting AFTER the TLS
// handshake. The initial MySQL handshake (greeting, SSL request) was sent in
// plaintext before TLS and is captured separately by the relay path. In this
// mode we skip the handshake phase and go directly to the command-phase
// pipeline with reasonable default capabilities for MySQL 8.0.
func Record(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)

	// Get the error group from the context.
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	postTLSMode := ctx.Value(models.PostTLSModeKey) != nil

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		var hsResult *handshakeResult

		if postTLSMode {
			// Post-TLS (JSSE/SSL uprobe): data starts after the TLS handshake.
			// The initial MySQL handshake (greeting + SSL request) was captured
			// by the relay path. Here we only have the command phase plaintext.
			// Use default MySQL 8.0 capabilities.
			logger.Info("MySQL post-TLS mode: skipping handshake, using default capabilities")

			// Synthetic ServerGreeting so the decode pipeline can access
			// CapabilityFlags without nil-pointer panics.  The real greeting
			// was exchanged in plaintext before TLS; we just need a
			// capabilities value that matches what MySQL 8.0 typically uses.
			defaultCaps := mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_DEPRECATE_EOF | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL
			syntheticGreeting := &mysql.HandshakeV10Packet{
				ProtocolVersion: 10,
				ServerVersion:   "8.0.0-keploy-synthetic",
				CapabilityFlags: defaultCaps,
				CharacterSet:    0xFF, // utf8mb4
				StatusFlags:     0x0002,
				AuthPluginName:  "caching_sha2_password",
			}

			hsResult = &handshakeResult{
				ClientConn: clientConn,
				DestConn:   destConn,
				State: handshakeState{
					// MySQL 8.0 defaults: deprecate EOF is standard.
					ServerCaps:     defaultCaps,
					ClientCaps:     defaultCaps,
					DeprecateEOF:   true,
					UseSSL:         true,
					ServerGreeting: syntheticGreeting,
				},
			}

			// In post-TLS mode, the JSSE data starts with HandshakeResponse41
			// (client→server) and auth exchange (server→client). We need to
			// consume and record these as a config mock, then proceed to
			// command phase.
			logger.Info("MySQL post-TLS: consuming auth exchange from JSSE data")
			authMocks, err := consumePostTLSAuth(ctx, logger, clientConn, destConn)
			if err != nil {
				if err != io.EOF {
					logger.Warn("post-TLS auth consumption failed", zap.Error(err))
				} else {
					logger.Info("post-TLS auth consumption got EOF")
					errCh <- err
					return nil
				}
			}
			logger.Info("MySQL post-TLS auth consumed successfully",
				zap.Int("authMocks", len(authMocks)))
			if len(authMocks) > 0 {
				hsResult.Mocks = authMocks
			}
		} else {
			// Standard path: synchronous handshake.
			var err error
			hsResult, err = handleHandshake(ctx, logger, clientConn, destConn, opts)
			if err != nil {
				if err != io.EOF {
					logger.Error("handshake failed. Check MySQL server credentials and ensure the server is accepting connections", zap.Error(err))
				}
				errCh <- err
				return nil
			}
		}

		// If TLSOnly, only the handshake was captured (relay path for TLS MySQL).
		// Record handshake mocks and return — command phase is encrypted and will
		// be captured by JSSE/SSL uprobes separately.
		if hsResult.TLSOnly {
			logger.Debug("MySQL TLS-only mode: recording handshake config mock, command phase handled by JSSE/SSL uprobes")
			connID := ""
			if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
				connID = v.(string)
			}
			for _, entry := range hsResult.Mocks {
				mock, err := decodeRawMockEntry(ctx, logger, entry, nil, nil)
				if err != nil {
					logger.Debug("failed to decode handshake mock", zap.Error(err))
					continue
				}
				setConnID(mock, connID)
				mocks <- mock
			}
			errCh <- nil
			return nil
		}

		cmdClientConn := hsResult.ClientConn
		cmdDestConn := hsResult.DestConn

		// ── Phase 2: TeeForwardConn-based forwarding ──
		// ── Phase 2: Set up data sources for the pipeline ──
		//
		// Standard path: TeeForwardConns read from real TCP sockets, forward
		// traffic at wire speed, and buffer data in ring buffers for the parser.
		//
		// Post-TLS (JSSE) path: Data is already pushed into SimulatedConns by
		// the JSSE agent — there's no real TCP socket and nothing to "forward."
		// Using TeeForwardConn would deadlock because SimulatedConn.Read()
		// blocks on a channel and TeeForwardConn's ctx check only runs every
		// 64 Read iterations. Instead, wrap SimulatedConns in bufio.Reader
		// (which satisfies peekReader) and read directly.
		var clientSrc, serverSrc peekReader
		if postTLSMode {
			// Direct readers — no forwarding goroutines, no ring buffers.
			// SimulatedConn.Read() blocks until Push() feeds data or Close()
			// signals EOF, which is exactly right.
			clientSrc = bufio.NewReaderSize(cmdClientConn, 64*1024)
			serverSrc = bufio.NewReaderSize(cmdDestConn, 64*1024)
		} else {
			// Two TeeForwardConns: one per direction.
			// Each reads from src, forwards to dest at wire speed, and
			// buffers data in a 2 MB pre-allocated ring buffer (ZERO heap
			// allocations in the forwarding goroutine).
			//
			// clientTee: client→server (captures requests)
			//   → forwards queries to MySQL at wire speed BEFORE the pipeline
			//     wakes up, which is critical for P50 latency.
			// serverTee: server→client (captures responses)
			clientSrc = orchestrator.NewTeeForwardConn(ctx, logger, cmdClientConn, cmdDestConn)
			serverSrc = orchestrator.NewTeeForwardConn(ctx, logger, cmdDestConn, cmdClientConn)
		}

		// ── Phase 3: Merged reassembler+decoder (single goroutine) ──
		// Send handshake mocks. These use raw packet representation for config
		// type, so decode is very fast (just wrapping bytes).
		if len(hsResult.Mocks) > 0 {
			connID := ""
			if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
				connID = v.(string)
			}
			for _, entry := range hsResult.Mocks {
				mock, err := decodeRawMockEntry(ctx, logger, entry, nil, nil)
				if err != nil {
					logger.Debug("failed to decode handshake mock", zap.Error(err))
					continue
				}
				setConnID(mock, connID)
				mocks <- mock
			}
		}

		// The command-phase is handled by a SINGLE merged goroutine that
		// reads from both ring buffers, frames packets using slab allocation,
		// decodes inline, and sends mocks.
		pipelineDone := make(chan struct{})
		go func() {
			defer close(pipelineDone)
			runRecordPipeline(ctx, logger, clientSrc, serverSrc, mocks, opts, hsResult.State)
		}()

		// ── Phase 4: Wait for completion ─────────────────────────
		<-pipelineDone

		errCh <- nil
		return nil
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

// recordMockDirect creates a models.Mock from a RawMockEntry's decoded data
// and sends it to the output channel. Used by ProcessRawMocksV2.
func recordMockDirect(ctx context.Context, mock *models.Mock, mocks chan<- *models.Mock, opts models.OutgoingOptions) {
	if opts.Synchronous {
		mgr := syncMock.Get()
		mgr.AddMock(mock)
		return
	}

	// Non-blocking send: if the channel buffer is full, fall back to a
	// goroutine so the decoder loop is never stalled.
	select {
	case mocks <- mock:
	default:
		go func() {
			select {
			case mocks <- mock:
			case <-ctx.Done():
			}
		}()
	}
}

// Ensure time is used (for mock timestamps).
var _ = time.Now

// consumePostTLSAuth reads the post-TLS authentication exchange from the
// JSSE-captured data and returns it as a config mock entry. In the MySQL
// STARTTLS flow, after TLS is established the client sends:
//   - HandshakeResponse41 (client→server via ClientConn)
//   - Server replies with OK/AuthSwitch/AuthMore (server→client via ServerConn)
//   - Possibly more auth packets
//
// We consume these auth packets as "config" mock entries. The actual command
// phase starts after auth completes (server sends OK with 0x00 marker).
func consumePostTLSAuth(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn) ([]RawMockEntry, error) {
	var reqPackets [][]byte
	var respPackets [][]byte
	reqTimestamp := time.Now()

	// Read HandshakeResponse41 from client.
	hsResp, err := readPacketSync(clientConn)
	if err != nil {
		return nil, err
	}
	reqPackets = append(reqPackets, hsResp)

	// Read auth response from server in a loop until we get an OK or error.
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		authResp, err := readPacketSync(destConn)
		if err != nil {
			return nil, err
		}
		respPackets = append(respPackets, authResp)

		if len(authResp) < 5 {
			break
		}

		marker := authResp[4]
		switch marker {
		case mysql.OK: // 0x00 — auth complete
			resTimestamp := time.Now()
			return []RawMockEntry{{
				ReqPackets:   reqPackets,
				RespPackets:  respPackets,
				CmdType:      mysql.HandshakeV10,
				MockType:     "config",
				ReqTimestamp: reqTimestamp,
				ResTimestamp: resTimestamp,
			}}, nil
		case mysql.ERR: // 0xFF — auth failed
			resTimestamp := time.Now()
			return []RawMockEntry{{
				ReqPackets:   reqPackets,
				RespPackets:  respPackets,
				CmdType:      mysql.HandshakeV10,
				MockType:     "config",
				ReqTimestamp: reqTimestamp,
				ResTimestamp: resTimestamp,
			}}, nil
		case 0xFE: // AuthSwitchRequest — need to respond
			switchResp, err := readPacketSync(clientConn)
			if err != nil {
				logger.Debug("post-TLS auth: failed to read switch response", zap.Error(err))
				break
			}
			reqPackets = append(reqPackets, switchResp)
			continue
		case 0x01: // AuthMoreData (caching_sha2_password)
			// Check the mechanism byte to determine whether the client responds.
			// payload[0] = 0x01 (AuthMoreData marker), payload[1] = mechanism byte
			if len(authResp) > 5 {
				mechByte := authResp[5]
				switch mechByte {
				case 0x03: // FastAuthSuccess — server sends OK next, no client response.
					logger.Debug("post-TLS auth: fast auth success, waiting for OK from server")
					continue
				case 0x04: // PerformFullAuthentication — client sends password over TLS.
					clientData, err := readPacketSync(clientConn)
					if err != nil {
						logger.Debug("post-TLS auth: failed to read full auth client data", zap.Error(err))
						break
					}
					reqPackets = append(reqPackets, clientData)
					continue
				}
			}
			// Unknown AuthMoreData mechanism — try reading client response as fallback.
			moreData, err := readPacketSync(clientConn)
			if err != nil {
				logger.Debug("post-TLS auth: failed to read more auth data", zap.Error(err))
				break
			}
			reqPackets = append(reqPackets, moreData)
			continue
		default:
			// Unknown auth packet, stop consuming.
			logger.Debug("post-TLS auth: unexpected marker, ending auth capture",
				zap.Uint8("marker", marker))
			resTimestamp := time.Now()
			return []RawMockEntry{{
				ReqPackets:   reqPackets,
				RespPackets:  respPackets,
				CmdType:      mysql.HandshakeV10,
				MockType:     "config",
				ReqTimestamp: reqTimestamp,
				ResTimestamp: resTimestamp,
			}}, nil
		}
	}

	resTimestamp := time.Now()
	return []RawMockEntry{{
		ReqPackets:   reqPackets,
		RespPackets:  respPackets,
		CmdType:      mysql.HandshakeV10,
		MockType:     "config",
		ReqTimestamp: reqTimestamp,
		ResTimestamp: resTimestamp,
	}}, nil
}
