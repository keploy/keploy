package mongo

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/hooks"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/proxy/util"
	"go.keploy.io/server/v2/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"
var configRequests = []string{""}
var password string

type MongoParser struct {
	logger *zap.Logger
	hooks  *hooks.Hook
}

func NewMongoParser(logger *zap.Logger, h *hooks.Hook, authPassword string) *MongoParser {
	password = authPassword
	return &MongoParser{
		logger: logger,
		hooks:  h,
	}
}

// IsOutgoingMongo function determines if the outgoing network call is Mongo by comparing the
// message format with that of a mongo wire message.
func (m *MongoParser) OutgoingType(buffer []byte) bool {
	if len(buffer) < 4 {
		return false
	}
	messageLength := binary.LittleEndian.Uint32(buffer[0:4])
	return int(messageLength) == len(buffer)
}

func (m *MongoParser) ProcessOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, ctx context.Context) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		m.logger.Debug("the outgoing mongo in record mode")
		encodeOutgoingMongo(requestBuffer, clientConn, destConn, m.hooks, m.logger, ctx)
	case models.MODE_TEST:
		logger := m.logger.With(zap.Any("Client IP Address", clientConn.RemoteAddr().String()), zap.Any("Client ConnectionID", util.GetNextID()), zap.Any("Destination ConnectionID", util.GetNextID()))
		m.logger.Debug("the outgoing mongo in test mode")
		decodeOutgoingMongo(requestBuffer, clientConn, destConn, m.hooks, logger)
	default:
	}
}

