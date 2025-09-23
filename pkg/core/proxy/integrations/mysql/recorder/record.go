//go:build linux

// Package recorder is used to record the MySQL traffic between the client and the server.
package recorder

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// Binary to Mock Yaml

func Record(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *yaml.NetworkTrafficDoc, opts models.OutgoingOptions) error {

	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	errCh := make(chan error, 1)

	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		// Helper struct for decoding packets
		decodeCtx := &wire.DecodeContext{
			Mode: models.MODE_RECORD,
			// Map for storing last operation per connection
			LastOp: wire.NewLastOpMap(),
			// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
			ServerGreetings: wire.NewGreetings(),
			// Map for storing prepared statements per connection
			PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		}
		decodeCtx.LastOp.Store(clientConn, wire.RESET) //resetting last command for new loop

		// handle the initial client-server handshake (connection phase)
		result, err := handleInitialHandshake(ctx, logger, clientConn, destConn, decodeCtx, opts)
		if err != nil {
			utils.LogError(logger, err, "failed to handle initial handshake")
			errCh <- err
			return nil
		}
		requests = append(requests, result.req...)
		responses = append(responses, result.resp...)

		reqTimestamp := result.reqTimestamp

		recordMock(ctx, logger, requests, responses, "config", result.requestOperation, result.responseOperation, mocks, reqTimestamp)

		// reset the requests and responses
		requests = []mysql.Request{}
		responses = []mysql.Response{}

		if decodeCtx.UseSSL {
			if result.tlsClientConn == nil || result.tlsDestConn == nil {
				utils.LogError(logger, err, "Expected Tls connections are nil", zap.Any("tlsClientConn", result.tlsClientConn), zap.Any("tlsDestConn", result.tlsDestConn))
				errCh <- errors.New("tls connection is not established")
				return nil
			}
			clientConn = result.tlsClientConn
			destConn = result.tlsDestConn
		}

		lstOp, _ := decodeCtx.LastOp.Load(clientConn)
		logger.Debug("last operation after initial handshake", zap.Any("last operation", lstOp))

		// handle the client-server interaction (command phase)
		err = handleClientQueries(ctx, logger, clientConn, destConn, mocks, decodeCtx)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to handle client queries")
			}
			errCh <- err
			return nil
		}
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

func recordMock(ctx context.Context, logger *zap.Logger, requests []mysql.Request, responses []mysql.Response, mockType, requestOperation, responseOperation string, mocks chan<- *yaml.NetworkTrafficDoc, reqTimestampMock time.Time) {
	meta := map[string]string{
		"type":              mockType,
		"requestOperation":  requestOperation,
		"responseOperation": responseOperation,
		"connID":            ctx.Value(models.ClientConnectionIDKey).(string),
	}

	// Create MySQL spec with requests and responses
	requestsYaml := []mysql.RequestYaml{}
	for _, v := range requests {
		req := mysql.RequestYaml{
			Header: v.Header,
			Meta:   v.Meta,
		}
		err := req.Message.Encode(v.Message)
		if err != nil {
			utils.LogError(logger, err, "failed to encode mysql request wiremessage into yaml")
			return
		}
		requestsYaml = append(requestsYaml, req)
	}

	responsesYaml := []mysql.ResponseYaml{}
	for _, v := range responses {
		resp := mysql.ResponseYaml{
			Header: v.Header,
			Meta:   v.Meta,
		}
		err := resp.Message.Encode(v.Message)
		if err != nil {
			utils.LogError(logger, err, "failed to encode mysql response wiremessage into yaml")
			return
		}
		responsesYaml = append(responsesYaml, resp)
	}

	sqlSpec := mysql.Spec{
		Metadata:         meta,
		Requests:         requestsYaml,
		Response:         responsesYaml,
		CreatedAt:        time.Now().Unix(),
		ReqTimestampMock: reqTimestampMock,
		ResTimestampMock: time.Now(),
	}

	// Create NetworkTrafficDoc with serialized YAML spec
	yamlDoc := yaml.NetworkTrafficDoc{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Name:    mockType,
	}

	err := yamlDoc.Spec.Encode(sqlSpec)
	if err != nil {
		utils.LogError(logger, err, "failed to marshal the MySQL input-output as yaml")
		return
	}

	mocks <- &yamlDoc
}
