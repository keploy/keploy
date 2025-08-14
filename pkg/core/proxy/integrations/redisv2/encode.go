//go:build linux

package redisv2

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {
	var redisRequests []models.RedisRequests
	var redisResponses []models.RedisResponses
	var reqAccumBuf []byte
	var respAccumBuf []byte

	// accumulate the first request
	reqAccumBuf = append(reqAccumBuf, reqBuf...)

	dataType := models.String
	if !util.IsASCII(string(reqBuf)) {
		bufStr := util.EncodeBase64(reqBuf)
		dataType = "binary"
		logger.Debug("Encoding redis request", zap.String("dataType", dataType), zap.String("bufStr", bufStr))
	} else {
		logger.Debug("Encoding redis request", zap.String("dataType", dataType), zap.ByteString("buf", reqBuf))
	}

	var redisProtocolVersion int
	bufStr := string(reqBuf)
	if len(bufStr) > 0 {
		if strings.Contains(bufStr, "ping") {
			redisProtocolVersion = 2
		} else if strings.Contains(bufStr, "hello") {
			idx := strings.Index(bufStr, "hello") + len("hello") + 6
			if idx < len(bufStr) {
				version, err := strconv.Atoi(bufStr[idx : idx+1])
				if err != nil {
					logger.Error("Failed to convert protocol version to int", zap.Error(err))
					return fmt.Errorf("failed to convert protocol version: %w", err)
				}
				redisProtocolVersion = version
			} else {
				logger.Error("No protocol version found after hello")
				return fmt.Errorf("no protocol version found after hello")
			}
		}
	}
	logger.Debug("redisProtocolVersion", zap.Int("version", redisProtocolVersion))

	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)

	err = pUtil.ReadFromPeer(ctx, logger, clientConn, clientBuffChan, errChan, pUtil.Client)
	if err != nil {
		return fmt.Errorf("error reading from client:%v", err)
	}
	err = pUtil.ReadFromPeer(ctx, logger, destConn, destBuffChan, errChan, pUtil.Destination)
	if err != nil {
		return fmt.Errorf("error reading from destination:%v", err)
	}

	prevChunkWasReq := false
	reqTimestampMock := time.Now()
	var resTimestampMock time.Time

	for {
		select {
		case <-ctx.Done():
			if len(respAccumBuf) > 0 {
				processBufferResponses(respAccumBuf, models.FromServer, &redisResponses)
				// respAccumBuf = nil
			}
			if len(reqAccumBuf) > 0 {
				processBufferRequests(reqAccumBuf, models.FromClient, &redisRequests)
				// reqAccumBuf = nil
			}
			if len(redisRequests) > 0 && len(redisResponses) > 0 {
				metadata := map[string]string{"type": "config"}
				mocks <- &models.Mock{
					Version: models.GetVersion(),
					Name:    "mocks",
					Kind:    models.REDIS,
					Spec: models.MockSpec{
						RedisRequests:    redisRequests,
						RedisResponses:   redisResponses,
						ReqTimestampMock: reqTimestampMock,
						ResTimestampMock: resTimestampMock,
						Metadata:         metadata,
					},
				}
			}
			return ctx.Err()

		case buffer := <-clientBuffChan:
			if len(respAccumBuf) > 0 {
				processBufferResponses(respAccumBuf, models.FromServer, &redisResponses)
				respAccumBuf = nil
			}

			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			if !prevChunkWasReq && len(redisRequests) > 0 && len(redisResponses) > 0 {
				reqs := append([]models.RedisRequests(nil), redisRequests...)
				resps := append([]models.RedisResponses(nil), redisResponses...)
				go func(reqs []models.RedisRequests, resps []models.RedisResponses) {
					metadata := map[string]string{"type": "config"}
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
				}(reqs, resps)
				redisRequests = nil
				redisResponses = nil
			}

			reqAccumBuf = append(reqAccumBuf, buffer...)
			prevChunkWasReq = true

		case buffer := <-destBuffChan:
			if prevChunkWasReq {
				if len(reqAccumBuf) > 0 {
					processBufferRequests(reqAccumBuf, models.FromClient, &redisRequests)
					reqAccumBuf = nil
				}
				reqTimestampMock = time.Now()
				respAccumBuf = nil
			}

			respAccumBuf = append(respAccumBuf, buffer...)
			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				return err
			}
			resTimestampMock = time.Now()
			prevChunkWasReq = false

		case err := <-errChan:
			if err == io.EOF {
				if len(respAccumBuf) > 0 {
					processBufferResponses(respAccumBuf, models.FromServer, &redisResponses)
					// respAccumBuf = nil
				}
				if len(reqAccumBuf) > 0 {
					processBufferRequests(reqAccumBuf, models.FromClient, &redisRequests)
					// reqAccumBuf = nil
				}
				if len(redisRequests) > 0 && len(redisResponses) > 0 {
					saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
				}
				return nil
			}
			return err
		}
	}
}

func processBufferRequests(buffer []byte, origin models.OriginType, payloads *[]models.RedisRequests) {
	bodies, err := parseRedis(buffer)
	if err != nil {
		return
	}
	*payloads = append(*payloads, models.RedisRequests{Origin: origin, Message: bodies})
}

func processBufferResponses(buffer []byte, origin models.OriginType, payloads *[]models.RedisResponses) {
	bodies, err := parseRedis(buffer)
	if err != nil {
		return
	}
	*payloads = append(*payloads, models.RedisResponses{Origin: origin, Message: bodies})
}

func saveMock(requests []models.RedisRequests, responses []models.RedisResponses, reqTimestampMock, resTimestampMock time.Time, mocks chan<- *models.Mock) {
	redisRequestsCopy := make([]models.RedisRequests, len(requests))
	redisResponsesCopy := make([]models.RedisResponses, len(responses))
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
