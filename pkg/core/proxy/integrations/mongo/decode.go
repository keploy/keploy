//go:build linux

package mongo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

// decodeMongo decodes the mongo wire message from the client connection
// and sends the response back to the client.
func decodeMongo(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	startedDecoding := time.Now()
	requestBuffers := [][]byte{reqBuf}

	errCh := make(chan error, 1)

	go func(errCh chan error, reqBuf []byte, startedDecoding time.Time, requestBuffers [][]byte) {
		defer util.Recover(logger, clientConn, nil)
		defer close(errCh)
		var readRequestDelay time.Duration
		var err error
		for {

			var (
				mongoRequests []models.MongoRequest // stores the request packet
			)
			// check to read the request buffer from the client connection after the initial packet
			if string(reqBuf) == "read form client conn" {
				started := time.Now()
				// reads the first chunk of the mongo request
				reqBuf, err = util.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					if err == io.EOF {
						logger.Debug("received request buffer is empty in test mode for mongo calls")
						errCh <- err
						return
					}
					utils.LogError(logger, err, "failed to read request from the mongo client")
					errCh <- err
					return
				}
				requestBuffers = append(requestBuffers, reqBuf)
				logger.Debug("the request from the mongo client", zap.Any("buffer", reqBuf))
				readRequestDelay = time.Since(started)
			}
			if len(reqBuf) == 0 {
				errCh <- errors.New("the request buffer is empty")
				return
			}
			logger.Debug(fmt.Sprintf("the loop starts with the time delay: %v", time.Since(startedDecoding)))
			// convert the request buffer to the mongo wire message in the go struct
			opReq, requestHeader, mongoRequest, err := Decode(reqBuf, logger)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the mongo wire message from the client")
				errCh <- err
				return
			}
			mongoRequests = append(mongoRequests, models.MongoRequest{
				Header:    &requestHeader,
				Message:   mongoRequest,
				ReadDelay: int64(readRequestDelay),
			})
			// check for the more_to_come flag bit in the mongo request
			// header to read the next chunks of the request
			if val, ok := mongoRequest.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
				for {
					started := time.Now()
					logger.Debug("into the for loop for request stream")
					// reads the next chunk of the mongo request
					requestBuffer1, err := util.ReadBytes(ctx, logger, clientConn)
					if err != nil {
						if err == io.EOF {
							logger.Debug("received request buffer is empty for streaming mongo request call")
							errCh <- err
							return
						}
						utils.LogError(logger, err, "failed to read request from the mongo client", zap.String("mongo server address", dstCfg.Addr))
						errCh <- err
						return
					}
					requestBuffers = append(requestBuffers, reqBuf)
					readRequestDelay = time.Since(started)

					if len(requestBuffer1) == 0 {
						logger.Debug("the response from the server is complete")
						break
					}
					// convert the request buffer to the mongo response wire message in the go struct
					_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the mongo wire message from the mongo client")
						errCh <- err
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
			// check for the heartbeat request from client and use the config mocks to respond
			if isHeartBeat(logger, opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
				// isScramAuth(logger, opReq) {
				var bestMatchIndex = -1
				var maxMatchScore = 0.0
				var configMocks []*models.Mock
				for {
					configMocks, err = mockDb.GetUnFilteredMocks()
					if err != nil {
						utils.LogError(logger, err, "error while getting config mock")
					}
					logger.Debug(fmt.Sprintf("the config mocks are: %v", configMocks))
					maxMatchScore = 0.0
					bestMatchIndex = -1
					// loop over the recorded config mocks to match with the incoming request
					for configIndex, configMock := range configMocks {
						logger.Debug("the config mock is: ", zap.Any("config mock", configMock), zap.Any("actual request", mongoRequests))
						// checking the number of chunks for recorded config mocks and the incoming request
						if len(configMock.Spec.MongoRequests) != len(mongoRequests) {
							continue
						}

						for i, req := range configMock.Spec.MongoRequests {
							// check the opcode of the incoming request and the recorded config mocks
							if len(configMock.Spec.MongoRequests) != len(mongoRequests) || req.Header.Opcode != mongoRequests[i].Header.Opcode {
								continue
							}
							switch req.Header.Opcode {
							case wiremessage.OpQuery:
								// check the query fields of the incoming request and the recorded config mocks
								expectedQuery := req.Message.(*models.MongoOpQuery)
								actualQuery := mongoRequests[i].Message.(*models.MongoOpQuery)
								if actualQuery.FullCollectionName != expectedQuery.FullCollectionName ||
									actualQuery.ReturnFieldsSelector != expectedQuery.ReturnFieldsSelector ||
									actualQuery.Flags != expectedQuery.Flags ||
									actualQuery.NumberToReturn != expectedQuery.NumberToReturn ||
									actualQuery.NumberToSkip != expectedQuery.NumberToSkip {
									continue
								}

								// calculate the matching score for query bson dcouments of the incoming request and the recorded config mocks
								expected := map[string]interface{}{}
								actual := map[string]interface{}{}
								err = bson.UnmarshalExtJSON([]byte(expectedQuery.Query), true, &expected)
								if err != nil {
									utils.LogError(logger, err, "failed to unmarshal the section of recorded request to bson document")
									continue
								}
								err = bson.UnmarshalExtJSON([]byte(actualQuery.Query), true, &actual)
								if err != nil {
									utils.LogError(logger, err, "failed to unmarshal the section of incoming request to bson document")
									continue
								}
								score := calculateMatchingScore(expected, actual)
								logger.Debug("the expected and actual msg in the heartbeat OpQuery query.", zap.Any("expected", expected), zap.Any("actual", actual), zap.Any("score", score))
								if score > maxMatchScore {
									maxMatchScore = score
									bestMatchIndex = configIndex
								}

							case wiremessage.OpMsg:
								// check the OpMsg sections of the incoming request and the recorded config mocks
								if req.Message.(*models.MongoOpMessage).FlagBits != mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits {
									continue
								}
								scoreSum := 0.0
								if len(req.Message.(*models.MongoOpMessage).Sections) != len(mongoRequests[i].Message.(*models.MongoOpMessage).Sections) {
									continue
								}
								// calculate the matching score for each section of the incoming request and the recorded config mocks
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
								utils.LogError(logger, err, "the OpCode of the mongo wiremessage is invalid.", zap.Any("opcode", req.Header.Opcode))
							}
						}
					}

					if bestMatchIndex != -1 && maxMatchScore != 0.0 {
						matchedMock := *configMocks[bestMatchIndex]
						matchedMock.TestModeInfo.SortOrder = math.MaxInt
						isUpdated := mockDb.UpdateUnFilteredMock(configMocks[bestMatchIndex], &matchedMock)
						if !isUpdated {
							continue
						}
					}
					break
				}

				responseTo := mongoRequests[0].Header.RequestID
				if bestMatchIndex == -1 || maxMatchScore == 0.0 {
					logger.Debug("the mongo request do not matches with any config mocks", zap.Any("request", mongoRequests))
					continue
				}
				// set the config as used in the mockManager
				err = mockDb.FlagMockAsUsed(*configMocks[bestMatchIndex])
				if err != nil {
					utils.LogError(logger, err, "failed to flag mock as used in mongo parser", zap.Any("for mock", configMocks[bestMatchIndex].Name))
					errCh <- err
					return
				}
				// write the mongo response to the client connection from the recorded config mocks that most matches the incoming request
				for _, mongoResponse := range configMocks[bestMatchIndex].Spec.MongoResponses {
					switch mongoResponse.Header.Opcode {
					case wiremessage.OpReply:
						replySpec := mongoResponse.Message.(*models.MongoOpReply)
						replyMessage, err := encodeOpReply(ctx, mongoRequests[0], configMocks[bestMatchIndex].Spec.MongoRequests[0], replySpec, opts.MongoPassword, logger)
						if err != nil {
							utils.LogError(logger, err, "failed to encode the recorded OpReply yaml", zap.Any("for request with id", responseTo))
							errCh <- err
							return
						}
						requestID := wiremessage.NextRequestID()
						heathCheckReplyBuffer := replyMessage.Encode(responseTo, requestID)
						responseTo = requestID
						logger.Debug(fmt.Sprintf("the bufffer response is: %v", string(heathCheckReplyBuffer)))
						// handle scram auth request
						_, err = clientConn.Write(heathCheckReplyBuffer)
						if err != nil {
							if ctx.Err() != nil {
								return
							}
							utils.LogError(logger, err, "failed to write the health check reply to mongo client")
							errCh <- err
							return
						}
					case wiremessage.OpMsg:
						respMessage := mongoResponse.Message.(*models.MongoOpMessage)

						var expectedRequestSections []string
						if len(configMocks[bestMatchIndex].Spec.MongoRequests) > 0 {
							expectedRequestSections = configMocks[bestMatchIndex].Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections
						}
						message, err := encodeOpMsg(ctx, respMessage, mongoRequest.(*models.MongoOpMessage).Sections, expectedRequestSections, opts.MongoPassword, logger)
						if err != nil {
							utils.LogError(logger, err, "failed to encode the recorded OpMsg response", zap.Any("for request with id", responseTo))
							errCh <- err
							return
						}
						_, err = clientConn.Write(message.Encode(responseTo, wiremessage.NextRequestID()))
						if err != nil {
							if ctx.Err() != nil {
								return
							}
							utils.LogError(logger, err, "failed to write the health check opmsg to mongo client")
							errCh <- err
							return
						}
					}
				}
			} else {
				// handle for the non-heartbeat request from the client

				// match the incoming request with the recorded tcsMocks and return a mocked response which matches most with incoming request
				matched, matchedMock, err := match(ctx, logger, mongoRequests, mockDb)
				if err != nil {
					errCh <- err
					utils.LogError(logger, err, "error while matching mongo mocks")
					return
				}
				if !matched {
					logger.Debug("mongo request not matched with any tcsMocks", zap.Any("request", mongoRequests))
					reqBuf, err = util.PassThrough(ctx, logger, clientConn, dstCfg, requestBuffers)
					if err != nil {
						utils.LogError(logger, err, "failed to passthrough the mongo request to the actual database server")
						errCh <- err
						return
					}
					continue
				}

				responseTo := mongoRequests[0].Header.RequestID
				logger.Debug("the mock matched with the current request", zap.Any("mock", matchedMock), zap.Any("responseTo", responseTo))

				// write the mongo response to the client connection from the recorded tcsMocks that most matches the incoming request
				for _, resp := range matchedMock.Spec.MongoResponses {
					respMessage := resp.Message.(*models.MongoOpMessage)
					var expectedRequestSections []string
					if len(matchedMock.Spec.MongoRequests) > 0 {
						expectedRequestSections = matchedMock.Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections
					}
					message, err := encodeOpMsg(ctx, respMessage, mongoRequest.(*models.MongoOpMessage).Sections, expectedRequestSections, opts.MongoPassword, logger)
					if err != nil {
						utils.LogError(logger, err, "failed to encode the recorded OpMsg response", zap.Any("for request with id", responseTo))
						errCh <- err
						return
					}
					requestID := wiremessage.NextRequestID()
					_, err = clientConn.Write(message.Encode(responseTo, requestID))
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						utils.LogError(logger, err, "failed to write the health check opmsg to mongo client", zap.Any("for request with id", responseTo))
						errCh <- err
						return
					}
					responseTo = requestID
				}
			}
			logger.Debug("the length of the requestBuffer after matching: " + strconv.Itoa(len(reqBuf)) + strconv.Itoa(len(requestBuffers[0])))
			if len(requestBuffers) > 0 && len(reqBuf) == len(requestBuffers[0]) {
				reqBuf = []byte("read form client conn")
			}

			// Clear the buffer for the next dependency call
			requestBuffers = [][]byte{}
		}
	}(errCh, reqBuf, startedDecoding, requestBuffers)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			logger.Debug("connection lost from client")
			return nil
		}
		return err
	}
}
