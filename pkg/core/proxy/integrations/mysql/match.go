package mysql

import (
	"fmt"
	"go.keploy.io/server/v2/pkg/models"
)

func matchRequestWithMock(mysqlRequest models.MySQLRequest, configMocks, tcsMocks []*models.Mock) (*models.MySQLResponse, int, string, error) {
	//TODO: any reason to write the similar code twice?
	allMocks := append([]*models.Mock(nil), configMocks...)
	allMocks = append(allMocks, tcsMocks...)
	var bestMatch *models.MySQLResponse
	var matchedIndex int
	var matchedReqIndex int
	var mockType string
	maxMatchCount := 0

	for i, mock := range allMocks {
		for j, mockReq := range mock.Spec.MySqlRequests {
			matchCount := compareMySQLRequests(mysqlRequest, mockReq)
			if matchCount > maxMatchCount {
				maxMatchCount = matchCount
				matchedIndex = i
				matchedReqIndex = j
				mockType = mock.Spec.Metadata["type"]
				if len(mock.Spec.MySqlResponses) > j {
					if mockType == "config" {
						responseCopy := mock.Spec.MySqlResponses[j]
						bestMatch = &responseCopy
					} else {
						bestMatch = &mock.Spec.MySqlResponses[j]
					}
				}
			}
		}
	}

	if bestMatch == nil {
		return nil, -1, "", fmt.Errorf("no matching mock found")
	}

	if mockType == "config" {
		if matchedIndex >= len(configMocks) {
			return nil, -1, "", fmt.Errorf("index out of range in configMocks")
		}
		configMocks[matchedIndex].Spec.MySqlRequests = append(configMocks[matchedIndex].Spec.MySqlRequests[:matchedReqIndex], configMocks[matchedIndex].Spec.MySqlRequests[matchedReqIndex+1:]...)
		configMocks[matchedIndex].Spec.MySqlResponses = append(configMocks[matchedIndex].Spec.MySqlResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySqlResponses[matchedReqIndex+1:]...)

		if len(configMocks[matchedIndex].Spec.MySqlResponses) == 0 {
			configMocks = append(configMocks[:matchedIndex], configMocks[matchedIndex+1:]...)
		}
		//h.SetConfigMocks(configMocks)
	} else {
		realIndex := matchedIndex - len(configMocks)
		if realIndex < 0 || realIndex >= len(tcsMocks) {
			return nil, -1, "", fmt.Errorf("index out of range in tcsMocks")
		}
		tcsMocks[realIndex].Spec.MySqlRequests = append(tcsMocks[realIndex].Spec.MySqlRequests[:matchedReqIndex], tcsMocks[realIndex].Spec.MySqlRequests[matchedReqIndex+1:]...)
		tcsMocks[realIndex].Spec.MySqlResponses = append(tcsMocks[realIndex].Spec.MySqlResponses[:matchedReqIndex], tcsMocks[realIndex].Spec.MySqlResponses[matchedReqIndex+1:]...)

		if len(tcsMocks[realIndex].Spec.MySqlResponses) == 0 {
			tcsMocks = append(tcsMocks[:realIndex], tcsMocks[realIndex+1:]...)
		}
		//h.SetTcsMocks(tcsMocks)
	}

	return bestMatch, matchedIndex, mockType, nil
}

func compareMySQLRequests(req1, req2 models.MySQLRequest) int {
	matchCount := 0

	// Compare Header fields
	if req1.Header.PacketType == "MySQLQuery" && req2.Header.PacketType == "MySQLQuery" {
		packet1 := req1.Message
		packet, ok := packet1.(*QueryPacket)
		if !ok {
			return 0
		}
		packet2 := req2.Message

		packet3, ok := packet2.(*models.MySQLQueryPacket)
		if !ok {
			return 0
		}
		if packet.Query == packet3.Query {
			matchCount += 5
		}
	}
	if req1.Header.PacketLength == req2.Header.PacketLength {
		matchCount++
	}
	if req1.Header.PacketNumber == req2.Header.PacketNumber {
		matchCount++
	}
	if req1.Header.PacketType == req2.Header.PacketType {
		matchCount++
	}
	return matchCount
}
