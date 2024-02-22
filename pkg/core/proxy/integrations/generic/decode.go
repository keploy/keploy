package generic

import (
	"context"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"net"
	"time"
)

func decodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	genericRequests := [][]byte{reqBuf}
	logger.Debug("into the generic parser in test mode")
	for {
		// Since protocol packets have to be parsed for checking stream end,
		// clientConnection have deadline for read to determine the end of stream.
		err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if err != nil {
			logger.Error("failed to set the read deadline for the client conn", zap.Error(err))
			return err
		}

		for {
			buffer, err := util.ReadBytes(ctx, clientConn)
			if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil && err.Error() != "EOF" {
				logger.Error("failed to read the request message in proxy for generic dependency", zap.Error(err))
				// errChannel <- err
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
		matched, genericResponses, err := fuzzymatch(ctx, genericRequests, h)
		if err != nil {
			logger.Error("error while fuzzy matching", zap.Error(err))
		}

		if !matched {
			// log.Error("failed to match the dependency call from user application", zap.Any("request packets", len(genericRequests)))
			clientConn.SetReadDeadline(time.Time{})
			logger.Debug("the genericRequests are before pass through", zap.Any("length", len(genericRequests)))
			for _, vgen := range genericRequests {
				logger.Debug("the genericRequests are:", zap.Any("h", string(vgen)))
			}
			requestBuffer, err = util.Passthrough(clientConn, destConn, genericRequests, h.Recover, logger)
			// if err != nil {
			// 	return err
			// }
			genericRequests = [][]byte{}
			logger.Debug("the request buffer after pass through in generic", zap.Any("buffer", string(requestBuffer)))
			if len(requestBuffer) > 0 {
				genericRequests = [][]byte{requestBuffer}
			}
			logger.Debug("the length of genericRequests after passthrough ", zap.Any("length", len(genericRequests)))
			continue
			// return errors.New("failed to match the dependency call from user application")
			// continue
		}
		for _, genericResponse := range genericResponses {
			encoded := []byte(genericResponse.Message[0].Data)
			if genericResponse.Message[0].Type != models.String {
				encoded, _ = PostgresDecoder(genericResponse.Message[0].Data)
			}
			_, err := clientConn.Write([]byte(encoded))
			if err != nil {
				logger.Error("failed to write request message to the client application", zap.Error(err))
				// errChannel <- err
				return err
			}
		}
		// }

		// tools for the next dependency call
		genericRequests = [][]byte{}
		logger.Debug("the genericRequests after the iteration", zap.Any("length", len(genericRequests)))
	}
}
