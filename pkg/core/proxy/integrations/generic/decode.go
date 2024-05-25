// Package generic provides functionality for decoding generic dependencies.
package generic

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

func decodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	genericRequests := [][]byte{reqBuf}
	logger.Debug("Into the generic parser in test mode")
	errCh := make(chan error, 1)
	go func(errCh chan error, genericRequests [][]byte) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return
			}

			// To read the stream of request packets from the client
			for {
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil && err.Error() != "EOF" {
					utils.LogError(logger, err, "failed to read the request message in proxy for generic dependency")
					return
				}
				if netErr, ok := err.(net.Error); (ok && netErr.Timeout()) || (err != nil && err.Error() == "EOF") {
					logger.Debug("the timeout for the client read in generic or EOF")
					break
				}
				genericRequests = append(genericRequests, buffer)
			}

			if len(genericRequests) == 0 {
				logger.Debug("the generic request buffer is empty")
				continue
			}
			logger.Info("the generic rq no", zap.Any("length", len(genericRequests)))

			// bestMatchedIndx := 0
			// fuzzy match gives the index for the best matched generic mock
			matched, genericResponses, err := fuzzyMatch(ctx, genericRequests, mockDb, logger)
			if err != nil {
				utils.LogError(logger, err, "error while matching generic mocks")
			}

			if !matched {
				err := clientConn.SetReadDeadline(time.Time{})
				if err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return
				}

				logger.Debug("the genericRequests before pass through are", zap.Any("length", len(genericRequests)))
				for _, genReq := range genericRequests {
					logger.Debug("the genericRequests are:", zap.Any("h", string(genReq)))
				}
				// fmt.Println("passed through log", genericRequests)

				// for _, request := range genericRequests {
				// 	logger.Info(string(request))
				// }
				reqBuffer, err := pUtil.PassThrough(ctx, logger, clientConn, dstCfg, genericRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to passthrough the generic request")
					return
				}

				genericRequests = [][]byte{}
				logger.Debug("the request buffer after pass through in generic", zap.Any("buffer", string(reqBuffer)))
				if len(reqBuffer) > 0 {
					genericRequests = [][]byte{reqBuffer}
				}
				logger.Debug("the length of genericRequests after passThrough ", zap.Any("length", len(genericRequests)))
				continue
			} else {
				logger.Info("the generic requests matched length ", zap.Any("genericRequests", len(genericRequests)))

				for _, a := range genericRequests {
					logger.Info("the generic requests matched", zap.String("genericRequests", string(a)))
				}
			}
			for _, genericResponse := range genericResponses {
				encoded := []byte(genericResponse.Message[0].Data)
				if genericResponse.Message[0].Type != models.String {
					encoded, err = util.DecodeBase64(genericResponse.Message[0].Data)
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

			// Clear the genericRequests buffer for the next dependency call
			genericRequests = [][]byte{}
			logger.Debug("the genericRequests after the iteration", zap.Any("length", len(genericRequests)))
		}
	}(errCh, genericRequests)

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
