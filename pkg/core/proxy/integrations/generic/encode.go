package generic

import (
	"context"
	"encoding/base64"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"net"
	"strconv"
	"time"
)

func encodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	//closing the destination conn
	defer func(destConn net.Conn) {
		err := destConn.Close()
		if err != nil {
			logger.Error("failed to close the destination connection", zap.Error(err))
		}
	}(destConn)

	var genericRequests []models.GenericPayload

	bufStr := string(reqBuf)
	dataType := models.String
	if !util.IsAsciiPrintable(string(reqBuf)) {
		bufStr = util.EncodeBase64(reqBuf)
		dataType = "binary"
	}

	if bufStr != "" {
		genericRequests = append(genericRequests, models.GenericPayload{
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
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	var genericResponses []models.GenericPayload

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)

	// read requests from client
	go func() {
		defer utils.Recover(logger)
		pUtil.ReadBuffConn(ctx, logger, clientConn, clientBuffChan, errChan)
	}()
	// read responses from destination
	go func() {
		defer utils.Recover(logger)
		pUtil.ReadBuffConn(ctx, logger, destConn, destBuffChan, errChan)
	}()

	prevChunkWasReq := false
	var reqTimestampMock = time.Now()
	var resTimestampMock time.Time

	// ticker := time.NewTicker(1 * time.Second)
	logger.Debug("the iteration for the generic request starts", zap.Any("genericReqs", len(genericRequests)), zap.Any("genericResps", len(genericResponses)))
	for {
		select {
		case <-ctx.Done():
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				genericRequestsCopy := make([]models.GenericPayload, len(genericRequests))
				genericResponsesCopy := make([]models.GenericPayload, len(genericResponses))
				copy(genericResponsesCopy, genericResponses)
				copy(genericRequestsCopy, genericRequests)

				metadata := make(map[string]string)
				metadata["type"] = "config"
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
				return nil
			}
		case buffer := <-clientBuffChan:
			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return err
			}

			logger.Debug("the iteration for the generic request ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				genericRequestsCopy := make([]models.GenericPayload, len(genericRequests))
				genericResponseCopy := make([]models.GenericPayload, len(genericResponses))
				copy(genericResponseCopy, genericResponses)
				copy(genericRequestsCopy, genericRequests)
				go func(reqs []models.GenericPayload, resps []models.GenericPayload) {
					metadata := make(map[string]string)
					metadata["type"] = "config"
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
				genericRequests = []models.GenericPayload{}
				genericResponses = []models.GenericPayload{}
			}

			bufStr := string(buffer)
			buffDataType := models.String
			if !util.IsAsciiPrintable(string(buffer)) {
				bufStr = util.EncodeBase64(buffer)
				buffDataType = "binary"
			}

			if bufStr != "" {
				genericRequests = append(genericRequests, models.GenericPayload{
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
				logger.Error("failed to write response to the client", zap.Error(err))
				return err
			}

			bufStr := string(buffer)
			buffDataType := models.String
			if !util.IsAsciiPrintable(string(buffer)) {
				bufStr = base64.StdEncoding.EncodeToString(buffer)
				buffDataType = "binary"
			}

			if bufStr != "" {
				genericResponses = append(genericResponses, models.GenericPayload{
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

			logger.Debug("the iteration for the generic response ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			prevChunkWasReq = false
		case err := <-errChan:
			return err
		}
	}
}
