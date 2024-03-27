package mongo

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/utils"
	"go.mongodb.org/mongo-driver/bson"

	"go.keploy.io/server/v2/pkg/models"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func match(ctx context.Context, logger *zap.Logger, mongoRequests []models.MongoRequest, mockDb integrations.MockMemDb) (bool, *models.Mock, error) {
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:
			tcsMocks, err := mockDb.GetFilteredMocks()
			if err != nil {
				return false, nil, fmt.Errorf("error while getting tcs mock: %v", err)
			}
			maxMatchScore := 0.0
			bestMatchIndex := -1
			for tcsIndx, tcsMock := range tcsMocks {
				if ctx.Err() != nil {
					return false, nil, ctx.Err()
				}
				if len(tcsMock.Spec.MongoRequests) == len(mongoRequests) {
					for i, req := range tcsMock.Spec.MongoRequests {
						if ctx.Err() != nil {
							return false, nil, ctx.Err()
						}
						if len(tcsMock.Spec.MongoRequests) != len(mongoRequests) || req.Header.Opcode != mongoRequests[i].Header.Opcode {
							logger.Debug("the recieved request is not of same type with the tcmocks", zap.Any("at index", tcsIndx))
							continue
						}
						switch req.Header.Opcode {
						case wiremessage.OpMsg:
							if req.Message.(*models.MongoOpMessage).FlagBits != mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits {
								logger.Debug("the recieved request is not of same flagbit with the tcmocks", zap.Any("at index", tcsIndx))
								continue
							}
							scoreSum := 0.0
							for sectionIndx, section := range req.Message.(*models.MongoOpMessage).Sections {
								if len(req.Message.(*models.MongoOpMessage).Sections) == len(mongoRequests[i].Message.(*models.MongoOpMessage).Sections) {
									score := compareOpMsgSection(logger, section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx])
									scoreSum += score
								}
							}
							currentScore := scoreSum / float64(len(mongoRequests))
							if currentScore > maxMatchScore {
								maxMatchScore = currentScore
								bestMatchIndex = tcsIndx
							}
						default:
							utils.LogError(logger, nil, "the OpCode of the mongo wiremessage is invalid.")
						}
					}
				}
			}
			if bestMatchIndex == -1 {
				return false, nil, nil
			}
			mock := tcsMocks[bestMatchIndex]
			isDeleted := mockDb.DeleteFilteredMock(mock)
			if !isDeleted {
				continue
			}
			return true, mock, nil
		}
	}
}

func compareOpMsgSection(logger *zap.Logger, expectedSection, actualSection string) float64 {
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
			utils.LogError(logger, err, "failed to fetch the identifier/msgs from the section single of recorded OpMsg", zap.Any("identifier", expectedIdentifier))
			return 0
		}

		var actualIdentifier string
		var actualMsgsStr string
		// _, err = fmt.Sscanf(actualSection, "{ SectionSingle identifier: %s , msgs: [ %s ] }", &actualIdentifier, &actualMsgsStr)
		actualIdentifier, actualMsgsStr, err = decodeOpMsgSectionSequence(actualSection)
		if err != nil {
			utils.LogError(logger, err, "failed to fetch the identifier/msgs from the section single of incoming OpMsg", zap.Any("identifier", actualIdentifier))
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
				utils.LogError(logger, err, "failed to unmarshal the section of recorded request to bson document")
				return 0
			}
			err = bson.UnmarshalExtJSON([]byte(actualMsgs[i]), true, &actual)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal the section of incoming request to bson document")
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
			utils.LogError(logger, err, "failed to fetch the msgs from the single section of recorded OpMsg")
			return 0
		}
		// // Define the regular expression pattern
		// // Compile the regular expression
		// // Find submatches using the regular expression

		var actualMsgsStr string
		actualMsgsStr, err = extractSectionSingle(actualSection)
		if err != nil {
			utils.LogError(logger, err, "failed to fetch the msgs from the single section of incoming OpMsg")
			return 0
		}
		// // Compile the regular expression
		// // Find submatches using the regular expression

		expected := map[string]interface{}{}
		actual := map[string]interface{}{}

		err = bson.UnmarshalExtJSON([]byte(expectedMsgsStr), true, &expected)
		if err != nil {
			utils.LogError(logger, err, "failed to unmarshal the section of recorded request to bson document")
			return 0
		}
		err = bson.UnmarshalExtJSON([]byte(actualMsgsStr), true, &actual)
		if err != nil {
			utils.LogError(logger, err, "failed to unmarshal the section of incoming request to bson document")
			return 0
		}
		logger.Debug("the expected and actual msg in the single section.", zap.Any("expected", expected), zap.Any("actual", actual), zap.Any("score", calculateMatchingScore(expected, actual)))
		return calculateMatchingScore(expected, actual)

	default:
		utils.LogError(logger, nil, "failed to detect the OpMsg section into mongo request wiremessage due to invalid format")
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
