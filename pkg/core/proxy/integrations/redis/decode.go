package redis

import (
	"context"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func decodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	redisRequests := [][]byte{reqBuf}
	logger.Debug("Into the redis parser in test mode")
	errCh := make(chan error, 1)

	go func(errCh chan error, redisRequests [][]byte) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)
		for {

			// Read the stream of request packets from the client
			for {
				if len(redisRequests) > 0 {
					break
				}
				clientConn.SetReadDeadline(time.Now().Add(10 * time.Second))
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil && err.Error() != "EOF" {
					logger.Debug("failed to read the request message in proxy for redis dependency")
					return
				}
				if netErr, ok := err.(net.Error); (ok && netErr.Timeout()) || (err != nil && err.Error() == "EOF") {
					logger.Debug("timeout for client read in redis or EOF")
					break
				}
				if len(buffer) > 0 {
					redisRequests = append(redisRequests, buffer)
					break
				}
			}

			if len(redisRequests) == 0 {
				logger.Debug("redis request buffer is empty")
				continue
			}

			// Fuzzy match to get the best matched redis mock
			matched, redisResponses, err := fuzzyMatch(ctx, redisRequests, mockDb)
			if err != nil {
				utils.LogError(logger, err, "error while matching redis mocks")
			}

			if !matched {
				err := clientConn.SetReadDeadline(time.Time{})
				if err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return
				}

				logger.Debug("redisRequests before pass through:", zap.Any("length", len(redisRequests)))
				for _, redReq := range redisRequests {
					logger.Debug("redisRequests:", zap.Any("h", string(redReq)))
				}

				reqBuffer, err := pUtil.PassThrough(ctx, logger, clientConn, dstCfg, redisRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to passthrough the redis request")
					return
				}

				redisRequests = [][]byte{}
				logger.Debug("request buffer after pass through in redis:", zap.Any("buffer", string(reqBuffer)))
				if len(reqBuffer) > 0 {
					redisRequests = [][]byte{reqBuffer}
				}
				logger.Debug("length of redisRequests after passThrough:", zap.Any("length", len(redisRequests)))
				continue
			}
			for _, redisResponse := range redisResponses {
				encoded := []byte(redisResponse.Message[0].Data)
				if redisResponse.Message[0].Type != models.String {
					encoded, err = util.DecodeBase64(redisResponse.Message[0].Data)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the base64 response")
						return
					}
				}
				_, err := clientConn.Write(encoded)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(logger, err, "failed to write the response message to the client application")
					return
				}
			}

			// Clear the redisRequests buffer for the next dependency call
			redisRequests = [][]byte{}
			logger.Debug("redisRequests after the iteration:", zap.Any("length", len(redisRequests)))
		}
	}(errCh, redisRequests)

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
