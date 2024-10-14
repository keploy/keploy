//go:build linux

package mongo

import (
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func hasSecondSetBit(num int) bool {
	// Shift the number right by 1 bit and check if the least significant bit is set
	return (num>>1)&1 == 1
}

// Skip heartbeat from capturing in the global set of mocks. Since, the heartbeat packet always contain the "hello" boolean.
// See: https://github.com/mongodb/mongo-go-driver/blob/8489898c64a2d8c2e2160006eb851a11a9db9e9d/x/mongo/driver/operation/hello.go#L503
func isHeartBeat(logger *zap.Logger, opReq Operation, requestHeader models.MongoHeader, mongoRequest interface{}) bool {

	switch requestHeader.Opcode {
	case wiremessage.OpQuery:
		return true
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
