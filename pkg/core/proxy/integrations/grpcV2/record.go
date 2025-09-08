//go:build linux

package grpcV2

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg"
	temputils "go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func RecordIncoming(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, t chan *models.TestCase) error {
	return recordIncomingTestCase(ctx, logger, clientConn, destConn, t)
}

type grpcTestCaseProxy struct {
	logger    *zap.Logger
	destConn  net.Conn
	testCases chan *models.TestCase // Note: This is now a TestCase channel
	ctx       context.Context
	ccMu      sync.Mutex
	cc        *grpc.ClientConn
}

func recordIncomingTestCase(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, t chan *models.TestCase) error {
	proxy := &grpcTestCaseProxy{
		logger:    logger,
		destConn:  destConn,
		testCases: t,
		ctx:       ctx,
	}

	// Defer connection closures with nil checks
	defer func() {
		if err := clientConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error("failed to close client connection in test case mode", zap.Error(err))
		}
		if err := destConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error("failed to close destination connection in test case mode", zap.Error(err))
		}

		if proxy.cc != nil {
			if err := proxy.cc.Close(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Error("failed to close gRPC client connection in test case mode", zap.Error(err))
			}
		}
	}()

	srv := grpc.NewServer(
		grpc.UnknownServiceHandler(proxy.handler),
		grpc.ForceServerCodec(new(rawCodec)),
	)

	lis := newSingleConnListener(clientConn)
	logger.Info("starting gRPC test case capture server")

	srvErr := make(chan error, 1)

	go func() {
		err := srv.Serve(lis)
		logger.Info("gRPC server has stopped, sending error to channel", zap.Error(err))
		srvErr <- err
	}()

	select {
	case <-ctx.Done():
		go srv.GracefulStop()
		logger.Debug("waiting for gRPC test case capture server to stop")
		err := <-srvErr
		logger.Info("gRPC test case capture server stopped gracefully", zap.Error(err))
		return ctx.Err()
	case err := <-srvErr:
		switch {
		case err == nil,
			err == io.EOF,
			strings.Contains(err.Error(), "connection reset by peer"),
			strings.Contains(err.Error(), "use of closed network connection"):
			logger.Info("gRPC test case capture server stopped gracefully")
			return nil
		default:
			logger.Error("gRPC test case capture server failed", zap.Error(err))
			return err
		}
	}
}

func recordOutgoing(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock) error {
	// Ensure connections are closed on exit
	cid, ok := ctx.Value(models.ClientConnectionIDKey).(string)
	if !ok {
		return status.Errorf(codes.Internal, "missing ClientConnectionID in context")
	}
	proxy := &grpcRecordingProxy{
		logger:   logger,
		destConn: destConn,
		mocks:    mocks,
		connID:   cid,
	}

	defer func() {
		if err := clientConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error("failed to close client connection in record mode", zap.Error(err))
		}
		if err := destConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Error("failed to close destination connection in record mode", zap.Error(err))
		}
		// Close the grpc.ClientConn if it was created.
		if proxy.cc != nil {
			err := proxy.cc.Close()
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Error("failed to close gRPC client connection in record mode", zap.Error(err))
			}
		}
	}()

	// Create a gRPC server to handle the client's request
	srv := grpc.NewServer(
		grpc.UnknownServiceHandler(proxy.handler),
		grpc.ForceServerCodec(new(rawCodec)),
	)

	lis := newSingleConnListener(clientConn)
	logger.Info("starting recording gRPC proxy server")

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(lis) }()

	select {
	case <-ctx.Done():
		// Gracefully shut down once the recorder context is cancelled.
		go srv.GracefulStop()
		logger.Debug("waiting for gRPC recording proxy server to stop")
		err := <-srvErr
		logger.Info("gRPC recording proxy server stopped gracefully", zap.Error(err))
		return ctx.Err()
	case err := <-srvErr:
		switch {
		case err == nil,
			err == io.EOF,
			strings.Contains(err.Error(), "connection reset by peer"),
			strings.Contains(err.Error(), "use of closed network connection"):
			logger.Info("gRPC recording proxy stopped gracefully")
			return nil
		default:
			logger.Error("gRPC recording proxy server failed", zap.Error(err))
			return err
		}
	}

}

