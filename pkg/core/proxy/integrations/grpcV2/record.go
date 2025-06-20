//go:build linux

package grpcV2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// recordOutgoing starts a gRPC proxy to record a session.
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
		<-srvErr
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
		return p.cc, nil
	}

	dialer := func(context.Context, string) (net.Conn, error) { return p.destConn, nil }
	cc, err := grpc.NewClient(
		"",
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
		p.logger.Error("failed to dial destination server", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to connect to destination: %v", err)
	}

	// 2. Forward the call to the destination
	downstreamCtx, cancelDownstream := context.WithCancel(clientCtx)
	defer cancelDownstream()

	// ── Clean metadata: gRPC forbids user-supplied pseudo headers ("*:").
	cleanMD := metadata.New(nil)
	for k, v := range md {
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
			reqBuf.Write(reqMsg.data)
			if err := destStream.SendMsg(reqMsg); err != nil {
				p.logger.Error("failed to send message to destination", zap.Error(err))
				reqErr = err
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		header, err := destStream.Header()
		if err != nil {
			p.logger.Warn("failed to get headers from destination stream", zap.Error(err))
		}
		if err := clientStream.SendHeader(header); err != nil {
			p.logger.Error("failed to send headers to client", zap.Error(err))
			respErr = err
			return
		}
		for {
			respMsg := new(rawMessage)
			respErr = destStream.RecvMsg(respMsg)

			switch respErr {
			case nil:
				// normal message – relay it
			case io.EOF:
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

	respHeader, _ := destStream.Header()
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
