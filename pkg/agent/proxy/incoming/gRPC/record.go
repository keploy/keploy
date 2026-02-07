package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/protocolbuffers/protoscope"
	"go.keploy.io/server/v3/pkg"
	Utils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func RecordIncoming(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, t chan *models.TestCase, appPort uint16) error {
	return recordIncomingTestCase(ctx, logger, clientConn, destConn, t, appPort)
}

func recordIncomingTestCase(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, t chan *models.TestCase, appPort uint16) error {
	proxy := &grpcTestCaseProxy{
		logger:    logger,
		destConn:  destConn,
		testCases: t,
		ctx:       ctx,
		appPort:   appPort,
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
				logger.Debug("failed to close gRPC client connection in test case mode", zap.Error(err))
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
		logger.Debug("gRPC server has stopped, sending error to channel", zap.Error(err))
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

type grpcTestCaseProxy struct {
	logger    *zap.Logger
	destConn  net.Conn
	testCases chan *models.TestCase // Note: This is now a TestCase channel
	ctx       context.Context
	ccMu      sync.Mutex
	cc        *grpc.ClientConn
	appPort   uint16
}

func (p *grpcTestCaseProxy) getClientConn(ctx context.Context) (*grpc.ClientConn, error) {
	p.ccMu.Lock()
	defer p.ccMu.Unlock()

	if p.cc != nil {
		s := p.cc.GetState()
		p.logger.Debug("checking gRPC client connection state", zap.String("state", s.String()))
		if s != connectivity.Ready && s != connectivity.Connecting {
			_ = p.cc.Close()
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
	p.logger.Debug("proxying gRPC request for test case capture", zap.String("method", fullMethod))

	destClientConn, err := p.getClientConn(clientCtx)
	if err != nil {
		p.logger.Debug("failed to dial destination server", zap.Error(err))
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

	Utils.CaptureGRPC(p.ctx, p.logger, p.testCases, http2Stream, p.appPort)

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
		// :method
		if _, ok := hdr.PseudoHeaders[":method"]; !ok {
			hdr.PseudoHeaders[":method"] = "POST"
		}

		// :scheme (keep http as we dial over the provided net.Conn)
		if _, ok := hdr.PseudoHeaders[":scheme"]; !ok {
			hdr.PseudoHeaders[":scheme"] = "http"
		}

		// :path — prefer fullMethod; ensure it's non-empty and starts with '/'
		if _, ok := hdr.PseudoHeaders[":path"]; !ok || hdr.PseudoHeaders[":path"] == "" {
			if fullMethod != "" {
				if !strings.HasPrefix(fullMethod, "/") {
					fullMethod = "/" + fullMethod
				}
				hdr.PseudoHeaders[":path"] = fullMethod
			} else {
				// absolute last resort to avoid "missing :path header"
				hdr.PseudoHeaders[":path"] = "/"
			}
		}

		// :authority — derive from Host header if present, else from destConn
		if _, ok := hdr.PseudoHeaders[":authority"]; !ok || hdr.PseudoHeaders[":authority"] == "" {
			if p.destConn != nil && p.destConn.RemoteAddr() != nil {
				hdr.PseudoHeaders[":authority"] = p.destConn.RemoteAddr().String()
			}
		}

		if _, ok := hdr.OrdinaryHeaders["te"]; !ok {
			hdr.OrdinaryHeaders["te"] = "trailers"
		}
	} else {
		if _, ok := hdr.PseudoHeaders[":status"]; !ok {
			hdr.PseudoHeaders[":status"] = "200"
		}
		if ct, ok := hdr.OrdinaryHeaders["content-type"]; ok &&
			strings.HasPrefix(ct, "application/grpc") {
			hdr.OrdinaryHeaders["content-type"] = "application/grpc"
		}
	}
	return hdr
}

func createLengthPrefixedMessage(data []byte) models.GrpcLengthPrefixedMessage {
	// The original implementation stored the raw bytes as a string, which can
	// safely hold binary data in Go. We will follow this for consistency with
	// the existing fuzzy matching logic.
	return models.GrpcLengthPrefixedMessage{
		// Compression flag is 0 for uncompressed.
		CompressionFlag: 0,
		// MessageLength is the length of the raw data.
		MessageLength: uint32(len(data)),
		// DecodedData holds the text representation of the wire data.
		DecodedData: prettyPrintWire(data, 0),
	}
}

func prettyPrintWire(b []byte, _ int) string {
	// The indent argument is ignored as protoscope handles formatting automatically.
	return protoscope.Write(b, protoscope.WriterOptions{})
}
