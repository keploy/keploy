//go:build linux

package v1

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/agnivade/levenshtein"
	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

var testmap TestPrepMap

func getTestPS(reqBuff [][]byte, logger *zap.Logger, ConnectionID string) {
	// maintain a map of current prepared statements and their corresponding connection id
	// if it's the prepared statement match the query with the recorded prepared statement and return the response of that matched prepared statement at that connection
	// so if parse is coming save to a same map
	actualPgReq := decodePgRequest(reqBuff[0], logger)
	if actualPgReq == nil {
		return
	}
	testmap2 := make(TestPrepMap)
	if testmap != nil {
		testmap2 = testmap
	}
	querydata := make([]QueryData, 0)
	if len(actualPgReq.PacketTypes) > 0 && actualPgReq.PacketTypes[0] != "p" && actualPgReq.Identfier != "StartupRequest" {
		p := 0
		for _, header := range actualPgReq.PacketTypes {
			if header == "P" {
				if (strings.Contains(actualPgReq.Parses[p].Name, "S_") || strings.Contains(actualPgReq.Parses[p].Name, "s")) && !IsValuePresent(ConnectionID, actualPgReq.Parses[p].Name) {
					querydata = append(querydata, QueryData{PrepIdentifier: actualPgReq.Parses[p].Name, Query: actualPgReq.Parses[p].Query})
				}
				p++
			}
		}
	}

	// also append the query data for the prepared statement
	if len(querydata) > 0 {
		testmap2[ConnectionID] = append(testmap2[ConnectionID], querydata...)
		// logger.Debug("Test Prepared statement Map", testmap2)
		testmap = testmap2
	}

}

func IsValuePresent(connectionid string, value string) bool {
	if testmap != nil {
		for _, v := range testmap[connectionid] {
			if v.PrepIdentifier == value {
				return true
			}
		}
	}
	return false
}