func decodeOutgoingMongo(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	startedDecoding := time.Now()
	requestBuffers := [][]byte{requestBuffer}
	var readRequestDelay time.Duration
	for {
		configMocks, err := h.GetConfigMocks()
		if err != nil {
			logger.Error("error while getting config mock", zap.Error(err))
		}
		logger.Debug(fmt.Sprintf("the config mocks are: %v", configMocks))

		var (
			mongoRequests = []models.MongoRequest{}
		)
		if string(requestBuffer) == "read form client conn" {
			started := time.Now()
			requestBuffer, err = util.ReadBytes(clientConn)
			if err != nil {
				if err == io.EOF {
					logger.Debug("recieved request buffer is empty in test mode for mongo calls")
					return
				}
				logger.Error("failed to read request from the mongo client", zap.Error(err))
				return
			}
			requestBuffers = append(requestBuffers, requestBuffer)
			logger.Debug("the request from the mongo client", zap.Any("buffer", requestBuffer))
			readRequestDelay = time.Since(started)
		}
		if len(requestBuffer) == 0 {
			return
		}
		logger.Debug(fmt.Sprintf("the loop starts with the time delay: %v", time.Since(startedDecoding)))
		opReq, requestHeader, mongoRequest, err := Decode((requestBuffer), logger)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the client", zap.Error(err))
			return
		}
		mongoRequests = append(mongoRequests, models.MongoRequest{
			Header:    &requestHeader,
			Message:   mongoRequest,
			ReadDelay: int64(readRequestDelay),
		})
		if val, ok := mongoRequest.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
			for {
				started := time.Now()
				logger.Debug("into the for loop for request stream")
				requestBuffer1, err := util.ReadBytes(clientConn)
				if err != nil {
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty for streaming mongo request call")
						return
					}
					logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
					return
				}
				requestBuffers = append(requestBuffers, requestBuffer)
				readRequestDelay = time.Since(started)

				if len(requestBuffer1) == 0 {
					logger.Debug("the response from the server is complete")
					break
				}
				_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
				if err != nil {
					logger.Error("failed to decode the mongo wire message from the mongo client", zap.Error(err))
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
		if isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message, logger) {
			logger.Debug("recieved a heartbeat request for mongo", zap.Any("config mocks", len(configMocks)))
			maxMatchScore := 0.0
			bestMatchIndex := -1
			for configIndex, configMock := range configMocks {
				logger.Debug("the config mock is: ", zap.Any("config mock", configMock), zap.Any("actual request", mongoRequests))
				if len(configMock.Spec.MongoRequests) == len(mongoRequests) {
					for i, req := range configMock.Spec.MongoRequests {
						if len(configMock.Spec.MongoRequests) != len(mongoRequests) || req.Header.Opcode != mongoRequests[i].Header.Opcode {
							continue
						}
						switch req.Header.Opcode {
						case wiremessage.OpQuery:
							expectedQuery := req.Message.(*models.MongoOpQuery)
							actualQuery := mongoRequests[i].Message.(*models.MongoOpQuery)
							if actualQuery.FullCollectionName != expectedQuery.FullCollectionName ||
								actualQuery.ReturnFieldsSelector != expectedQuery.ReturnFieldsSelector ||
								actualQuery.Flags != expectedQuery.Flags ||
								actualQuery.NumberToReturn != expectedQuery.NumberToReturn ||
								actualQuery.NumberToSkip != expectedQuery.NumberToSkip {
								continue
							}

							expected := map[string]interface{}{}
							actual := map[string]interface{}{}
							err = bson.UnmarshalExtJSON([]byte(expectedQuery.Query), true, &expected)
							if err != nil {
								logger.Error("failed to unmarshal the section of recorded request to bson document", zap.Error(err))
								continue
							}
							err = bson.UnmarshalExtJSON([]byte(actualQuery.Query), true, &actual)
							if err != nil {
								logger.Error("failed to unmarshal the section of incoming request to bson document", zap.Error(err))
								continue
							}
							score := calculateMatchingScore(expected, actual)
							logger.Debug("the expected and actual msg in the heartbeat OpQuery query.", zap.Any("expected", expected), zap.Any("actual", actual), zap.Any("score", score))
							if score > maxMatchScore {
								maxMatchScore = score
								bestMatchIndex = configIndex
							}

						case wiremessage.OpMsg:
							if req.Message.(*models.MongoOpMessage).FlagBits != mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits {
								continue
							}
							scoreSum := 0.0
							if len(req.Message.(*models.MongoOpMessage).Sections) != len(mongoRequests[i].Message.(*models.MongoOpMessage).Sections) {
								continue
							}
							for sectionIndx, section := range req.Message.(*models.MongoOpMessage).Sections {
								if len(req.Message.(*models.MongoOpMessage).Sections) == len(mongoRequests[i].Message.(*models.MongoOpMessage).Sections) {
									score := compareOpMsgSection(section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx], logger)
									scoreSum += score
								}
							}
							currentScore := scoreSum / float64(len(mongoRequests))
							logger.Debug("the expected and actual msg in the heartbeat OpMsg single section.", zap.Any("expected", req.Message.(*models.MongoOpMessage).Sections), zap.Any("actual", mongoRequests[i].Message.(*models.MongoOpMessage).Sections), zap.Any("score", currentScore))
							if currentScore > maxMatchScore {
								maxMatchScore = currentScore
								bestMatchIndex = configIndex
							}
						default:
							logger.Error("the OpCode of the mongo wiremessage is invalid.")
						}
					}
				}
			}
			responseTo := mongoRequests[0].Header.RequestID
			if bestMatchIndex == -1 || maxMatchScore == 0.0 {
				logger.Debug("the mongo request do not matches with any config mocks", zap.Any("request", mongoRequests))
				continue
			}
			for _, mongoResponse := range configMocks[bestMatchIndex].Spec.MongoResponses {
				switch mongoResponse.Header.Opcode {
				case wiremessage.OpReply:
					replySpec := mongoResponse.Message.(*models.MongoOpReply)
					replyMessage, err := encodeOpReply(replySpec, logger)
					if err != nil {
						logger.Error("failed to encode the recorded OpReply yaml", zap.Error(err), zap.Any("for request with id", responseTo))
						return
					}
					requestId := wiremessage.NextRequestID()
					heathCheckReplyBuffer := replyMessage.Encode(responseTo, requestId)
					responseTo = requestId
					logger.Debug(fmt.Sprintf("the bufffer response is: %v", string(heathCheckReplyBuffer)))
					_, err = clientConn.Write(heathCheckReplyBuffer)
					if err != nil {
						logger.Error("failed to write the health check reply to mongo client", zap.Error(err))
						return
					}
				case wiremessage.OpMsg:
					respMessage := mongoResponse.Message.(*models.MongoOpMessage)

					expectedRequestSections := []string{}
					if len(configMocks[bestMatchIndex].Spec.MongoRequests) > 0 {
						expectedRequestSections = configMocks[bestMatchIndex].Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections
					}
					message, err := encodeOpMsg(respMessage, mongoRequest.(*models.MongoOpMessage).Sections, expectedRequestSections, logger)
					if err != nil {
						logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", responseTo))
						return
					}
					if hasSecondSetBit(respMessage.FlagBits) {
						// the first response wiremessage have
						for {
							time.Sleep(time.Duration(mongoResponse.ReadDelay))
							// generate requestId for the mongo wiremessage
							requestId := wiremessage.NextRequestID()
							_, err := clientConn.Write(message.Encode(responseTo, requestId))
							logger.Debug("the response lifecycle ended.")
							if err != nil {
								logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
								return
							}
							// the 'responseTo' field of response wiremessage is set to requestId of currently sent wiremessage
							responseTo = requestId
						}
					} else {
						_, err := clientConn.Write(message.Encode(responseTo, wiremessage.NextRequestID()))
						if err != nil {
							logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
							return
						}
					}

				}
			}
		} else {

			isMatched, matchedMock, err := match(h, mongoRequests, logger)
			if err != nil {
				logger.Error("failed to match the mongo request with recorded tcsMocks", zap.Error(err))
			}
			if !isMatched {
				logger.Debug("mongo request not matched with any tcsMocks", zap.Any("request", mongoRequests))
				requestBuffer, err = util.Passthrough(clientConn, destConn, requestBuffers, h.Recover, logger)
				if err != nil {
					logger.Error("failed to passthrough the mongo request to the actual database server", zap.Error(err))
					return
				}
				continue
			}

			responseTo := mongoRequests[0].Header.RequestID
			logger.Debug("the mock matched with the current request", zap.Any("mock", matchedMock), zap.Any("responseTo", responseTo))

			for _, resp := range matchedMock.Spec.MongoResponses {
				respMessage := resp.Message.(*models.MongoOpMessage)
				expectedRequestSections := []string{}
				if len(matchedMock.Spec.MongoRequests) > 0 {
					expectedRequestSections = matchedMock.Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections
				}
				message, err := encodeOpMsg(respMessage, mongoRequest.(*models.MongoOpMessage).Sections, expectedRequestSections, logger)
				if err != nil {
					logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", responseTo))
					return
				}
				requestId := wiremessage.NextRequestID()
				_, err = clientConn.Write(message.Encode(responseTo, requestId))
				if err != nil {
					logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err), zap.Any("for request with id", responseTo))
					return
				}
				responseTo = requestId
			}
		}
		logger.Debug("the length of the requestBuffer after matching: " + strconv.Itoa(len(requestBuffer)) + strconv.Itoa(len(requestBuffers[0])))
		if len(requestBuffers) > 0 && len(requestBuffer) == len(requestBuffers[0]) {
			requestBuffer = []byte("read form client conn")
		}
		requestBuffers = [][]byte{}
	}
}

