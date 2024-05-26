package redis

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
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

	logger.Debug("the iteration for the redis request starts", zap.Any("redisReqs", len(redisRequests)), zap.Any("redisResps", len(redisResponses)))
	for {
		select {
		case <-ctx.Done():
			if !prevChunkWasReq && len(redisRequests) > 0 && len(redisResponses) > 0 {
				saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
				return ctx.Err()
			}
		case buffer, ok := <-clientBuffChan:
			if !ok {
				return nil // Channel closed, end the loop
			}
			// Write the request message to the destination
			if _, err := destConn.Write(buffer); err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			processBuffer(buffer, models.FromClient, &redisRequests)
			prevChunkWasReq = true
		case buffer, ok := <-destBuffChan:
			if !ok {
				return nil // Channel closed, end the loop
			}
			if prevChunkWasReq {
				// Store the request timestamp
				reqTimestampMock = time.Now()
			}
			// Write the response message to the client
			if _, err := clientConn.Write(buffer); err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				return err
			}

			processBuffer(buffer, models.FromServer, &redisResponses)
			resTimestampMock = time.Now()

			if prevChunkWasReq && len(redisRequests) > 0 && len(redisResponses) > 0 {
				saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
				redisRequests = []models.Payload{}
				redisResponses = []models.Payload{}
			}

			prevChunkWasReq = false
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func processBuffer(buffer []byte, origin models.OriginType, payloads *[]models.Payload) {
	bufStr := string(buffer)
	buffDataType := models.String
	if !util.IsASCII(bufStr) {
		bufStr = base64.StdEncoding.EncodeToString(buffer)
		buffDataType = "binary"
	}

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
