// Package v1 provides functionality for decoding Postgres requests and responses.
package v1

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func decodePostgres(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	pgRequests := [][]byte{reqBuf}
	errCh := make(chan error, 1)

	go func(errCh chan error, pgRequests [][]byte) {
		defer pUtil.Recover(logger, clientConn, nil)
		// close should be called from the producer of the channel
		defer close(errCh)
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the pg client conn")
				errCh <- err
			}

			// To read the stream of request packets from the client
			for {
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) {
						if err == io.EOF {
							logger.Debug("EOF error received from client. Closing conn in postgres !!")
							errCh <- err
						}
						logger.Debug("failed to read the request message in proxy for postgres dependency")
						errCh <- err
					}
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					logger.Debug("the timeout for the client read in pg")
					break
				}
				pgRequests = append(pgRequests, buffer)
			}

			if len(pgRequests) == 0 {
				logger.Debug("the postgres request buffer is empty")
				continue
			}
			var mutex sync.Mutex
			matched, pgResponses, err := matchingReadablePG(ctx, logger, &mutex, pgRequests, mockDb)
			if err != nil {
				errCh <- fmt.Errorf("error while matching tcs mocks %v", err)
				return
			}

			if !matched {
				logger.Debug("MISMATCHED REQ is" + string(pgRequests[0]))
				_, err = pUtil.PassThrough(ctx, logger, clientConn, dstCfg, pgRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to pass the request", zap.Any("request packets", len(pgRequests)))
					errCh <- err
				}
				continue
			}
			for _, pgResponse := range pgResponses {
				encoded, err := util.DecodeBase64(pgResponse.Payload)
				if len(pgResponse.PacketTypes) > 0 && len(pgResponse.Payload) == 0 {
					encoded, err = postgresDecoderFrontend(pgResponse)
				}
				if err != nil {
					utils.LogError(logger, err, "failed to decode the response message in proxy for postgres dependency")
					errCh <- err
				}
				_, err = clientConn.Write(encoded)
				if err != nil {
					utils.LogError(logger, err, "failed to write the response message to the client application")
					errCh <- err
				}
			}
			// Clear the buffer for the next dependency call
			pgRequests = [][]byte{}
		}
	}(errCh, pgRequests)

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

type QueryData struct {
	PrepIdentifier string `json:"PrepIdentifier" yaml:"PrepIdentifier"`
	Query          string `json:"Query" yaml:"Query"`
}

type PrepMap map[string][]QueryData

type TestPrepMap map[string][]QueryData

func getRecordPrepStatement(allMocks []*models.Mock) PrepMap {
	preparedstatement := make(PrepMap)
	for _, v := range allMocks {
		if v.Kind != "Postgres" {
			continue
		}
		for _, req := range v.Spec.PostgresRequests {
			var querydata []QueryData
			psMap := make(map[string]string)
			if len(req.PacketTypes) > 0 && req.PacketTypes[0] != "p" && req.Identfier != "StartupRequest" {
				p := 0
				for _, header := range req.PacketTypes {
					if header == "P" {
						if strings.Contains(req.Parses[p].Name, "S_") {
							psMap[req.Parses[p].Query] = req.Parses[p].Name
							querydata = append(querydata, QueryData{PrepIdentifier: req.Parses[p].Name,
								Query: req.Parses[p].Query,
							})

						}
						p++
					}
				}
			}
			// also append the query data for the prepared statement
			if len(querydata) > 0 {
				preparedstatement[v.ConnectionID] = append(preparedstatement[v.ConnectionID], querydata...)
			}
		}

	}
	return preparedstatement
}