func GetPacketLength(src []byte) (length int32) {
	length = (int32(src[0]) | int32(src[1])<<8 | int32(src[2])<<16 | int32(src[3])<<24)
	return length
}

func encodeOutgoingMongo(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) {
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

		pckLength := GetPacketLength(responsePckLengthBuffer)
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
						recordMessage(h, requestBuffer, responseBuffer, mongoRequests, mongoResponses, opReq, ctx, reqTimestampMock, logger)
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
			// Recover from panic and gracefully shutdown
			defer h.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			recordMessage(h, requestBuffer, responseBuffer, mongoRequests, mongoResponses, opReq, ctx, reqTimestampMock, logger)
		}()
		requestBuffer = []byte("read form client conn")

	}

}

func recordMessage(h *hooks.Hook, requestBuffer, responseBuffer []byte, mongoRequests []models.MongoRequest, mongoResponses []models.MongoResponse, opReq Operation, ctx context.Context, reqTimestampMock time.Time, logger *zap.Logger) {
	// // capture if the wiremessage is a mongo operation call

	shouldRecordCalls := true
	name := "mocks"
	meta1 := map[string]string{
		"operation": opReq.String(),
	}

	// Skip heartbeat from capturing in the global set of mocks. Since, the heartbeat packet always contain the "hello" boolean.
	// See: https://github.com/mongodb/mongo-go-driver/blob/8489898c64a2d8c2e2160006eb851a11a9db9e9d/x/mongo/driver/operation/hello.go#L503
	if isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message, logger) {
		meta1["type"] = "config"
		for _, v := range configRequests {
			for _, req := range mongoRequests {

				switch req.Header.Opcode {
				case wiremessage.OpQuery:
					if req.Message.(*models.MongoOpQuery).Query == v {
						shouldRecordCalls = false
						break
					}
					configRequests = append(configRequests, req.Message.(*models.MongoOpQuery).Query)
				case wiremessage.OpMsg:
					if len(req.Message.(*models.MongoOpMessage).Sections) > 0 && req.Message.(*models.MongoOpMessage).Sections[0] == v {
						shouldRecordCalls = false
						break
					}
					configRequests = append(configRequests, req.Message.(*models.MongoOpMessage).Sections[0])
				default:
					if opReq.String() == v {
						shouldRecordCalls = false
						break
					}
					configRequests = append(configRequests, opReq.String())

				}
			}
		}
	}
	if shouldRecordCalls {
		mongoMock := &models.Mock{
			Version: models.GetVersion(),
			Kind:    models.Mongo,
			Name:    name,
			Spec: models.MockSpec{
				Metadata:         meta1,
				MongoRequests:    mongoRequests,
				MongoResponses:   mongoResponses,
				Created:          time.Now().Unix(),
				ReqTimestampMock: reqTimestampMock,
				ResTimestampMock: time.Now(),
			},
		}
		h.AppendMocks(mongoMock, ctx)
	}
}