// grpcRecordingProxy proxies gRPC calls, recording the request and response.
type grpcRecordingProxy struct {
	logger   *zap.Logger
	destConn net.Conn
	mocks    chan<- *models.Mock
	connID   string
	ccMu     sync.Mutex       // protects cc
	cc       *grpc.ClientConn // reused for all streams on this TCP conn
}

// getClientConn returns the (lazily-constructed) grpc.ClientConn that
// multiplexes over p.destConn.
func (p *grpcRecordingProxy) getClientConn(ctx context.Context) (*grpc.ClientConn, error) {
	p.ccMu.Lock()
	defer p.ccMu.Unlock()

	if p.cc != nil {
		s := p.cc.GetState()
		p.logger.Debug("checking gRPC client connection state",
			zap.String("state", s.String()),
			zap.String("connID", p.connID))
		if s != connectivity.Ready && s != connectivity.Connecting {
			_ = p.cc.Close() // ignore error
			// p.cc = nil       // force re-dial
			return nil, io.EOF
		}
	}

	if p.cc != nil {
		return p.cc, nil
	}

	p.logger.Debug("creating new gRPC client connection because p.cc is nil", zap.Any("p.cc", p.cc))

	dialer := func(context.Context, string) (net.Conn, error) { return p.destConn, nil }

	target := p.destConn.RemoteAddr().String() // or explicit host:port string
	cc, err := grpc.DialContext(
		ctx,
		target,
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(passthroughCodec{})),
	)
	if err != nil {
		return nil, err
	}
	p.cc = cc
	return cc, nil
}

