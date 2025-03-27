//go:build linux

package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	var redisRequests []models.Payload
	var redisResponses []models.Payload

	bufStr := string(reqBuf)
	dataType := models.String

	if bufStr != "" {
		redisRequests = append(redisRequests, models.Payload{
			Origin: models.FromClient,
			Message: []models.OutputBinary{
				{
					Type: dataType,
					Data: bufStr,
				},
			},
		})
	}
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}
	errCh := make(chan error, 1)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	reqTimestampMock := time.Now()

	// Read and process responses from the destination server
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		for {
			// Read the response from the destination server
			resp, err := pUtil.ReadBytes(ctx, logger, destConn)
			if err != nil {
				if err == io.EOF {
					logger.Debug("Response complete, exiting the loop.")
					// if there is any buffer left before EOF, we must send it to the client and save this as mock
					if len(resp) != 0 {
						resTimestampMock := time.Now()
						_, err = clientConn.Write(resp)
						if err != nil {
							utils.LogError(logger, err, "failed to write response message to the client")
							errCh <- err
							return nil
						}
						processBuffer(resp, models.FromServer, &redisResponses)
						saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
					}
					break
				}
				utils.LogError(logger, err, "failed to read the response message from the destination server")
				errCh <- err
				return nil
			}

			logger.Debug("Read response from the destination server", zap.ByteString("response", resp))

			// Write the response message to the client
			_, err = clientConn.Write(resp)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				errCh <- err
				return nil
			}

			resTimestampMock := time.Now()
			processBuffer(resp, models.FromServer, &redisResponses)

			// Save the mock with both request and response
			if len(redisRequests) > 0 && len(redisResponses) > 0 {
				saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
				redisRequests = []models.Payload{}
				redisResponses = []models.Payload{}
			}

			// Read the next request from the client
			reqBuf, err = pUtil.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read the request message from the client")
					errCh <- err
					return nil
				}
				errCh <- err
				return nil
			}

			logger.Debug("Read request from the client", zap.ByteString("request", reqBuf))
			bufStr := string(reqBuf)
			dataType := models.String

			if bufStr != "" {
				redisRequests = append(redisRequests, models.Payload{
					Origin: models.FromClient,
					Message: []models.OutputBinary{
						{
							Type: dataType,
							Data: bufStr,
						},
					},
				})
			}
			_, err = destConn.Write(reqBuf)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				errCh <- err
				return nil
			}
			reqTimestampMock = time.Now()
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

func processBuffer(buffer []byte, origin models.OriginType, payloads *[]models.Payload) {
	bufStr := string(buffer)
	buffDataType := models.String

	if bufStr != "" {
		*payloads = append(*payloads, models.Payload{
			Origin: origin,
			Message: []models.OutputBinary{
				{
					Type: buffDataType,
					Data: bufStr,
				},
			},
		})
	}
}

func saveMock(requests, responses []models.Payload, reqTimestampMock, resTimestampMock time.Time, mocks chan<- *models.Mock) {
	redisRequestsCopy := make([]models.Payload, len(requests))
	redisResponsesCopy := make([]models.Payload, len(responses))
	copy(redisResponsesCopy, responses)
	copy(redisRequestsCopy, requests)

	metadata := make(map[string]string)
	metadata["type"] = "config"

	mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.REDIS,
		Spec: models.MockSpec{
			RedisRequests:    redisRequestsCopy,
			RedisResponses:   redisResponsesCopy,
			ReqTimestampMock: reqTimestampMock,
			ResTimestampMock: resTimestampMock,
			Metadata:         metadata,
		},
	}
}
