package mysql

import (
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"
)

func matchRequestWithMock(ctx context.Context, mysqlRequest models.MySQLRequest, configMocks, tcsMocks []*models.Mock, mockDb integrations.MockMemDb) (*models.MySQLResponse, int, string, error) {
	//TODO: any reason to write the similar code twice?
	allMocks := append([]*models.Mock(nil), configMocks...)
	allMocks = append(allMocks, tcsMocks...)
	var bestMatch *models.MySQLResponse
	var matchedIndex int
	var matchedReqIndex int
	var mockType string
	maxMatchCount := 0

	for i, mock := range allMocks {
		if ctx.Err() != nil {
			return nil, -1, "", ctx.Err()
		}
		for j, mockReq := range mock.Spec.MySQLRequests {
			if ctx.Err() != nil {
				return nil, -1, "", ctx.Err()
			}
			matchCount := compareMySQLRequests(mysqlRequest, mockReq)
			if matchCount > maxMatchCount {
				maxMatchCount = matchCount
				matchedIndex = i
				matchedReqIndex = j
				mockType = mock.Spec.Metadata["type"]
				if len(mock.Spec.MySQLResponses) > j {
					if mockType == "config" {
						responseCopy := mock.Spec.MySQLResponses[j]
						bestMatch = &responseCopy
					} else {
						bestMatch = &mock.Spec.MySQLResponses[j]
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
		configMocks[matchedIndex].Spec.MySQLRequests = append(configMocks[matchedIndex].Spec.MySQLRequests[:matchedReqIndex], configMocks[matchedIndex].Spec.MySQLRequests[matchedReqIndex+1:]...)
		configMocks[matchedIndex].Spec.MySQLResponses = append(configMocks[matchedIndex].Spec.MySQLResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySQLResponses[matchedReqIndex+1:]...)
		if len(configMocks[matchedIndex].Spec.MySQLResponses) == 0 {
			mockDb.DeleteUnFilteredMock(configMocks[matchedIndex])
		}
	} else {
		realIndex := matchedIndex - len(configMocks)
		if realIndex < 0 || realIndex >= len(tcsMocks) {
			return nil, -1, "", fmt.Errorf("index out of range in tcsMocks")
		}
		tcsMocks[realIndex].Spec.MySQLRequests = append(tcsMocks[realIndex].Spec.MySQLRequests[:matchedReqIndex], tcsMocks[realIndex].Spec.MySQLRequests[matchedReqIndex+1:]...)
		tcsMocks[realIndex].Spec.MySQLResponses = append(tcsMocks[realIndex].Spec.MySQLResponses[:matchedReqIndex], tcsMocks[realIndex].Spec.MySQLResponses[matchedReqIndex+1:]...)
		if len(tcsMocks[realIndex].Spec.MySQLResponses) == 0 {
			mockDb.DeleteFilteredMock(tcsMocks[realIndex])
		}
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