func hasSecondSetBit(num int) bool {
	// Shift the number right by 1 bit and check if the least significant bit is set
	return (num>>1)&1 == 1
}

// Skip heartbeat from capturing in the global set of mocks. Since, the heartbeat packet always contain the "hello" boolean.
// See: https://github.com/mongodb/mongo-go-driver/blob/8489898c64a2d8c2e2160006eb851a11a9db9e9d/x/mongo/driver/operation/hello.go#L503
func isHeartBeat(opReq Operation, requestHeader models.MongoHeader, mongoRequest interface{}, logger *zap.Logger) bool {

	switch requestHeader.Opcode {
	case wiremessage.OpQuery:
		val, ok := mongoRequest.(*models.MongoOpQuery)
		if ok {
			return val.FullCollectionName == "admin.$cmd" && opReq.IsIsMaster() && strings.Contains(opReq.String(), "helloOk")
		}
	case wiremessage.OpMsg:
		_, ok := mongoRequest.(*models.MongoOpMessage)
		if ok {
			return (opReq.IsIsAdminDB() && strings.Contains(opReq.String(), "hello")) ||
				opReq.IsIsMaster() ||
				isScramAuthRequest(mongoRequest.(*models.MongoOpMessage).Sections, logger)
		}
	default:
		return false
	}
	return false
}

