package generic

import (
	"context"
	"encoding/base64"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func encodeGeneric(ctx context.Context, logger *zap.Logger, req []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	genericRequests := []models.GenericPayload{}

	bufStr := string(req)
	dataType := models.String
	if !util.IsAsciiPrintable(string(req)) {
		bufStr = base64.StdEncoding.EncodeToString(req)
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
	_, err := destConn.Write(req)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	genericResponses := []models.GenericPayload{}

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)

	// read requests from client
	go func() {
		pUtil.ReadBuffConn(ctx, logger, clientConn, clientBuffChan, errChan)
	}()
	// read response from destination
	go func() {
		pUtil.ReadBuffConn(ctx, logger, destConn, destBuffChan, errChan)
	}()

	prevChunkWasReq := false
	var reqTimestampMock time.Time = time.Now()
	var resTimestampMock time.Time

	// ticker := time.NewTicker(1 * time.Second)
	logger.Debug("the iteration for the generic request starts", zap.Any("genericReqs", len(genericRequests)), zap.Any("genericResps", len(genericResponses)))
	for {

		// start := time.NewTicker(1*time.Second)
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		select {
		// case <-start.C:
		case <-sigChan:
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				genericRequestsCopy := make([]models.GenericPayload, len(genericRequests))
				genericResponseCopy := make([]models.GenericPayload, len(genericResponses))
				copy(genericResponseCopy, genericResponses)
				copy(genericRequestsCopy, genericRequests)
				go func(reqs []models.GenericPayload, resps []models.GenericPayload) {
					metadata := make(map[string]string)
					metadata["type"] = "config"
					h.AppendMocks(&models.Mock{
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
					}, ctx)
				}(genericRequestsCopy, genericResponseCopy)
				clientConn.Close()
				destConn.Close()
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
					h.AppendMocks(&models.Mock{
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
					}, ctx)
				}(genericRequestsCopy, genericResponseCopy)
				genericRequests = []models.GenericPayload{}
				genericResponses = []models.GenericPayload{}
			}

			bufStr := string(buffer)
			buffrDataType := models.String
			if !util.IsAsciiPrintable(string(buffer)) {
				bufStr = base64.StdEncoding.EncodeToString(buffer)
				buffrDataType = "binary"
			}

			if bufStr != "" {
				genericRequests = append(genericRequests, models.GenericPayload{
					Origin: models.FromClient,
					Message: []models.OutputBinary{
						{
							Type: buffrDataType,
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
			buffrDataType := models.String
			if !util.IsAsciiPrintable(string(buffer)) {
				bufStr = base64.StdEncoding.EncodeToString(buffer)
				buffrDataType = "binary"
			}

			if bufStr != "" {

				genericResponses = append(genericResponses, models.GenericPayload{
					Origin: models.FromServer,
					Message: []models.OutputBinary{
						{
							Type: buffrDataType,
							Data: bufStr,
						},
					},
				})
			}

			resTimestampMock = time.Now()

			logger.Debug("the iteration for the generic response ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			isPreviousChunkRequest = false
		case err := <-errChannel:
			return err
		}

	}
}
