package redisv2

import (
	"context"
	"errors"
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
	"golang.org/x/sync/errgroup"
)

func encodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte,
	clientConn, destConn net.Conn, mocks chan<- *models.Mock,
	_ models.OutgoingOptions) error {

	var redisProtocolVersion int
	var redisRequests []models.RedisRequests
	var redisResponses []models.RedisResponses

	// parse initial client request
	processBufferRequests(reqBuf, models.FromClient, &redisRequests)

	dataType := models.String
	if !util.IsASCII(string(reqBuf)) {
		bufStr := util.EncodeBase64(reqBuf)
		dataType = "binary"
		logger.Debug("Encoding redis request", zap.String("dataType", dataType), zap.String("bufStr", bufStr))
	} else {
		logger.Debug("Encoding redis request", zap.String("dataType", dataType), zap.ByteString("buf", reqBuf))
	}

	// determine protocol version
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

	// send to server
	if _, err := destConn.Write(reqBuf); err != nil {
		utils.LogError(logger, err, "failed to write request to destination server")
		return err
	}

	errCh := make(chan error, 1)
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get error group from context")
	}
	reqTimestamp := time.Now()

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		for {
			resp, err := pUtil.ReadBytes(ctx, logger, destConn)
			logger.Info("received response", zap.ByteString("resp", resp))
			if err != nil {
				if err == io.EOF {
					if len(resp) != 0 {
						ts := time.Now()
						clientConn.Write(resp)
						processBufferResponses(resp, models.FromServer, &redisResponses)
						saveMock(redisRequests, redisResponses, reqTimestamp, ts, mocks)
					}
					break
				}
				utils.LogError(logger, err, "failed reading response from server")
				errCh <- err
				return nil
			}

			clientConn.Write(resp)
			ts := time.Now()
			processBufferResponses(resp, models.FromServer, &redisResponses)
			if len(redisRequests) > 0 && len(redisResponses) > 0 {
				saveMock(redisRequests, redisResponses, reqTimestamp, ts, mocks)
				redisRequests = nil
				redisResponses = nil
			}

			// next client request
			reqBuf, err = pUtil.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed reading request from client")
					errCh <- err
					return nil
				}
				errCh <- err
				return nil
			}
			processBufferRequests(reqBuf, models.FromClient, &redisRequests)
			destConn.Write(reqBuf)
			reqTimestamp = time.Now()
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
