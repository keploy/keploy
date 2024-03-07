// Package generic provides functionality for decoding generic dependencies.
package generic

import (
	"context"
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
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return err
			}

			// To read the stream of request packets from the client
			for {
				buffer, err := pUtil.ReadBytes(ctx, clientConn)
				if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil && err.Error() != "EOF" {
					utils.LogError(logger, err, "failed to read the request message in proxy for generic dependency")
					return err
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

			// bestMatchedIndx := 0
			// fuzzy match gives the index for the best matched generic mock
			matched, genericResponses, err := fuzzymatch(ctx, genericRequests, mockDb)
			if err != nil {
				utils.LogError(logger, err, "error while matching generic mocks")
			}

			if !matched {
				err := clientConn.SetReadDeadline(time.Time{})
				if err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return err
				}

				logger.Debug("the genericRequests before pass through are", zap.Any("length", len(genericRequests)))
				for _, genReq := range genericRequests {
					logger.Debug("the genericRequests are:", zap.Any("h", string(genReq)))
				}

				// making destConn
				destConn, err := net.Dial("tcp", dstCfg.Addr)
				if err != nil {
					utils.LogError(logger, err, "failed to dial the destination server")
					return err
				}

				reqBuffer, err := pUtil.PassThrough(ctx, logger, clientConn, destConn, genericRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to passthrough the generic request")
					return err
				}

				genericRequests = [][]byte{}
				logger.Debug("the request buffer after pass through in generic", zap.Any("buffer", string(reqBuffer)))
				if len(reqBuffer) > 0 {
					genericRequests = [][]byte{reqBuffer}
				}
				logger.Debug("the length of genericRequests after passthrough ", zap.Any("length", len(genericRequests)))
				continue
			}
			for _, genericResponse := range genericResponses {
				encoded := []byte(genericResponse.Message[0].Data)
				if genericResponse.Message[0].Type != models.String {
					encoded, err = util.DecodeBase64(genericResponse.Message[0].Data)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the base64 response")
						return err
					}
				}
				_, err := clientConn.Write(encoded)
				if err != nil {
					utils.LogError(logger, err, "failed to write the response message to the client application")
					return err
				}
			}

			// Clear the genericRequests buffer for the next dependency call
			genericRequests = [][]byte{}
			logger.Debug("the genericRequests after the iteration", zap.Any("length", len(genericRequests)))
		}
	}
}
