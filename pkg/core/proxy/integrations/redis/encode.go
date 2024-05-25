package redis

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	var redisRequests []models.Payload

	bufStr := string(reqBuf)
	dataType := models.String
	if !util.IsASCII(string(reqBuf)) {
		bufStr = util.EncodeBase64(reqBuf)
		dataType = "binary"
	}

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
	var redisResponses []models.Payload

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)
	//TODO: where to close the error channel since it is used in both the go routines
	//close(errChan)

	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// read requests from client
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(clientBuffChan)
		pUtil.ReadBuffConn(ctx, logger, clientConn, clientBuffChan, errChan)
		return nil
	})
	// read responses from destination
	g.Go(func() error {
		defer pUtil.Recover(logger, nil, destConn)
		defer close(destBuffChan)
		pUtil.ReadBuffConn(ctx, logger, destConn, destBuffChan, errChan)
		return nil
	})

	prevChunkWasReq := false
	var reqTimestampMock = time.Now()
	var resTimestampMock time.Time

	// ticker := time.NewTicker(1 * time.Second)
	logger.Debug("the iteration for the redis request starts", zap.Any("redisReqs", len(redisRequests)), zap.Any("redisResps", len(redisResponses)))
	for {
		select {
		case <-ctx.Done():
			if !prevChunkWasReq && len(redisRequests) > 0 && len(redisResponses) > 0 {
				redisRequestsCopy := make([]models.Payload, len(redisRequests))
				redisResponsesCopy := make([]models.Payload, len(redisResponses))
				copy(redisResponsesCopy, redisResponses)
				copy(redisRequestsCopy, redisRequests)

				metadata := make(map[string]string)
				metadata["type"] = "config"
				// Save the mock
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
				return ctx.Err()
			}
		case buffer := <-clientBuffChan:
			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			logger.Debug("the iteration for the redis request ends with no of redisReqs:" + strconv.Itoa(len(redisRequests)) + " and redisResps: " + strconv.Itoa(len(redisResponses)))
			if !prevChunkWasReq && len(redisRequests) > 0 && len(redisResponses) > 0 {
				redisRequestsCopy := make([]models.Payload, len(redisRequests))
				redisResponseCopy := make([]models.Payload, len(redisResponses))
				copy(redisResponseCopy, redisResponses)
				copy(redisRequestsCopy, redisRequests)
				go func(reqs []models.Payload, resps []models.Payload) {
					metadata := make(map[string]string)
					metadata["type"] = "config"
					// Save the mock
					mocks <- &models.Mock{
						Version: models.GetVersion(),
						Name:    "mocks",
						Kind:    models.REDIS,
						Spec: models.MockSpec{
							RedisRequests:    reqs,
							RedisResponses:   resps,
							ReqTimestampMock: reqTimestampMock,
							ResTimestampMock: resTimestampMock,
							Metadata:         metadata,
						},
					}

				}(redisRequestsCopy, redisResponseCopy)
				redisRequests = []models.Payload{}
				redisResponses = []models.Payload{}
			}

			bufStr := string(buffer)
			buffDataType := models.String
			if !util.IsASCII(string(buffer)) {
				bufStr = util.EncodeBase64(buffer)
				buffDataType = "binary"
			}

			if bufStr != "" {
				redisRequests = append(redisRequests, models.Payload{
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

			if bufStr != "" {
				redisResponses = append(redisResponses, models.Payload{
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

			logger.Debug("the iteration for the redis response ends with no of redisReqs:" + strconv.Itoa(len(redisRequests)) + " and redisResps: " + strconv.Itoa(len(redisResponses)))
			prevChunkWasReq = false
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
