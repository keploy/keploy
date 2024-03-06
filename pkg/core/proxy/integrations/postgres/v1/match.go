package v1

import (
	"context"
	"fmt"
	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"math"
)

func matchingReadablePG(ctx context.Context, logger *zap.Logger, requestBuffers [][]byte, mockDb integrations.MockMemDb) (bool, []models.Frontend, error) {
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:

			tcsMocks, err := mockDb.GetUnFilteredMocks()
			if err != nil {
				return false, nil, fmt.Errorf("error while getting tcs mocks %v", err)
			}

			var sortFlag = true
			var sortedTcsMocks []*models.Mock
			var matchedMock *models.Mock

			for _, mock := range tcsMocks {
				if mock == nil {
					continue
				}

				if sortFlag {
					if mock.TestModeInfo.IsFiltered == false {
						sortFlag = false
					} else {
						sortedTcsMocks = append(sortedTcsMocks, mock)
					}
				}

				if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
					for requestIndex, reqBuf := range requestBuffers {
						bufStr := util.EncodeBase64(reqBuf)
						encoded, err := postgresDecoderBackend(mock.Spec.PostgresRequests[requestIndex])
						if err != nil {
							logger.Debug("Error while decoding postgres request", zap.Error(err))
						}

						if mock.Spec.PostgresRequests[requestIndex].Identfier == "StartupRequest" {
							logger.Debug("CHANGING TO MD5 for Response")
							mock.Spec.PostgresResponses[requestIndex].AuthType = 5
							continue
						}

						if len(encoded) > 0 && encoded[0] == 'p' {
							logger.Debug("CHANGING TO MD5 for Request and Response")
							mock.Spec.PostgresRequests[requestIndex].PasswordMessage.Password = "md5fe4f2f657f01fa1dd9d111d5391e7c07"

							mock.Spec.PostgresResponses[requestIndex].PacketTypes = []string{"R", "S", "S", "S", "S", "S", "S", "S", "S", "S", "S", "S", "K", "Z"}
							mock.Spec.PostgresResponses[requestIndex].AuthType = 0
							mock.Spec.PostgresResponses[requestIndex].BackendKeyData = pgproto3.BackendKeyData{
								ProcessID: 2613,
								SecretKey: 824670820,
							}
							mock.Spec.PostgresResponses[requestIndex].ReadyForQuery.TxStatus = 73
							mock.Spec.PostgresResponses[requestIndex].ParameterStatusCombined = []pgproto3.ParameterStatus{
								{
									Name:  "application_name",
									Value: "",
								},
								{
									Name:  "client_encoding",
									Value: "UTF8",
								},
								{
									Name:  "DateStyle",
									Value: "ISO, MDY",
								},
								{
									Name:  "integer_datetimes",
									Value: "on",
								},
								{
									Name:  "IntervalStyle",
									Value: "postgres",
								},
								{
									Name:  "is_superuser",
									Value: "UTF8",
								},
								{
									Name:  "server_version",
									Value: "13.12 (Debian 13.12-1.pgdg120+1)",
								},
								{
									Name:  "session_authorization",
									Value: "keploy-user",
								},
								{
									Name:  "standard_conforming_strings",
									Value: "on",
								},
								{
									Name:  "TimeZone",
									Value: "Etc/UTC",
								},
								{
									Name:  "TimeZone",
									Value: "Etc/UTC",
								},
							}
						}

						if bufStr == "AAAACATSFi8=" {
							ssl := models.Frontend{
								Payload: "Tg==",
							}
							return true, []models.Frontend{ssl}, nil
						}
					}
				}
			}

			logger.Debug("Sorted Mocks: ", zap.Any("Len of sortedTcsMocks", len(sortedTcsMocks)))

			var matched, sorted = false, false
			var idx int
			//use findBinaryMatch twice one for sorted and another for unsorted
			// give more priority to sorted like if you find more than 0.5 in sorted then return that
			if len(sortedTcsMocks) > 0 {
				sorted = true
				idx = findBinaryStreamMatch(logger, sortedTcsMocks, requestBuffers, sorted)
				if idx != -1 {
					matched = true
					matchedMock = tcsMocks[idx]
				}
			}

			if !matched {
				sorted = false
				idx = findBinaryStreamMatch(logger, tcsMocks, requestBuffers, sorted)
				if idx != -1 {
					matched = true
					matchedMock = tcsMocks[idx]
				}
			}

			if matched {
				logger.Debug("Matched mock", zap.String("mock", matchedMock.Name))
				if matchedMock.TestModeInfo.IsFiltered {
					originalMatchedMock := matchedMock
					matchedMock.TestModeInfo.IsFiltered = false
					matchedMock.TestModeInfo.SortOrder = math.MaxInt
					updated := mockDb.UpdateUnFilteredMock(originalMatchedMock, matchedMock)
					if !updated {
						continue
					}
				}
				return true, matchedMock.Spec.PostgresResponses, nil
			}

			return false, nil, nil
		}
	}
}

func findBinaryStreamMatch(logger *zap.Logger, tcsMocks []*models.Mock, requestBuffers [][]byte, sorted bool) int {

	mxSim := -1.0
	mxIdx := -1

	for idx, mock := range tcsMocks {

		if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
			for requestIndex, reqBuf := range requestBuffers {

				expectedPgReq := mock.Spec.PostgresRequests[requestIndex]
				encoded, err := postgresDecoderBackend(expectedPgReq)
				if err != nil {
					logger.Debug("Error while decoding postgres request", zap.Error(err))
				}
				var encoded64 []byte
				if expectedPgReq.Payload != "" {
					encoded64, err = util.DecodeBase64(mock.Spec.PostgresRequests[requestIndex].Payload)
					if err != nil {
						logger.Debug("Error while decoding postgres request", zap.Error(err))
						return -1
					}
				}
				var similarity1, similarity2 float64
				if len(encoded) > 0 {
					similarity1 = fuzzyCheck(encoded, reqBuf)
				}
				if len(encoded64) > 0 {
					similarity2 = fuzzyCheck(encoded64, reqBuf)
				}

				// calculate the jaccard similarity between the two buffers one with base64 encoding and another via that
				//find the max similarity between the two
				similarity := math.Max(similarity1, similarity2)
				if mxSim < similarity {
					mxSim = similarity
					mxIdx = idx
					continue
				}
			}
		}
	}

	if sorted {
		if mxIdx != -1 && mxSim >= 0.78 {
			logger.Debug("Matched with Sorted Stream", zap.Float64("similarity", mxSim))
		} else {
			mxIdx = -1
		}
	} else {
		if mxIdx != -1 {
			logger.Debug("Matched with Unsorted Stream", zap.Float64("similarity", mxSim))
		}
	}
	return mxIdx
}

func fuzzyCheck(encoded, reqBuf []byte) float64 {
	k := util.AdaptiveK(len(reqBuf), 3, 8, 5)
	shingles1 := util.CreateShingles(encoded, k)
	shingles2 := util.CreateShingles(reqBuf, k)
	similarity := util.JaccardSimilarity(shingles1, shingles2)
	return similarity
}
