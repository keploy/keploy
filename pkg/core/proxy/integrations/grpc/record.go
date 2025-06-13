package grpc

import (
	"context"
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
	defer func() {
		if err := clientConn.Close(); err != nil {
			logger.Error("failed to close client connection in record mode", zap.Error(err))
		}
		if err := destConn.Close(); err != nil {
			logger.Error("failed to close destination connection in record mode", zap.Error(err))
		}
	}()

	proxy := &grpcRecordingProxy{
		logger:   logger,
		destConn: destConn,
		mocks:    mocks,
	}

	// Create a gRPC server to handle the client's request
	srv := grpc.NewServer(
		grpc.UnknownServiceHandler(proxy.handler),
		grpc.ForceServerCodec(new(rawCodec)),
	)

	lis := newSingleConnListener(clientConn)
	logger.Info("starting recording gRPC proxy server")

	err := srv.Serve(lis)
	if err != nil && err != io.EOF && !strings.Contains(err.Error(), "connection reset by peer") {
		logger.Error("gRPC recording proxy server failed", zap.Error(err))
		return err
	}

	logger.Info("gRPC recording proxy stopped gracefully")
	return nil
}

// grpcRecordingProxy proxies gRPC calls, recording the request and response.
type grpcRecordingProxy struct {
	logger   *zap.Logger
	destConn net.Conn
	mocks    chan<- *models.Mock
}

// handler is the core of the proxy. It receives a call, forwards it, and records the interaction.
func (p *grpcRecordingProxy) handler(_ interface{}, clientStream grpc.ServerStream) error {
	startTime := time.Now()
	fullMethod, _ := grpc.MethodFromServerStream(clientStream)
	clientCtx := clientStream.Context()
	md, _ := metadata.FromIncomingContext(clientCtx)

	p.logger.Info("proxying gRPC request", zap.String("method", fullMethod), zap.Any("metadata", md))

	// 1. Create a client connection to the destination server
	dialer := func(context.Context, string) (net.Conn, error) {
		return p.destConn, nil
	}
	destClientConn, err := grpc.DialContext(clientCtx, "",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()), // We are proxying, not terminating TLS
		grpc.WithDefaultCallOptions(grpc.ForceCodec(new(rawCodec))),
	)
	if err != nil {
		p.logger.Error("failed to dial destination server", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to connect to destination: %v", err)
	}
	defer destClientConn.Close()

	// 2. Forward the call to the destination
	downstreamCtx, cancelDownstream := context.WithCancel(clientCtx)
	defer cancelDownstream()

	downstreamCtx = metadata.NewOutgoingContext(downstreamCtx, md.Copy())
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
	var reqBody, respBody []byte

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			reqMsg := new(rawMessage)
			reqErr = clientStream.RecvMsg(reqMsg)
			if reqErr == io.EOF {
				destStream.CloseSend()
				return
			}
			if reqErr != nil {
				p.logger.Error("failed to receive message from client", zap.Error(reqErr))
				cancelDownstream()
				return
			}
			reqBody = reqMsg.data
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
			if respErr == io.EOF {
				return
			}
			if respErr != nil {
				p.logger.Error("failed to receive message from destination", zap.Error(respErr))
				return
			}
			respBody = respMsg.data
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

	if reqErr != nil && reqErr != io.EOF {
		return status.Errorf(codes.Internal, "error during request forwarding: %v", reqErr)
	}
	if respErr != nil && respErr != io.EOF {
		return status.Errorf(codes.Internal, "error during response forwarding: %v", respErr)
	}

	// Construct the mock
	grpcReq := &models.GrpcReq{
		Body:    createLengthPrefixedMessage(reqBody),
		Headers: p.grpcMetadataToHeaders(md, fullMethod, false),
	}

	respHeader, _ := destStream.Header()
	grpcResp := &models.GrpcResp{
		Body:     createLengthPrefixedMessage(respBody),
		Headers:  p.grpcMetadataToHeaders(respHeader, "", true),
		Trailers: p.grpcMetadataToHeaders(destTrailers, "", true),
	}

	p.mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "gRPC"},
			GRPCReq:          grpcReq,
			GRPCResp:         grpcResp,
			ReqTimestampMock: startTime,
			ResTimestampMock: endTime,
		},
	}
	p.logger.Info("successfully recorded gRPC interaction", zap.String("method", fullMethod))

	if respErr != nil {
		if s, ok := status.FromError(respErr); ok {
			return s.Err()
		}
		return status.Errorf(codes.Internal, "destination returned non-gRPC error: %v", respErr)
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
	}
	return hdr
}
