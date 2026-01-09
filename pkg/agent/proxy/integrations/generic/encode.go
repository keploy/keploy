package generic

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func encodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {

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
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}
	var genericResponses []models.Payload

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)
	//TODO: where to close the error channel since it is used in both the go routines
	//close(errChan)

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

	// ticker := time.NewTicker(1 * time.Second)
	logger.Debug("the iteration for the generic request starts", zap.Int("genericReqs", len(genericRequests)), zap.Int("genericResps", len(genericResponses)))
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
				metadata["connID"] = ctx.Value(models.ClientConnectionIDKey).(string)
				// Save the mock
				mock := &models.Mock{
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
				if opts.Synchronous {
					if mgr := syncMock.Get(); mgr != nil {
						mgr.AddMock(mock)
						return ctx.Err()
					}
				}
				mocks <- mock
				return ctx.Err()
			}
		case buffer := <-clientBuffChan:
			payload := string(buffer)
			if strings.Contains(payload, "saslStart") {
				// SCRAM payloads usually contain "n=username,r=nonce"
				// We want to extract the value after "n="
				var username string
				if idx := strings.Index(payload, "n="); idx != -1 {
					start := idx + 2
					rest := payload[start:]
					if end := strings.Index(rest, ","); end != -1 {
						username = rest[:end]
					} else {
						// Case where comma might be encoded or at end
						username = rest
					}
				}

				// If we found a username, check if we have a password for it
				if username != "" {
					// Check the new map (opts.MongoPasswords)
					if _, ok := opts.MongoPasswords[username]; ok {
						logger.Debug("Found configured password for MongoDB user", zap.String("user", username))
					} else {
						// Fallback check
						if opts.MongoPassword != "" {
							logger.Debug("Using default password for MongoDB user", zap.String("user", username))
						} else {
							logger.Warn("MongoDB login detected but NO password configured for user", zap.String("user", username))
						}
					}
				}
			}
			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			logger.Debug("the iteration for the generic request ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				genericRequestsCopy := make([]models.Payload, len(genericRequests))
				genericResponseCopy := make([]models.Payload, len(genericResponses))
				copy(genericResponseCopy, genericResponses)
				copy(genericRequestsCopy, genericRequests)
				reqTS := reqTimestampMock
				resTS := resTimestampMock
				go func(reqs []models.Payload, resps []models.Payload, reqTimestamp, resTimestamp time.Time) {
					metadata := make(map[string]string)
					metadata["type"] = "config"
					metadata["connID"] = ctx.Value(models.ClientConnectionIDKey).(string)
					// create the mock
					mock := &models.Mock{
						Version: models.GetVersion(),
						Name:    "mocks",
						Kind:    models.GENERIC,
						Spec: models.MockSpec{
							GenericRequests:  reqs,
							GenericResponses: resps,
							ReqTimestampMock: reqTimestamp,
							ResTimestampMock: resTimestamp,
							Metadata:         metadata,
						},
					}
					if opts.Synchronous {
						if mgr := syncMock.Get(); mgr != nil {
							mgr.AddMock(mock)
						}
					} else {
						mocks <- mock
					}

				}(genericRequestsCopy, genericResponseCopy, reqTS, resTS)
				genericRequests = []models.Payload{}
				genericResponses = []models.Payload{}
			}

			bufStr := string(buffer)
			buffDataType := models.String
			if !util.IsASCII(string(buffer)) {
				bufStr = util.EncodeBase64(buffer)
				buffDataType = "binary"
			}

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

			logger.Debug("the iteration for the generic response ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			prevChunkWasReq = false
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
