package mongo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func (m *Mongo) encodeMongo(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	errCh := make(chan error, 1)

	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)
		for {
			var err error
			var readRequestDelay time.Duration
			// var logStr string = fmt.Sprintln("the conn id: ", clientConnId, " the destination conn id: ", destConnId)

			// logStr += fmt.Sprintln("started reading from the client: ", started)
			if string(reqBuf) == "read form client conn" {
				// lstr := ""
				started := time.Now()
				reqBuf, err = pUtil.ReadBytes(ctx, logger, clientConn)
				logger.Debug("reading from the mongo conn", zap.Any("", string(reqBuf)))
				if err != nil {
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in record mode for mongo call")
						errCh <- err
						return nil
					}
					utils.LogError(logger, err, "failed to read request from the mongo client", zap.String("mongo client address", clientConn.RemoteAddr().String()))
					errCh <- err
					return nil
				}
				readRequestDelay = time.Since(started)
				// logStr += lstr
				logger.Debug(fmt.Sprintf("the request in the mongo parser before passing to dest: %v", len(reqBuf)))
			}

			var (
				mongoRequests  []models.MongoRequest
				mongoResponses []models.MongoResponse
			)
			opReq, requestHeader, mongoRequest, err := Decode(reqBuf, logger)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the mongo wire message from the client")
				errCh <- err
				return nil
			}

			mongoRequests = append(mongoRequests, models.MongoRequest{
				Header:    &requestHeader,
				Message:   mongoRequest,
				ReadDelay: int64(readRequestDelay),
			})
			_, err = destConn.Write(reqBuf)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write the request buffer to mongo server", zap.String("mongo server address", destConn.RemoteAddr().String()))
				errCh <- err
				return nil
			}
			logger.Debug(fmt.Sprintf("the request in the mongo parser after passing to dest: %v", len(reqBuf)))

			// logStr += fmt.Sprintln("after writing the request to the destination: ", time.Since(started))
			if val, ok := mongoRequest.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
				for {
					requestBuffer1, err := pUtil.ReadBytes(ctx, logger, clientConn)

					// logStr += tmpStr
					if err != nil {
						if err == io.EOF {
							logger.Debug("recieved request buffer is empty in record mode for mongo request")
							errCh <- err
							return nil
						}
						utils.LogError(logger, err, "failed to read request from the mongo client", zap.String("mongo client address", clientConn.RemoteAddr().String()))
						errCh <- err
						return nil
					}

					// write the reply to mongo client
					_, err = destConn.Write(requestBuffer1)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write the reply message to mongo client")
						errCh <- err
						return nil
					}

					// logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

					if len(requestBuffer1) == 0 {
						logger.Debug("the response from the server is complete")
						break
					}
					_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the mongo wire message from the destination server")
						errCh <- err
						return nil
					}
					if mongoReqVal, ok := mongoReq.(models.MongoOpMessage); ok && !hasSecondSetBit(mongoReqVal.FlagBits) {
						logger.Debug("the request from the client is complete since the more_to_come flagbit is 0")
						break
					}
					mongoRequests = append(mongoRequests, models.MongoRequest{
						Header:    &reqHeader,
						Message:   mongoReq,
						ReadDelay: int64(readRequestDelay),
					})
				}
			}

			// read reply message from the mongo server
			// tmpStr := ""
			reqTimestampMock := time.Now()
			started := time.Now()
			responsePckLengthBuffer, err := pUtil.ReadRequiredBytes(ctx, logger, destConn, 4)
			if err != nil {
				if err == io.EOF {
					logger.Debug("recieved response buffer is empty in record mode for mongo call")
					errCh <- err
					return nil
				}
				utils.LogError(logger, err, "failed to read reply from the mongo server", zap.String("mongo server address", destConn.RemoteAddr().String()))
				errCh <- err
				return nil
			}

			logger.Debug("recieved these pck length packets", zap.Any("packets", responsePckLengthBuffer))

			pckLength := getPacketLength(responsePckLengthBuffer)
			logger.Debug("received pck length ", zap.Any("packet length", pckLength))

			responsePckDataBuffer, err := pUtil.ReadRequiredBytes(ctx, logger, destConn, int(pckLength)-4)

			logger.Debug("recieved these packets", zap.Any("packets", responsePckDataBuffer))

			responseBuffer := append(responsePckLengthBuffer, responsePckDataBuffer...)
			logger.Debug("reading from the destination mongo server", zap.Any("", string(responseBuffer)))
			// logStr += tmpStr
			if err != nil {
				if err == io.EOF {
					logger.Debug("recieved response buffer is empty in record mode for mongo call")
					errCh <- err
					return nil
				}
				utils.LogError(logger, err, "failed to read reply from the mongo server", zap.String("mongo server address", destConn.RemoteAddr().String()))
				errCh <- err
				return nil
			}
			readResponseDelay := time.Since(started)

			// write the reply to mongo client
			_, err = clientConn.Write(responseBuffer)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write the reply message to mongo client")
				errCh <- err
				return nil
			}

			// logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

			_, responseHeader, mongoResponse, err := Decode(responseBuffer, logger)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the mongo wire message from the destination server")
				errCh <- err
				return nil
			}
			mongoResponses = append(mongoResponses, models.MongoResponse{
				Header:    &responseHeader,
				Message:   mongoResponse,
				ReadDelay: int64(readResponseDelay),
			})
			if val, ok := mongoResponse.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
				for i := 0; ; i++ {
					if i == 0 && isHeartBeat(logger, opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
						m.recordMessage(ctx, logger, mongoRequests, mongoResponses, opReq, reqTimestampMock, mocks)
					}
					started = time.Now()
					responseBuffer, err = pUtil.ReadBytes(ctx, logger, destConn)
					// logStr += tmpStr
					if err != nil {
						if err == io.EOF {
							logger.Debug("recieved response buffer is empty in record mode for mongo call")
							errCh <- err
							return nil
						}
						utils.LogError(logger, err, "failed to read reply from the mongo server", zap.String("mongo server address", destConn.RemoteAddr().String()))
						errCh <- err
						return nil
					}
					logger.Debug(fmt.Sprintf("the response in the mongo parser before passing to client: %v", len(responseBuffer)))

					readResponseDelay := time.Since(started)

					// write the reply to mongo client
					_, err = clientConn.Write(responseBuffer)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write the reply message to mongo client")
						errCh <- err
						return nil
					}
					logger.Debug(fmt.Sprintf("the response in the mongo parser after passing to client: %v", len(responseBuffer)))

					// logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

					if len(responseBuffer) == 0 {
						logger.Debug("the response from the server is complete")
						break
					}
					_, respHeader, mongoResp, err := Decode(responseBuffer, logger)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the mongo wire message from the destination server")
						errCh <- err
						return nil
					}
					if mongoRespVal, ok := mongoResp.(models.MongoOpMessage); ok && !hasSecondSetBit(mongoRespVal.FlagBits) {
						logger.Debug("the response from the server is complete since the more_to_come flagbit is 0")
						break
					}
					mongoResponses = append(mongoResponses, models.MongoResponse{
						Header:    &respHeader,
						Message:   mongoResp,
						ReadDelay: int64(readResponseDelay),
					})
				}
			}

			m.recordMessage(ctx, logger, mongoRequests, mongoResponses, opReq, reqTimestampMock, mocks)
			reqBuf = []byte("read form client conn")
		}
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

func getPacketLength(src []byte) (length int32) {
	length = int32(src[0]) | int32(src[1])<<8 | int32(src[2])<<16 | int32(src[3])<<24
	return length
}
