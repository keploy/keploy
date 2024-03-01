package mongo

import (
	"context"
	"errors"
	"fmt"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
	"io"
	"net"
	"strconv"
	"time"
)

func decodeMongo(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	startedDecoding := time.Now()
	requestBuffers := [][]byte{reqBuf}
	var readRequestDelay time.Duration
	for {

		select {
		case <-ctx.Done():
			return nil
		default:
			configMocks, err := mockDb.GetUnFilteredMocks()
			if err != nil {
				logger.Error("error while getting config mock", zap.Error(err))
			}
			logger.Debug(fmt.Sprintf("the config mocks are: %v", configMocks))

			var (
				mongoRequests []models.MongoRequest
			)
			if string(reqBuf) == "read form client conn" {
				started := time.Now()
				reqBuf, err = util.ReadBytes(ctx, clientConn)
				if err != nil {
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in test mode for mongo calls")
						return err
					}
					logger.Error("failed to read request from the mongo client", zap.Error(err))
					return err
				}
				requestBuffers = append(requestBuffers, reqBuf)
				logger.Debug("the request from the mongo client", zap.Any("buffer", reqBuf))
				readRequestDelay = time.Since(started)
			}
			if len(reqBuf) == 0 {
				return errors.New("the request buffer is empty")
			}
			logger.Debug(fmt.Sprintf("the loop starts with the time delay: %v", time.Since(startedDecoding)))
			opReq, requestHeader, mongoRequest, err := Decode(reqBuf, logger)
			if err != nil {
				logger.Error("failed to decode the mongo wire message from the client", zap.Error(err))
				return err
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
					requestBuffer1, err := util.ReadBytes(ctx, clientConn)
					if err != nil {
						if err == io.EOF {
							logger.Debug("recieved request buffer is empty for streaming mongo request call")
							return err
						}
						logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", dstCfg.Addr))
						return err
					}
					requestBuffers = append(requestBuffers, reqBuf)
					readRequestDelay = time.Since(started)

					if len(requestBuffer1) == 0 {
						logger.Debug("the response from the server is complete")
						break
					}
					_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
					if err != nil {
						logger.Error("failed to decode the mongo wire message from the mongo client", zap.Error(err))
						return err
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
			if isHeartBeat(logger, opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
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
										score := compareOpMsgSection(logger, section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx])
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
							return err
						}
						requestId := wiremessage.NextRequestID()
						heathCheckReplyBuffer := replyMessage.Encode(responseTo, requestId)
						responseTo = requestId
						logger.Debug(fmt.Sprintf("the bufffer response is: %v", string(heathCheckReplyBuffer)))
						_, err = clientConn.Write(heathCheckReplyBuffer)
						if err != nil {
							logger.Error("failed to write the health check reply to mongo client", zap.Error(err))
							return err
						}
					case wiremessage.OpMsg:
						respMessage := mongoResponse.Message.(*models.MongoOpMessage)

						var expectedRequestSections []string
						if len(configMocks[bestMatchIndex].Spec.MongoRequests) > 0 {
							expectedRequestSections = configMocks[bestMatchIndex].Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections
						}
						message, err := encodeOpMsg(respMessage, mongoRequest.(*models.MongoOpMessage).Sections, expectedRequestSections, logger)
						if err != nil {
							logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", responseTo))
							return err
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
									return err
								}
								// the 'responseTo' field of response wiremessage is set to requestId of currently sent wiremessage
								responseTo = requestId
							}
						} else {
							_, err := clientConn.Write(message.Encode(responseTo, wiremessage.NextRequestID()))
							if err != nil {
								logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
								return err
							}
						}
					}
				}
			} else {
				matched, matchedMock, err := match(ctx, logger, mongoRequests, mockDb)
				if err != nil {
					logger.Error("failed to match the mongo request with recorded tcsMocks", zap.Error(err))
				}
				if !matched {
					// making destConn
					destConn, err := net.Dial("tcp", dstCfg.Addr)
					if err != nil {
						logger.Error("failed to dial the destination server", zap.Error(err))
						return err
					}

					logger.Debug("mongo request not matched with any tcsMocks", zap.Any("request", mongoRequests))
					reqBuf, err = util.PassThrough(ctx, logger, clientConn, destConn, requestBuffers)
					if err != nil {
						logger.Error("failed to passthrough the mongo request to the actual database server", zap.Error(err))
						return err
					}
					continue
				}

				responseTo := mongoRequests[0].Header.RequestID
				logger.Debug("the mock matched with the current request", zap.Any("mock", matchedMock), zap.Any("responseTo", responseTo))

				for _, resp := range matchedMock.Spec.MongoResponses {
					respMessage := resp.Message.(*models.MongoOpMessage)
					var expectedRequestSections []string
					if len(matchedMock.Spec.MongoRequests) > 0 {
						expectedRequestSections = matchedMock.Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections
					}
					message, err := encodeOpMsg(respMessage, mongoRequest.(*models.MongoOpMessage).Sections, expectedRequestSections, logger)
					if err != nil {
						logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", responseTo))
						return err
					}
					requestId := wiremessage.NextRequestID()
					_, err = clientConn.Write(message.Encode(responseTo, requestId))
					if err != nil {
						logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err), zap.Any("for request with id", responseTo))
						return err
					}
					responseTo = requestId
				}
			}
			logger.Debug("the length of the requestBuffer after matching: " + strconv.Itoa(len(reqBuf)) + strconv.Itoa(len(requestBuffers[0])))
			if len(requestBuffers) > 0 && len(reqBuf) == len(requestBuffers[0]) {
				reqBuf = []byte("read form client conn")
			}

			// Clear the buffer for the next dependency call
			requestBuffers = [][]byte{}
		}
	}
}
