package mongo

import (
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"io"
	"math/rand"
	"net"
	"time"
)

func encodeMongo(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	rand.Seed(time.Now().UnixNano())
	for {

		var err error
		var readRequestDelay time.Duration
		// var logStr string = fmt.Sprintln("the conn id: ", clientConnId, " the destination conn id: ", destConnId)

		// logStr += fmt.Sprintln("started reading from the client: ", started)
		if string(requestBuffer) == "read form client conn" {
			// lstr := ""
			started := time.Now()
			requestBuffer, err = util.ReadBytes(clientConn)
			logger.Debug("reading from the mongo conn", zap.Any("", string(requestBuffer)))
			if err != nil {
				if !h.IsUserAppTerminateInitiated() {
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in record mode for mongo call")
						return
					}
					logger.Error("failed to read request from the mongo client", zap.Error(err), zap.String("mongo client address", clientConn.RemoteAddr().String()))
					return
				}
			}
			readRequestDelay = time.Since(started)
			// logStr += lstr
			logger.Debug(fmt.Sprintf("the request in the mongo parser before passing to dest: %v", len(requestBuffer)))
		}

		var (
			mongoRequests  = []models.MongoRequest{}
			mongoResponses = []models.MongoResponse{}
		)
		opReq, requestHeader, mongoRequest, err := Decode(requestBuffer, logger)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the client", zap.Error(err))
			return
		}
		mongoRequests = append(mongoRequests, models.MongoRequest{
			Header:    &requestHeader,
			Message:   mongoRequest,
			ReadDelay: int64(readRequestDelay),
		})
		_, err = destConn.Write(requestBuffer)
		if err != nil {
			logger.Error("failed to write the request buffer to mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
			return
		}
		logger.Debug(fmt.Sprintf("the request in the mongo parser after passing to dest: %v", len(requestBuffer)))

		// logStr += fmt.Sprintln("after writing the request to the destination: ", time.Since(started))
		if val, ok := mongoRequest.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
			for {
				requestBuffer1, err := util.ReadBytes(clientConn)

				// logStr += tmpStr
				if err != nil {
					if !h.IsUserAppTerminateInitiated() {
						if err == io.EOF {
							logger.Debug("recieved request buffer is empty in record mode for mongo request")
							return
						}
						logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
						return
					}
				}

				// write the reply to mongo client
				_, err = destConn.Write(requestBuffer1)
				if err != nil {
					// fmt.Println(logStr)
					logger.Error("failed to write the reply message to mongo client", zap.Error(err))
					return
				}

				// logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

				if len(requestBuffer1) == 0 {
					logger.Debug("the response from the server is complete")
					break
				}
				_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
				if err != nil {
					logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
					return
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
		responsePckLengthBuffer, err := util.ReadRequiredBytes(destConn, 4)
		if err != nil {
			if err == io.EOF {
				logger.Debug("recieved response buffer is empty in record mode for mongo call")
				destConn.Close()
				return
			}
			logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
			return
		}

		logger.Debug("recieved these pck length packets", zap.Any("packets", responsePckLengthBuffer))

		pckLength := getPacketLength(responsePckLengthBuffer)
		logger.Debug("recieved pck length ", zap.Any("packet length", pckLength))

		responsePckDataBuffer, err := util.ReadRequiredBytes(destConn, int(pckLength)-4)

		logger.Debug("recieved these packets", zap.Any("packets", responsePckDataBuffer))

		responseBuffer := append(responsePckLengthBuffer, responsePckDataBuffer...)
		logger.Debug("reading from the destination mongo server", zap.Any("", string(responseBuffer)))
		// logStr += tmpStr
		if err != nil {
			if err == io.EOF {
				logger.Debug("recieved response buffer is empty in record mode for mongo call")
				destConn.Close()
				return
			}
			logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
			return
		}
		readResponseDelay := time.Since(started)

		// write the reply to mongo client
		_, err = clientConn.Write(responseBuffer)
		if err != nil {
			logger.Error("failed to write the reply message to mongo client", zap.Error(err))
			return
		}

		// logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

		_, responseHeader, mongoResponse, err := Decode(responseBuffer, logger)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
			return
		}
		mongoResponses = append(mongoResponses, models.MongoResponse{
			Header:    &responseHeader,
			Message:   mongoResponse,
			ReadDelay: int64(readResponseDelay),
		})
		if val, ok := mongoResponse.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
			for i := 0; ; i++ {
				if i == 0 && isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message, logger) {
					go func() {
						// Recover from panic and gracefully shutdown
						defer h.Recover(pkg.GenerateRandomID())
						defer utils.HandlePanic()
						recordMessage(h, mongoRequests, mongoResponses, opReq, ctx, reqTimestampMock, logger)
					}()
				}
				started = time.Now()
				responseBuffer, err = util.ReadBytes(destConn)
				// logStr += tmpStr
				if err != nil {
					if err == io.EOF {
						logger.Debug("recieved response buffer is empty in record mode for mongo call")
						destConn.Close()
						return
					}
					logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
					return
				}
				logger.Debug(fmt.Sprintf("the response in the mongo parser before passing to client: %v", len(responseBuffer)))

				readResponseDelay := time.Since(started)

				// write the reply to mongo client
				_, err = clientConn.Write(responseBuffer)
				if err != nil {
					// fmt.Println(logStr)
					logger.Error("failed to write the reply message to mongo client", zap.Error(err))
					return
				}
				logger.Debug(fmt.Sprintf("the response in the mongo parser after passing to client: %v", len(responseBuffer)))

				// logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

				if len(responseBuffer) == 0 {
					logger.Debug("the response from the server is complete")
					break
				}
				_, respHeader, mongoResp, err := Decode(responseBuffer, logger)
				if err != nil {
					logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
					return
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

		go func() {
			recordMessage(h, mongoRequests, mongoResponses, opReq, ctx, reqTimestampMock, logger)
		}()
		requestBuffer = []byte("read form client conn")

	}

}

func getPacketLength(src []byte) (length int32) {
	length = (int32(src[0]) | int32(src[1])<<8 | int32(src[2])<<16 | int32(src[3])<<24)
	return length
}