// handler is the core of the proxy. It receives a call, forwards it, and records the interaction.
func (p *grpcRecordingProxy) handler(_ interface{}, clientStream grpc.ServerStream) error {
	p.logger.Debug("received gRPC call")
	startTime := time.Now()
	clientCtx := clientStream.Context()
	fullMethod, _ := grpc.MethodFromServerStream(clientStream)
	connID := p.connID
	if connID == "" {
		connID = "0" // graceful fallback
	}

	md, _ := metadata.FromIncomingContext(clientCtx)

	p.logger.Info("proxying gRPC request", zap.String("method", fullMethod), zap.Any("metadata", md))

	// 1. Obtain (or create once) the grpc.ClientConn that sits on destConn
	destClientConn, err := p.getClientConn(clientCtx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			p.logger.Warn("gRPC client connection is closed, cannot forward request", zap.Error(err))
			return io.EOF
		}
		p.logger.Error("failed to dial destination server", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to connect to destination: %v", err)
	}

	if destClientConn == nil {
		p.logger.Error("destination client connection is nil")
		return status.Errorf(codes.Internal, "destination client connection is nil")
	}

	// 2. Forward the call to the destination
	downstreamCtx, cancelDownstream := context.WithCancel(clientCtx)
	defer cancelDownstream()

	// ── Clean metadata: gRPC forbids user-supplied pseudo headers ("*:").
	cleanMD := metadata.New(nil)
	for k, v := range md {
		fmt.Printf("[MD] key: %s, value: %s\n", k, v)
		if strings.HasPrefix(k, ":") {
			continue // strip pseudo-headers
		}
		cleanMD[k] = v
	}

	downstreamCtx = metadata.NewOutgoingContext(downstreamCtx, cleanMD)
	destStream, err := destClientConn.NewStream(downstreamCtx, &grpc.StreamDesc{
		StreamName:    fullMethod,
		ServerStreams: true,
		ClientStreams: true,
	}, fullMethod)
	if err != nil {
		if downstreamCtx.Err() != nil {
			p.logger.Warn("context cancelled before creating stream to destination", zap.Error(downstreamCtx.Err()))
			return status.Errorf(codes.Canceled, "context cancelled before creating stream to destination: %v", downstreamCtx.Err())
		}
		p.logger.Error("failed to create new stream to destination", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to create stream to destination: %v", err)
	}

	// 3. Goroutines to proxy data in both directions and capture it
	var wg sync.WaitGroup
	var reqErr, respErr error
	var reqBuf, respBuf bytes.Buffer

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			reqMsg := new(rawMessage)
			reqErr = clientStream.RecvMsg(reqMsg)
			if reqErr == io.EOF {
				err := destStream.CloseSend()
				p.logger.Debug("client stream closed", zap.Error(err))
				if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
					p.logger.Error("failed to close send stream to destination", zap.Error(err))
					cancelDownstream()
				}
				return
			}
			if reqErr != nil {
				p.logger.Error("failed to receive message from client", zap.Error(reqErr))
				cancelDownstream()
				return
			}
			p.logger.Debug("received message from client", zap.Int("size", len(reqMsg.data)),
				zap.String("Msg", reqMsg.String()))

			// append keploy at the end of message
			// reqBuf.Write([]byte("keploy"))

			reqBuf.Write(reqMsg.data)
			if err := destStream.SendMsg(reqMsg); err != nil {
				p.logger.Error("failed to send message to destination", zap.Error(err))
				reqErr = err
				return
			}
		}
	}()

	respHeader := metadata.MD{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		header, err := destStream.Header()
		if err != nil {
			p.logger.Warn("failed to get headers from destination stream", zap.Error(err))
			respErr = err
			return
		}

		respHeader = header
		if err := clientStream.SendHeader(header); err != nil {
			p.logger.Error("failed to send headers to client", zap.Error(err))
			respErr = err
			return
		}
		for {
			respMsg := new(rawMessage)
			p.logger.Debug("received message from server", zap.Int("size", len(respMsg.data)),
				zap.String("Msg", respMsg.String()))

			respErr = destStream.RecvMsg(respMsg)
			if respErr != nil {
				p.logger.Debug("received Error from destination", zap.Error(respErr))
			}
			switch respErr {
			case nil:
				// normal message – relay it
			case io.EOF:
				p.logger.Debug("destination stream closed due to EOF")
				return // clean finish
			default:
				// gRPC status error (business failure) is *expected*; just stop reading.
				if _, ok := status.FromError(respErr); ok {
					return
				}
				// real transport problem – still log it.
				p.logger.Error("failed to receive message from destination",
					zap.Error(respErr))
				return
			}
			respBuf.Write(respMsg.data)
			if err := clientStream.SendMsg(respMsg); err != nil {
				p.logger.Error("failed to send message to client", zap.Error(err))
				respErr = err
				return
			}
		}
	}()

	wg.Wait()

	// 4. Finalize and record
	endTime := time.Now()
	destTrailers := destStream.Trailer()
	clientStream.SetTrailer(destTrailers)
	// ────────────────────────────────────────────────────────────────
	// Construct & enqueue the mock **before** we possibly return.
	// ────────────────────────────────────────────────────────────────
	grpcReq := &models.GrpcReq{
		Body:    createLengthPrefixedMessage(reqBuf.Bytes()),
		Headers: p.grpcMetadataToHeaders(md, fullMethod, false),
	}

	p.logger.Debug("headers and trailer of grpc response",
		zap.Any("headers", respHeader),
		zap.Any("trailers", destTrailers))

	body64 := base64.StdEncoding.EncodeToString(respBuf.Bytes())

	p.logger.Debug("Grpc Response body", zap.Int("body size", len(respBuf.Bytes())), zap.Any("body", respBuf.String()), zap.Any("body64", body64))
	// respHeader, _ := destStream.Header()
	grpcResp := &models.GrpcResp{
		Body:     createLengthPrefixedMessage(respBuf.Bytes()),
		Headers:  p.grpcMetadataToHeaders(respHeader, "", true),
		Trailers: p.grpcMetadataToHeaders(destTrailers, "", true),
	}

	//------------------------------------------------------------------
	// If the server terminated the stream with a status error, make
	// sure we record **that** code & message instead of default “0”.
	//------------------------------------------------------------------
	if st, ok := status.FromError(respErr); ok && respErr != nil {
		grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = fmt.Sprintf("%d", st.Code())
		grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = st.Message()
		// Per gRPC spec error responses have no body – keep what we already
		// captured (likely empty) but that’s harmless.
	} else {
		// Ensure mandatory keys are present for the happy-path case.
		if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-status"]; !ok {
			grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = "0"
		}
		if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-message"]; !ok {
			grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = ""
		}
	}

	p.mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": connID},
			GRPCReq:          grpcReq,
			GRPCResp:         grpcResp,
			ReqTimestampMock: startTime,
			ResTimestampMock: endTime,
		},
	}
	p.logger.Info("successfully recorded gRPC interaction", zap.String("method", fullMethod))

	// ────────────────────────────────────────────────────────────────
	// Now decide what to return to the client.
	// ────────────────────────────────────────────────────────────────

	// Treat normal end-of-stream (EOF) and context cancellation as success.
	benign := func(err error) bool {
		return err == nil || err == io.EOF || errors.Is(err, context.Canceled)
	}
	if !benign(reqErr) {
		return status.Errorf(codes.Internal, "error during request forwarding: %v", reqErr)
	}

	// If the server ended the call with a (business) gRPC status error,
	// propagate that **after** recording.
	if s, ok := status.FromError(respErr); ok && respErr != nil {
		p.logger.Debug("received gRPC status error from destination",
			zap.String("code", s.Code().String()),
			zap.String("message", s.Message()))

		return s.Err()
	}

	// Any other non-benign transport error?
	if !benign(respErr) {
		return status.Errorf(codes.Internal,
			"error during response forwarding: %v", respErr)
	}

	return nil
}