func compareOpMsgSection(expectedSection, actualSection string, logger *zap.Logger) float64 {
	// check that the sections are of same type. SectionSingle (section[16] is "m") or SectionSequence (section[16] is "i").
	if (len(expectedSection) < 16 || len(actualSection) < 16) && expectedSection[16] != actualSection[16] {
		return 0
	}
	logger.Debug(fmt.Sprintf("the sections are. Expected: %v\n and actual: %v", expectedSection, actualSection))
	switch {
	case strings.HasPrefix(expectedSection, "{ SectionSingle identifier:"):
		var expectedIdentifier string
		var expectedMsgsStr string
		// // Define the regular expression pattern
		// // Compile the regular expression
		// // Find submatches using the regular expression

		expectedIdentifier, expectedMsgsStr, err := decodeOpMsgSectionSequence(expectedSection)
		if err != nil {
			logger.Debug(fmt.Sprintf("the section in mongo OpMsg wiremessage: %v", expectedSection))
			logger.Error("failed to fetch the identifier/msgs from the section sequence of recorded OpMsg", zap.Error(err), zap.Any("identifier", expectedIdentifier))
			return 0
		}

		var actualIdentifier string
		var actualMsgsStr string
		// _, err = fmt.Sscanf(actualSection, "{ SectionSingle identifier: %s , msgs: [ %s ] }", &actualIdentifier, &actualMsgsStr)
		actualIdentifier, actualMsgsStr, err = decodeOpMsgSectionSequence(actualSection)
		if err != nil {
			logger.Error("failed to fetch the identifier/msgs from the section sequence of incoming OpMsg", zap.Error(err), zap.Any("identifier", actualIdentifier))
			return 0
		}

		// // Compile the regular expression
		// // Find submatches using the regular expression

		logger.Debug("the expected section", zap.Any("identifier", expectedIdentifier), zap.Any("docs", expectedMsgsStr))
		logger.Debug("the actual section", zap.Any("identifier", actualIdentifier), zap.Any("docs", actualMsgsStr))

		expectedMsgs := strings.Split(expectedMsgsStr, ", ")
		actualMsgs := strings.Split(actualMsgsStr, ", ")
		if len(expectedMsgs) != len(actualMsgs) || expectedIdentifier != actualIdentifier {
			return 0
		}
		score := 0.0
		for i := range expectedMsgs {
			expected := map[string]interface{}{}
			actual := map[string]interface{}{}
			err := bson.UnmarshalExtJSON([]byte(expectedMsgs[i]), true, &expected)
			if err != nil {
				logger.Error("failed to unmarshal the section of recorded request to bson document", zap.Error(err))
				return 0
			}
			err = bson.UnmarshalExtJSON([]byte(actualMsgs[i]), true, &actual)
			if err != nil {
				logger.Error("failed to unmarshal the section of incoming request to bson document", zap.Error(err))
				return 0
			}
			score += calculateMatchingScore(expected, actual)
		}
		logger.Debug("the matching score for sectionSequence", zap.Any("", score))
		return score
	case strings.HasPrefix(expectedSection, "{ SectionSingle msg:"):
		var expectedMsgsStr string
		expectedMsgsStr, err := extractSectionSingle(expectedSection)
		if err != nil {
			logger.Error("failed to fetch the msgs from the single section of recorded OpMsg", zap.Error(err))
			return 0
		}
		// // Define the regular expression pattern
		// // Compile the regular expression
		// // Find submatches using the regular expression

		var actualMsgsStr string
		actualMsgsStr, err = extractSectionSingle(actualSection)
		if err != nil {
			logger.Error("failed to fetch the msgs from the single section of incoming OpMsg", zap.Error(err))
			return 0
		}
		// // Compile the regular expression
		// // Find submatches using the regular expression

		expected := map[string]interface{}{}
		actual := map[string]interface{}{}

		err = bson.UnmarshalExtJSON([]byte(expectedMsgsStr), true, &expected)
		if err != nil {
			logger.Error("failed to unmarshal the section of recorded request to bson document", zap.Error(err))
			return 0
		}
		err = bson.UnmarshalExtJSON([]byte(actualMsgsStr), true, &actual)
		if err != nil {
			logger.Error("failed to unmarshal the section of incoming request to bson document", zap.Error(err))
			return 0
		}
		logger.Debug("the expected and actual msg in the single section.", zap.Any("expected", expected), zap.Any("actual", actual), zap.Any("score", calculateMatchingScore(expected, actual)))
		return calculateMatchingScore(expected, actual)

	default:
		logger.Error("failed to detect the OpMsg section into mongo request wiremessage due to invalid format")
		return 0
	}
}

func calculateMatchingScore(obj1, obj2 map[string]interface{}) float64 {
	totalFields := len(obj2)
	matchingFields := 0.0

	for key, value := range obj2 {
		if obj1Value, ok := obj1[key]; ok {
			if reflect.DeepEqual(value, obj1Value) {
				matchingFields++
			} else if reflect.TypeOf(value) == reflect.TypeOf(obj1Value) {
				if isNestedMap(value) {
					if isNestedMap(obj1Value) {
						matchingFields += calculateMatchingScore(obj1Value.(map[string]interface{}), value.(map[string]interface{}))
					}
				} else if isSlice(value) {
					if isSlice(obj1Value) {
						matchingFields += calculateMatchingScoreForSlices(obj1Value.([]interface{}), value.([]interface{}))
					}
				}
			}
		}
	}

	return float64(matchingFields) / float64(totalFields)
}

func calculateMatchingScoreForSlices(slice1, slice2 []interface{}) float64 {
	matchingCount := 0

	if len(slice1) == len(slice2) {
		for indx2, item2 := range slice2 {
			if len(slice1) > indx2 && reflect.DeepEqual(item2, slice1[indx2]) {
				matchingCount++
			}
		}
	}

	return float64(matchingCount) / float64(len(slice2))
}

func isNestedMap(value interface{}) bool {
	_, ok := value.(map[string]interface{})
	return ok
}

func isSlice(value interface{}) bool {
	_, ok := value.([]interface{})
	return ok
}
