//go:build linux

package generic

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	var genericRequests []models.Payload

	bufStr := string(reqBuf)
	dataType := models.String
	if !util.IsASCII(string(reqBuf)) {
		bufStr = util.EncodeBase64(reqBuf)
		dataType = "binary"
	}

	if bufStr != "" {
		genericRequests = append(genericRequests, models.Payload{
			Origin: models.FromClient,
			Message: []models.OutputBinary{
				{
					Type: dataType,
					Data: bufStr,
				},
			},
		})
	}

	// Debug: Log the initial buffer and data type
	logger.Debug("Encoding generic request", zap.String("dataType", dataType), zap.String("bufStr", bufStr))
	// bufStr gives me all the buffer from request

	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}
	var genericResponses []models.Payload

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)

	// Debug: Log that channels are initialized
	logger.Debug("Channels initialized for reading from peers")

	// read requests from client
	err = pUtil.ReadFromPeer(ctx, logger, clientConn, clientBuffChan, errChan, pUtil.Client)
	if err != nil {
		return fmt.Errorf("error reading from client:%v", err)
	}

	// read responses from destination
	err = pUtil.ReadFromPeer(ctx, logger, destConn, destBuffChan, errChan, pUtil.Destination)
	if err != nil {
		return fmt.Errorf("error reading from destination:%v", err)
	}

	prevChunkWasReq := false
	var reqTimestampMock = time.Now()
	var resTimestampMock time.Time

	// Debug: Log the start of the iteration
	logger.Debug("Starting iteration for generic requests and responses", zap.Int("genericRequestsLength", len(genericRequests)), zap.Int("genericResponsesLength", len(genericResponses)))

	for {
		select {
		case <-ctx.Done():
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				genericRequestsCopy := make([]models.Payload, len(genericRequests))
				genericResponsesCopy := make([]models.Payload, len(genericResponses))
				copy(genericResponsesCopy, genericResponses)
				copy(genericRequestsCopy, genericRequests)

				metadata := make(map[string]string)
				metadata["type"] = "config"

				// Debug: Log mock data being saved
				logger.Debug("Saving mock data", zap.Int("genericRequestsLength", len(genericRequestsCopy)), zap.Int("genericResponsesLength", len(genericResponsesCopy)))

				// Save the mock
				mocks <- &models.Mock{
					Version: models.GetVersion(),
					Name:    "mocks",
					Kind:    models.GENERIC,
					Spec: models.MockSpec{
						GenericRequests:  genericRequestsCopy,
						GenericResponses: genericResponsesCopy,
						ReqTimestampMock: reqTimestampMock,
						ResTimestampMock: resTimestampMock,
						Metadata:         metadata,
					},
				}
				return ctx.Err()
			}
		case buffer := <-clientBuffChan:
			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			// Debug: Log request data being written
			logger.Debug("Writing client request to destination", zap.Int("bufferLength", len(buffer)))

			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				genericRequestsCopy := make([]models.Payload, len(genericRequests))
				genericResponseCopy := make([]models.Payload, len(genericResponses))
				copy(genericResponseCopy, genericResponses)
				copy(genericRequestsCopy, genericRequests)
				go func(reqs []models.Payload, resps []models.Payload) {
					metadata := make(map[string]string)
					metadata["type"] = "config"

					// Debug: Log saving the mock data in goroutine
					logger.Debug("Saving mock data in goroutine", zap.Int("genericRequestsLength", len(reqs)), zap.Int("genericResponsesLength", len(resps)))

					// Save the mock
					mocks <- &models.Mock{
						Version: models.GetVersion(),
						Name:    "mocks",
						Kind:    models.GENERIC,
						Spec: models.MockSpec{
							GenericRequests:  reqs,
							GenericResponses: resps,
							ReqTimestampMock: reqTimestampMock,
							ResTimestampMock: resTimestampMock,
							Metadata:         metadata,
						},
					}

				}(genericRequestsCopy, genericResponseCopy)
				genericRequests = []models.Payload{}
				genericResponses = []models.Payload{}
			}

			bufStr := string(buffer)
			buffDataType := models.String
			if !util.IsASCII(string(buffer)) {
				bufStr = util.EncodeBase64(buffer)
				buffDataType = "binary"
			}

			// Debug: Log the buffer content being encoded
			logger.Debug("Encoding request buffer", zap.String("buffDataType", buffDataType), zap.String("bufStr", bufStr))

			if bufStr != "" {
				genericRequests = append(genericRequests, models.Payload{
					Origin: models.FromClient,
					Message: []models.OutputBinary{
						{
							Type: buffDataType,
							Data: bufStr,
						},
					},
				})
			}

			prevChunkWasReq = true
		case buffer := <-destBuffChan:
			if prevChunkWasReq {
				// store the request timestamp
				reqTimestampMock = time.Now()
			}

			// Debug: Log response data being written
			logger.Debug("Writing server response to client", zap.Int("bufferLength", len(buffer)))

			// Write the response message to the client
			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				return err
			}

			bufStr := string(buffer)
			buffDataType := models.String
			if !util.IsASCII(string(buffer)) {
				bufStr = base64.StdEncoding.EncodeToString(buffer)
				buffDataType = "binary"
			}

			// Debug: Log the buffer content being encoded for response
			logger.Debug("Encoding response buffer", zap.String("buffDataType", buffDataType), zap.String("bufStr", bufStr))

			if bufStr != "" {
				genericResponses = append(genericResponses, models.Payload{
					Origin: models.FromServer,
					Message: []models.OutputBinary{
						{
							Type: buffDataType,
							Data: bufStr,
						},
					},
				})
			}

			resTimestampMock = time.Now()

			// Debug: Log the end of the iteration
			logger.Debug("Iteration ends for generic response", zap.Int("genericRequestsLength", len(genericRequests)), zap.Int("genericResponsesLength", len(genericResponses)))

			prevChunkWasReq = false
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