// grpcMetadataToHeaders converts gRPC metadata to Keploy's header format.
func (p *grpcRecordingProxy) grpcMetadataToHeaders(md metadata.MD, fullMethod string, isResponse bool) models.GrpcHeaders {
	hdr := models.GrpcHeaders{
		PseudoHeaders:   make(map[string]string),
		OrdinaryHeaders: make(map[string]string),
	}

	for k, v := range md {
		val := strings.Join(v, ", ")
		if strings.HasPrefix(k, ":") {
			hdr.PseudoHeaders[k] = val
		} else {
			hdr.OrdinaryHeaders[k] = val
		}
	}

	if !isResponse {
		if _, ok := hdr.PseudoHeaders[":method"]; !ok {
			hdr.PseudoHeaders[":method"] = "POST"
		}
		if _, ok := hdr.PseudoHeaders[":scheme"]; !ok {
			hdr.PseudoHeaders[":scheme"] = "http"
		}
		if _, ok := hdr.PseudoHeaders[":path"]; !ok {
			hdr.PseudoHeaders[":path"] = fullMethod
		}
		hdr.OrdinaryHeaders["te"] = "trailers" // new – stable field
	} else {
		// if _, ok := hdr.PseudoHeaders[":status"]; !ok {
		// 	hdr.PseudoHeaders[":status"] = "200"
		// }
		// if ct, ok := hdr.OrdinaryHeaders["content-type"]; ok &&
		// 	strings.HasPrefix(ct, "application/grpc") {
		// 	hdr.OrdinaryHeaders["content-type"] = "application/grpc"
		// }
	}
	return hdr
}

func (p *grpcTestCaseProxy) getClientConn(ctx context.Context) (*grpc.ClientConn, error) {
	p.ccMu.Lock()
	defer p.ccMu.Unlock()

	if p.cc != nil {
		s := p.cc.GetState()
		p.logger.Debug("checking gRPC client connection state", zap.String("state", s.String()))
		if s != connectivity.Ready && s != connectivity.Connecting {
			_ = p.cc.Close() // ignore error
			return nil, io.EOF
		}
	}

	if p.cc != nil {
		return p.cc, nil
	}

	p.logger.Debug("creating new gRPC client connection because p.cc is nil")

	dialer := func(context.Context, string) (net.Conn, error) { return p.destConn, nil }

	target := p.destConn.RemoteAddr().String()
	cc, err := grpc.DialContext(
		ctx,
		target,
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(passthroughCodec{})),
	)
	if err != nil {
		return nil, err
	}
	p.cc = cc
	return cc, nil
}