func matchingReadablePG(ctx context.Context, logger *zap.Logger, mutex *sync.Mutex, requestBuffers [][]byte, mockDb integrations.MockMemDb) (bool, []models.Frontend, error) {
OuterLoop:
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:

			mocks, err := mockDb.GetUnFilteredMocks()
			var tcsMocks []*models.Mock

			for _, mock := range mocks {
				if mock.Kind != "Postgres" {
					continue
				}
				tcsMocks = append(tcsMocks, mock)
			}
			if err != nil {
				return false, nil, fmt.Errorf("error while getting tcs mocks %v", err)
			}

			ConnectionID := ctx.Value(models.ClientConnectionIDKey).(string)

			recordedPrep := getRecordPrepStatement(tcsMocks)
			reqGoingOn := decodePgRequest(requestBuffers[0], logger)
			if reqGoingOn != nil {
				logger.Debug("PacketTypes", zap.Any("PacketTypes", reqGoingOn.PacketTypes))
				// fmt.Println("REQUEST GOING ON - ", reqGoingOn)
				logger.Debug("ConnectionId-", zap.String("ConnectionId", ConnectionID))
				logger.Debug("TestMap*****", zap.Any("TestMap", testmap))
			}

			// merge all the streaming requests into 1 for matching
			newRq := mergePgRequests(requestBuffers, logger)
			if len(newRq) > 0 {
				requestBuffers = newRq
			}

			var sortFlag = true
			var sortedTcsMocks []*models.Mock
			var matchedMock *models.Mock

			for _, mock := range tcsMocks {
				if ctx.Err() != nil {
					return false, nil, ctx.Err()
				}
				if mock == nil {
					continue
				}

				mutex.Lock()
				if sortFlag {
					if !mock.TestModeInfo.IsFiltered {
						sortFlag = false
					} else {
						sortedTcsMocks = append(sortedTcsMocks, mock)
					}
				}
				mutex.Unlock()

				initMock := *mock
				if len(initMock.Spec.PostgresRequests) == len(requestBuffers) {
					for requestIndex, reqBuff := range requestBuffers {
						bufStr := base64.StdEncoding.EncodeToString(reqBuff)
						encodedMock, err := postgresDecoderBackend(initMock.Spec.PostgresRequests[requestIndex])
						if err != nil {
							logger.Debug("Error while decoding postgres request", zap.Error(err))
						}

						switch {
						case bufStr == "AAAACATSFi8=":
							ssl := models.Frontend{
								Payload: "Tg==",
							}
							return true, []models.Frontend{ssl}, nil
						case initMock.Spec.PostgresRequests[requestIndex].Identfier == "StartupRequest" && isStartupPacket(reqBuff) && initMock.Spec.PostgresRequests[requestIndex].Payload != "AAAACATSFi8=" && initMock.Spec.PostgresResponses[requestIndex].AuthType == 10:
							logger.Debug("CHANGING TO MD5 for Response", zap.String("mock", initMock.Name), zap.String("Req", bufStr))
							res := make([]models.Frontend, len(initMock.Spec.PostgresResponses))
							copy(res, initMock.Spec.PostgresResponses)
							res[requestIndex].AuthType = 5
							newInitMock := initMock
							newInitMock.TestModeInfo.IsFiltered = false
							newInitMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
							isUpdated := mockDb.UpdateUnFilteredMock(&initMock, &newInitMock)
							if !isUpdated {
								logger.Debug("failed to update matched mock", zap.Error(err))
								continue OuterLoop
							}
							return true, res, nil
						case len(encodedMock) > 0 && encodedMock[0] == 'p' && initMock.Spec.PostgresRequests[requestIndex].PacketTypes[0] == "p" && reqBuff[0] == 'p':
							logger.Debug("CHANGING TO MD5 for Request and Response", zap.String("mock", initMock.Name), zap.String("Req", bufStr))

							res := make([]models.Frontend, len(initMock.Spec.PostgresResponses))
							copy(res, initMock.Spec.PostgresResponses)
							res[requestIndex].PacketTypes = []string{"R", "S", "S", "S", "S", "S", "S", "S", "S", "S", "S", "S", "K", "Z"}
							res[requestIndex].AuthType = 0
							res[requestIndex].BackendKeyData = pgproto3.BackendKeyData{
								ProcessID: 2613,
								SecretKey: 824670820,
							}
							res[requestIndex].ReadyForQuery.TxStatus = 73
							res[requestIndex].ParameterStatusCombined = []pgproto3.ParameterStatus{
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
							newInitMock := initMock
							newInitMock.TestModeInfo.IsFiltered = false
							newInitMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
							isUpdated := mockDb.UpdateUnFilteredMock(&initMock, &newInitMock)
							if !isUpdated {
								logger.Debug("failed to update matched mock", zap.Error(err))
								continue OuterLoop
							}
							return true, res, nil
						}

					}
				}

				// maintain test prepare statement map for each connection id
				getTestPS(requestBuffers, logger, ConnectionID)
			}

			logger.Debug("Sorted Mocks inside pg parser: ", zap.Any("Len of sortedTcsMocks", len(sortedTcsMocks)))

			var matched, sorted bool
			var idx int
			//use findBinaryMatch twice one for sorted and another for unsorted
			// give more priority to sorted like if you find more than 0.5 in sorted then return that
			if len(sortedTcsMocks) > 0 {
				sorted = true
				idx1, newMock := findPGStreamMatch(sortedTcsMocks, requestBuffers, logger, sorted, ConnectionID, recordedPrep)
				if idx1 != -1 {
					matched = true
					matchedMock = tcsMocks[idx1]
					if newMock != nil {
						matchedMock = newMock
					}
					logger.Debug("Matched In Sorted PG Matching Stream", zap.String("mock", matchedMock.Name))
				}

				if !matched {
					idx = findBinaryStreamMatch(logger, sortedTcsMocks, requestBuffers, sorted)
					if idx != -1 {
						matched = true
						matchedMock = tcsMocks[idx]
					}
				}
			}

			if !matched {
				sorted = false
				idx1, newMock := findPGStreamMatch(tcsMocks, requestBuffers, logger, sorted, ConnectionID, recordedPrep)
				if idx1 != -1 {
					matched = true
					matchedMock = tcsMocks[idx1]
					if newMock != nil {
						matchedMock = newMock
					}
					logger.Debug("Matched In Unsorted PG Matching Stream", zap.String("mock", matchedMock.Name))
				}
				if !matched {
					idx = findBinaryStreamMatch(logger, tcsMocks, requestBuffers, sorted)
					// check if the validate the query with the matched mock
					// if the query is same then return the response of that mock
					var isValid = true
					if idx != -1 && len(sortedTcsMocks) != 0 {
						isValid, newMock = validateMock(tcsMocks, idx, requestBuffers, logger)
						logger.Debug("Is Valid", zap.Bool("Is Valid", isValid))
					}
					if idx != -1 {
						matched = true
						matchedMock = tcsMocks[idx]
						if newMock != nil && !isValid {
							matchedMock = newMock
						}
						logger.Debug("Matched In Binary Matching for Unsorted", zap.String("mock", matchedMock.Name))
					}

				}
			}

			if matched {
				logger.Debug("Matched mock", zap.String("mock", matchedMock.Name))
				originalMatchedMock := *matchedMock
				matchedMock.TestModeInfo.IsFiltered = false
				matchedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
				updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
				if !updated {
					logger.Debug("failed to update matched mock", zap.Error(err))
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
		// merging the mocks as well before comparing
		mock.Spec.PostgresRequests = mergeMocks(mock.Spec.PostgresRequests, logger)

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

func findPGStreamMatch(tcsMocks []*models.Mock, requestBuffers [][]byte, logger *zap.Logger, isSorted bool, connectionID string, recordedPrep PrepMap) (int, *models.Mock) {

	mxIdx := -1

	match := false
	// loop for the exact match of the request
	for idx, mock := range tcsMocks {
		// merging the mocks as well before comparing
		mock.Spec.PostgresRequests = mergeMocks(mock.Spec.PostgresRequests, logger)

		if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
			for _, reqBuff := range requestBuffers {
				actualPgReq := decodePgRequest(reqBuff, logger)
				if actualPgReq == nil {
					return -1, nil
				}
				// here handle cases of prepared statement very carefully
				match, err := compareExactMatch(mock, actualPgReq, logger)
				if err != nil {
					logger.Error("Error while matching exact match", zap.Error(err))
					continue
				}
				if match {
					return idx, nil
				}
			}
		}
	}
	if !isSorted {
		return mxIdx, nil
	}
	// loop for the ps match of the request
	if !match {
		for idx, mock := range tcsMocks {
			// merging the mocks as well before comparing
			mock.Spec.PostgresRequests = mergeMocks(mock.Spec.PostgresRequests, logger)

			if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
				for _, reqBuff := range requestBuffers {
					actualPgReq := decodePgRequest(reqBuff, logger)
					if actualPgReq == nil {
						return -1, nil
					}
					// just matching the corresponding PS in this case there is no need to edit the mock
					match, newBindPs, err := PreparedStatementMatch(mock, actualPgReq, logger, connectionID, recordedPrep)
					if err != nil {
						logger.Error("Error while matching prepared statements", zap.Error(err))
					}

					if match {
						logger.Debug("New Bind Prepared Statement", zap.Any("New Bind Prepared Statement", newBindPs), zap.String("ConnectionId", connectionID), zap.String("Mock Name", mock.Name))
						return idx, nil
					}
					// just check the query
					if reflect.DeepEqual(actualPgReq.PacketTypes, []string{"P", "B", "D", "E"}) && reflect.DeepEqual(mock.Spec.PostgresRequests[0].PacketTypes, []string{"P", "B", "D", "E"}) {
						if mock.Spec.PostgresRequests[0].Parses[0].Query == actualPgReq.Parses[0].Query {
							return idx, nil
						}
					}
				}
			}
		}
	}

	if !match {

		for idx, mock := range tcsMocks {
			// merging the mocks as well before comparing
			mock.Spec.PostgresRequests = mergeMocks(mock.Spec.PostgresRequests, logger)

			if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
				for _, reqBuff := range requestBuffers {
					actualPgReq := decodePgRequest(reqBuff, logger)
					if actualPgReq == nil {
						return -1, nil
					}

					// have to ignore first parse message of begin read only
					// should compare only query in the parse message
					if len(actualPgReq.PacketTypes) != len(mock.Spec.PostgresRequests[0].PacketTypes) {
						//check for begin read only
						if len(actualPgReq.PacketTypes) > 0 && len(mock.Spec.PostgresRequests[0].PacketTypes) > 0 {

							ischanged, newMock := changeResToPS(mock, actualPgReq, logger, connectionID)

							if ischanged {
								return idx, newMock
							}
							continue

						}

					}
				}
			}
		}
	}

	return mxIdx, nil
}

// check what are the queries for the given ps of actualPgReq
// check if the execute query is present for that or not
// mark that mock true and return the response by changing the res format like
// postgres data types acc to result set format
func changeResToPS(mock *models.Mock, actualPgReq *models.Backend, logger *zap.Logger, connectionID string) (bool, *models.Mock) {
	actualpackets := actualPgReq.PacketTypes
	mockPackets := mock.Spec.PostgresRequests[0].PacketTypes

	// [P, B, E, P, B, D, E] => [B, E, B, E]
	// write code that of packet is ["B", "E"] and mockPackets ["P", "B", "D", "E"] handle it in case1
	// and if packet is [B, E, B, E] and mockPackets [P, B, E, P, B, D, E] handle it in case2

	ischanged := false
	var newMock *models.Mock
	// [B E P D B E]
	// [P, B, E, P, B, D, E] -> [B, E, P, B, D, E]
	if (reflect.DeepEqual(actualpackets, []string{"B", "E", "P", "D", "B", "E"}) || reflect.DeepEqual(actualpackets, []string{"B", "E", "P", "B", "D", "E"})) && reflect.DeepEqual(mockPackets, []string{"P", "B", "E", "P", "B", "D", "E"}) {
		// logger.Debug("Handling Case 1 for mock", mock.Name)
		// handleCase1(packets, mockPackets)
		// also check if the second query is same or not
		// logger.Debug("ActualPgReq", actualPgReq.Parses[0].Query, "MOCK REQ 1", mock.Spec.PostgresRequests[0].Parses[0].Query, "MOCK REQ 2", mock.Spec.PostgresRequests[0].Parses[1].Query)
		if actualPgReq.Parses[0].Query != mock.Spec.PostgresRequests[0].Parses[1].Query {
			return false, nil
		}
		newMock = sliceCommandTag(mock, logger, testmap[connectionID], actualPgReq, 1)
		return true, newMock
	}

	// case 2
	var ps string
	if reflect.DeepEqual(actualpackets, []string{"B", "E"}) && reflect.DeepEqual(mockPackets, []string{"P", "B", "D", "E"}) {
		// logger.Debug("Handling Case 2 for mock", mock.Name)
		ps = actualPgReq.Binds[0].PreparedStatement
		for _, v := range testmap[connectionID] {
			if v.Query == mock.Spec.PostgresRequests[0].Parses[0].Query && v.PrepIdentifier == ps {
				ischanged = true
				break
			}
		}
	}

	if ischanged {
		// if strings.Contains(ps, "S_") {
		// logger.Debug("Inside Prepared Statement")
		newMock = sliceCommandTag(mock, logger, testmap[connectionID], actualPgReq, 2)
		// }
		return true, newMock
	}

	// packets = []string{"B", "E", "B", "E"}
	// mockPackets = []string{"P", "B", "E", "P", "B", "D", "E"}

	// Case 3
	if reflect.DeepEqual(actualpackets, []string{"B", "E", "B", "E"}) && reflect.DeepEqual(mockPackets, []string{"P", "B", "E", "P", "B", "D", "E"}) {
		// logger.Debug("Handling Case 3 for mock", mock.Name)
		ischanged1 := false
		ps1 := actualPgReq.Binds[0].PreparedStatement
		for _, v := range testmap[connectionID] {
			if v.Query == mock.Spec.PostgresRequests[0].Parses[0].Query && v.PrepIdentifier == ps1 {
				ischanged1 = true
				break
			}
		}
		//Matched In Binary Matching for Unsorted mock-222
		ischanged2 := false
		ps2 := actualPgReq.Binds[1].PreparedStatement
		for _, v := range testmap[connectionID] {
			if v.Query == mock.Spec.PostgresRequests[0].Parses[1].Query && v.PrepIdentifier == ps2 {
				ischanged2 = true
				break
			}
		}
		if ischanged1 && ischanged2 {
			newMock = sliceCommandTag(mock, logger, testmap[connectionID], actualPgReq, 2)
			return true, newMock
		}
	}

	// Case 4
	if reflect.DeepEqual(actualpackets, []string{"B", "E", "B", "E"}) && reflect.DeepEqual(mockPackets, []string{"B", "E", "P", "B", "D", "E"}) {
		// logger.Debug("Handling Case 4 for mock", mock.Name)
		// get the query for the prepared statement of test mode
		ischanged := false
		ps := actualPgReq.Binds[1].PreparedStatement
		for _, v := range testmap[connectionID] {
			if v.Query == mock.Spec.PostgresRequests[0].Parses[0].Query && v.PrepIdentifier == ps {
				ischanged = true
				break
			}
		}
		if ischanged {
			newMock = sliceCommandTag(mock, logger, testmap[connectionID], actualPgReq, 2)
			return true, newMock
		}

	}

	return false, nil

}

func PreparedStatementMatch(mock *models.Mock, actualPgReq *models.Backend, logger *zap.Logger, ConnectionID string, recordedPrep PrepMap) (bool, []string, error) {
	// logger.Debug("Inside PreparedStatementMatch")

	if !reflect.DeepEqual(mock.Spec.PostgresRequests[0].PacketTypes, actualPgReq.PacketTypes) {
		logger.Debug("mock and actual packet types are unequal", zap.Any("mock name", mock.Name))
		return false, nil, nil
	}

	// get all the binds from the actualPgReq
	binds := actualPgReq.Binds
	newBinPreparedStatement := make([]string, 0)
	mockBinds := mock.Spec.PostgresRequests[0].Binds
	// If the client sent a different number of Bind messages than the mock
	// recorded, the two batches can’t possibly align, so we can return early
	// instead of walking the loop and risking panic due to out‑of‑bounds.
	if len(binds) != len(mockBinds) {
		logger.Debug("len of binds in actual request is not equal to len of binds in mock", zap.String("mock name", mock.Name))
		return false, nil, nil
	}
	mockConn := mock.ConnectionID
	var foo = false
	for idx, bind := range binds {
		currentPs := bind.PreparedStatement
		currentQuerydata := testmap[ConnectionID]
		currentQuery := ""
		// check in the map that what's the current query for this preparedstatement
		// then will check what is the recorded prepared statement for this query
		for _, v := range currentQuerydata {
			if v.PrepIdentifier == currentPs {
				// logger.Debug("Current query for this identifier is ", v.Query)
				currentQuery = v.Query
				break
			}
		}

		// this means that the bind is preceeded by a parse with name field empty
		// we can say that the name field (identifier) was empty that's why it didn't get inserted in testMap.
		// skip it, as it doesn't use already cached query, instead parsing followed by binding is done in the same query.
		if currentQuery == "" {
			continue
		}

		logger.Debug("Current Query for this prepared statement", zap.String("Query", currentQuery), zap.String("Identifier", currentPs))
		foo = false

		// check if the query for mock ps (v.PreparedStatement) is same as the current query
		for _, querydata := range recordedPrep[mockConn] {
			if querydata.Query == currentQuery && mockBinds[idx].PreparedStatement == querydata.PrepIdentifier {
				logger.Debug("Matched with the recorded prepared statement with Identifier and connectionID is", zap.String("Identifier", querydata.PrepIdentifier), zap.String("ConnectionId", mockConn), zap.String("Current Identifier", currentPs), zap.String("Query", currentQuery))
				foo = true
				break
			}

		}
		// this means we are unable to find the query in recordedPrep or the prepared statement is not same
		if !foo {
			break
		}
	}
	if !foo {
		return false, nil, nil
	}

	parses := actualPgReq.Parses
	mockParses := mock.Spec.PostgresRequests[0].Parses
	// If the client sent a different number of Parse messages than the mock
	// recorded, the two batches can’t possibly align, so we can return early
	// instead of walking the loop and risking panic due to out‑of‑bounds.
	if len(parses) != len(mockParses) {
		logger.Debug("len of parse in actual request is not equal to len of parse in mock", zap.String("mock name", mock.Name))
		return false, nil, nil
	}

	foo = true
	// check if all parse queries in pg request is same for the corresponding query in mock
	for idx, parse := range parses {
		if parse.Query != mockParses[idx].Query {
			logger.Debug(fmt.Sprintf("parse query for actual request is not equal to parse query for mock name: %s, at index: %d", mock.Name, idx))
			foo = false // if any parse query is not same then break, mock didn't match
			break
		}
	}
	if foo {
		return true, newBinPreparedStatement, nil
	}

	return false, nil, nil
}

func compareExactMatch(mock *models.Mock, actualPgReq *models.Backend, logger *zap.Logger) (bool, error) {
	logger.Debug("Inside CompareExactMatch")
	// have to ignore first parse message of begin read only
	// should compare only query in the parse message
	if len(actualPgReq.PacketTypes) != len(mock.Spec.PostgresRequests[0].PacketTypes) {
		return false, nil
	}

	// call a separate function for matching prepared statements
	for idx, v := range actualPgReq.PacketTypes {
		if v != mock.Spec.PostgresRequests[0].PacketTypes[idx] {
			return false, nil
		}
	}
	// IsPreparedStatement(mock, actualPgReq, logger, ConnectionId)

	// this will give me the
	var (
		p, b, e = 0, 0, 0
	)
	for i := 0; i < len(actualPgReq.PacketTypes); i++ {
		switch actualPgReq.PacketTypes[i] {
		case "P":
			// logger.Debug("Inside P")
			p++
			if actualPgReq.Parses[p-1].Query != mock.Spec.PostgresRequests[0].Parses[p-1].Query {
				return false, nil
			}

			if actualPgReq.Parses[p-1].Name != mock.Spec.PostgresRequests[0].Parses[p-1].Name {
				return false, nil
			}

			if len(actualPgReq.Parses[p-1].ParameterOIDs) != len(mock.Spec.PostgresRequests[0].Parses[p-1].ParameterOIDs) {
				return false, nil
			}
			for j := 0; j < len(actualPgReq.Parses[p-1].ParameterOIDs); j++ {
				if actualPgReq.Parses[p-1].ParameterOIDs[j] != mock.Spec.PostgresRequests[0].Parses[p-1].ParameterOIDs[j] {
					return false, nil
				}
			}

		case "B":
			// logger.Debug("Inside B")
			b++
			if actualPgReq.Binds[b-1].DestinationPortal != mock.Spec.PostgresRequests[0].Binds[b-1].DestinationPortal {
				return false, nil
			}

			if actualPgReq.Binds[b-1].PreparedStatement != mock.Spec.PostgresRequests[0].Binds[b-1].PreparedStatement {
				return false, nil
			}

			if len(actualPgReq.Binds[b-1].ParameterFormatCodes) != len(mock.Spec.PostgresRequests[0].Binds[b-1].ParameterFormatCodes) {
				return false, nil
			}
			for j := 0; j < len(actualPgReq.Binds[b-1].ParameterFormatCodes); j++ {
				if actualPgReq.Binds[b-1].ParameterFormatCodes[j] != mock.Spec.PostgresRequests[0].Binds[b-1].ParameterFormatCodes[j] {
					return false, nil
				}
			}
			if len(actualPgReq.Binds[b-1].Parameters) != len(mock.Spec.PostgresRequests[0].Binds[b-1].Parameters) {
				return false, nil
			}
			for j := 0; j < len(actualPgReq.Binds[b-1].Parameters); j++ {
				// parameter represents a timestamp value do not compare it just continue
				if isTimestamp(actualPgReq.Binds[b-1].Parameters[j]) {
					logger.Debug("found a timestamp value")
					continue
				}
				if isBcryptHash(actualPgReq.Binds[b-1].Parameters[j]) {
					logger.Debug("found a bcrypt hash")
					continue
				}
				for i, v := range actualPgReq.Binds[b-1].Parameters[j] {
					if v != mock.Spec.PostgresRequests[0].Binds[b-1].Parameters[j][i] {
						return false, nil
					}
				}
			}
			if len(actualPgReq.Binds[b-1].ResultFormatCodes) != len(mock.Spec.PostgresRequests[0].Binds[b-1].ResultFormatCodes) {
				return false, nil
			}
			for j := 0; j < len(actualPgReq.Binds[b-1].ResultFormatCodes); j++ {
				if actualPgReq.Binds[b-1].ResultFormatCodes[j] != mock.Spec.PostgresRequests[0].Binds[b-1].ResultFormatCodes[j] {
					return false, nil
				}
			}

		case "E":
			// logger.Debug("Inside E")
			e++
			if actualPgReq.Executes[e-1].Portal != mock.Spec.PostgresRequests[0].Executes[e-1].Portal {
				return false, nil
			}
			if actualPgReq.Executes[e-1].MaxRows != mock.Spec.PostgresRequests[0].Executes[e-1].MaxRows {
				return false, nil
			}

		case "c":
			if actualPgReq.CopyDone != mock.Spec.PostgresRequests[0].CopyDone {
				return false, nil
			}
		case "H":
			if actualPgReq.CopyFail.Message != mock.Spec.PostgresRequests[0].CopyFail.Message {
				return false, nil
			}
		case "Q":
			if actualPgReq.Query.String != mock.Spec.PostgresRequests[0].Query.String {
				if LaevensteinDistance(actualPgReq.Query.String, mock.Spec.PostgresRequests[0].Query.String) {
					logger.Debug("The strings are more than 90%% similar.")
				}

				return false, nil
			}
		default:
			return false, nil
		}
	}
	return true, nil
}

func LaevensteinDistance(str1, str2 string) bool {
	// Compute the Levenshtein distance
	distance := levenshtein.ComputeDistance(str1, str2)
	maxLength := max(len(str1), len(str2))
	similarity := (1 - float64(distance)/float64(maxLength)) * 100

	// Check if similarity is greater than 90%
	return similarity > 90

}

// make this in such a way if it returns -1 then we will continue with the original mock
func validateMock(tcsMocks []*models.Mock, idx int, requestBuffers [][]byte, logger *zap.Logger) (bool, *models.Mock) {

	actualPgReq := decodePgRequest(requestBuffers[0], logger)
	if actualPgReq == nil {
		return true, nil
	}
	mock := tcsMocks[idx].Spec.PostgresRequests[0]
	if len(mock.PacketTypes) == len(actualPgReq.PacketTypes) {
		if reflect.DeepEqual(tcsMocks[idx].Spec.PostgresRequests[0].PacketTypes, []string{"B", "E", "P", "B", "D", "E"}) {
			if mock.Parses[0].Query == actualPgReq.Parses[0].Query {
				return true, nil
			}
		}
		if reflect.DeepEqual(mock.PacketTypes, []string{"B", "E", "B", "E"}) {
			// logger.Debug("Inside Validate Mock for B, E, B, E")
			return true, nil
		}
		if reflect.DeepEqual(mock.PacketTypes, []string{"B", "E"}) {
			// logger.Debug("Inside Validate Mock for B, E")
			copyMock := *tcsMocks[idx]
			copyMock.Spec.PostgresResponses[0].PacketTypes = []string{"2", "C", "Z"}
			copyMock.Spec.PostgresResponses[0].Payload = ""
			return false, &copyMock
		}
		if reflect.DeepEqual(mock.PacketTypes, []string{"P", "B", "D", "E"}) {
			// logger.Debug("Inside Validate Mock for P, B, D, E")
			copyMock := *tcsMocks[idx]
			copyMock.Spec.PostgresResponses[0].PacketTypes = []string{"1", "2", "T", "C", "Z"}
			copyMock.Spec.PostgresResponses[0].Payload = ""
			return false, &copyMock
		}
	} else {
		// [B, E, P, B, D, E] => [ P, B, D, E]
		if reflect.DeepEqual(mock.PacketTypes, []string{"B", "E", "P", "B", "D", "E"}) && reflect.DeepEqual(actualPgReq.PacketTypes, []string{"P", "B", "D", "E"}) {
			// logger.Debug("Inside Validate Mock for B, E, B, E")
			if mock.Parses[0].Query == actualPgReq.Parses[0].Query {
				// no need to do anything

				copyMock := *tcsMocks[idx]
				copyMock.Spec.PostgresResponses[0].PacketTypes = []string{"1", "2", "T", "C", "Z"}
				copyMock.Spec.PostgresResponses[0].Payload = ""
				copyMock.Spec.PostgresResponses[0].CommandCompletes = copyMock.Spec.PostgresResponses[0].CommandCompletes[1:]
				return false, &copyMock
			}
		}
	}
	return true, nil
}

func isTimestamp(byteArray []byte) bool {
	// Convert byte array to string
	s := string(byteArray)

	// Define a regex for ISO 8601 timestamps
	timestampRegex := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(.\d+)?(Z)?`)
	return timestampRegex.MatchString(s)
}

func isBcryptHash(byteArray []byte) bool {
	// Convert byte array to string
	s := string(byteArray)

	// Define a regex for bcrypt hashes
	bcryptRegex := regexp.MustCompile(`^\$2[aby]\$\d{2}\$[./A-Za-z0-9]{53}$`)
	return bcryptRegex.MatchString(s)
}
