package mongoparser

import (
	"fmt"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func match(h *hooks.Hook, mongoRequests []models.MongoRequest, logger *zap.Logger) (bool, *models.Mock, error) {
	for {
		tcsMocks, err := h.GetTcsMocks()
		if err != nil {
			return false, nil, fmt.Errorf("error while getting tcs mock: %v", err)
		}
		maxMatchScore := 0.0
		bestMatchIndex := -1
		for tcsIndx, tcsMock := range tcsMocks {
			if len(tcsMock.Spec.MongoRequests) == len(mongoRequests) {
				for i, req := range tcsMock.Spec.MongoRequests {
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
								score := compareOpMsgSection(section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx], logger)
								scoreSum += score
							}
						}
						currentScore := scoreSum / float64(len(mongoRequests))
						if currentScore > maxMatchScore {
							maxMatchScore = currentScore
							bestMatchIndex = tcsIndx
						}
					default:
						logger.Error("the OpCode of the mongo wiremessage is invalid.")
					}
				}
			}
		}
		if bestMatchIndex == -1 {
			return false, nil, nil
		}
		mock := tcsMocks[bestMatchIndex]
<<<<<<< HEAD
		isDeleted := h.DeleteTcsMock(mock)
=======
		isDeleted, err := h.DeleteTcsMock(mock)
		if err != nil {
			return false, nil, fmt.Errorf("error while deleting tcs mock: %v", err)
		}
>>>>>>> 70bcbc0 (merge: resolves merge conflicts)
		if !isDeleted {
			continue
		}
		return true, mock, nil
	}
}