func (p *grpcTestCaseProxy) handler(_ interface{}, clientStream grpc.ServerStream) error {
	p.logger.Debug("received gRPC call for test case capture")
	startTime := time.Now()
	clientCtx := clientStream.Context()
	fullMethod, _ := grpc.MethodFromServerStream(clientStream)

	md, _ := metadata.FromIncomingContext(clientCtx)
	p.logger.Info("proxying gRPC request for test case capture", zap.String("method", fullMethod))

	destClientConn, err := p.getClientConn(clientCtx)
	if err != nil {
		p.logger.Error("failed to dial destination server", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to connect to destination: %v", err)
	}

	downstreamCtx, cancelDownstream := context.WithCancel(clientCtx)
	defer cancelDownstream()

	cleanMD := metadata.New(nil)
	for k, v := range md {
		if strings.HasPrefix(k, ":") {
			continue
		}
		cleanMD[k] = v
	}

	downstreamCtx = metadata.NewOutgoingContext(downstreamCtx, cleanMD)
	destStream, err := destClientConn.NewStream(downstreamCtx, &grpc.StreamDesc{
		StreamName:    fullMethod,
		ServerStreams: true,
		ClientStreams: true,
	}, fullMethod)
	if err != nil {
		p.logger.Error("failed to create new stream to destination", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to create stream to destination: %v", err)
	}

	var wg sync.WaitGroup
	var reqErr, respErr error
	var reqBuf, respBuf bytes.Buffer

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			reqMsg := new(rawMessage)
			reqErr = clientStream.RecvMsg(reqMsg)
			if reqErr != nil { // Handles io.EOF and other errors
				destStream.CloseSend()
				return
			}
			reqBuf.Write(reqMsg.data)
			if err := destStream.SendMsg(reqMsg); err != nil {
				reqErr = err
				return
			}
		}
	}()

	respHeader := metadata.MD{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		header, err := destStream.Header()
		if err != nil {
			respErr = err
			return
		}

		respHeader = header
		if err := clientStream.SendHeader(header); err != nil {
			respErr = err
			return
		}
		for {
			respMsg := new(rawMessage)
			respErr = destStream.RecvMsg(respMsg)
			if respErr != nil { // Handles io.EOF and status errors
				return
			}
			respBuf.Write(respMsg.data)
			if err := clientStream.SendMsg(respMsg); err != nil {
				respErr = err
				return
			}
		}
	}()

	// Wait for both goroutines to finish data transfer.
	wg.Wait()

	endTime := time.Now()
	destTrailers := destStream.Trailer()
	clientStream.SetTrailer(destTrailers)

	grpcReq := &models.GrpcReq{
		Body:      createLengthPrefixedMessage(reqBuf.Bytes()),
		Headers:   p.grpcMetadataToHeaders(md, fullMethod, false),
		Timestamp: startTime,
	}

	grpcResp := &models.GrpcResp{
		Body:      createLengthPrefixedMessage(respBuf.Bytes()),
		Headers:   p.grpcMetadataToHeaders(respHeader, "", true),
		Trailers:  p.grpcMetadataToHeaders(destTrailers, "", true),
		Timestamp: endTime,
	}

	if st, ok := status.FromError(respErr); ok && respErr != nil {
		grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = fmt.Sprintf("%d", st.Code())
		grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = st.Message()
	} else {
		if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-status"]; !ok {
			grpcResp.Trailers.OrdinaryHeaders["grpc-status"] = "0"
		}
		if _, ok := grpcResp.Trailers.OrdinaryHeaders["grpc-message"]; !ok {
			grpcResp.Trailers.OrdinaryHeaders["grpc-message"] = ""
		}
	}

	http2Stream := &pkg.HTTP2Stream{
		ID:       0,
		GRPCReq:  grpcReq,
		GRPCResp: grpcResp,
	}
	temputils.CaptureGRPC(p.ctx, p.logger, p.testCases, http2Stream)

	if s, ok := status.FromError(respErr); ok && respErr != nil {
		return s.Err()
	}

	return nil
}

func (p *grpcTestCaseProxy) grpcMetadataToHeaders(md metadata.MD, fullMethod string, isResponse bool) models.GrpcHeaders {
	hdr := models.GrpcHeaders{
		PseudoHeaders:   make(map[string]string),
		OrdinaryHeaders: make(map[string]string),
	}

	for k, v := range md {
		val := strings.Join(v, ", ")
		if strings.HasPrefix(k, ":") {
			hdr.PseudoHeaders[k] = val
		} else {
			hdr.OrdinaryHeaders[k] = val
		}
	}

	if !isResponse {
		if _, ok := hdr.PseudoHeaders[":method"]; !ok {
			hdr.PseudoHeaders[":method"] = "POST"
		}
		if _, ok := hdr.PseudoHeaders[":scheme"]; !ok {
			hdr.PseudoHeaders[":scheme"] = "http"
		}
		if _, ok := hdr.PseudoHeaders[":path"]; !ok {
			hdr.PseudoHeaders[":path"] = fullMethod
		}
		if _, ok := hdr.OrdinaryHeaders["te"]; !ok {
			hdr.OrdinaryHeaders["te"] = "trailers"
		}
	} else {
		// Response headers are handled as they are received.
		// No special default values are typically needed.
	}
	return hdr
}
