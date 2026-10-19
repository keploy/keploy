package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

const canary = "kp-canary-grpc-match-3e0a"

// A gRPC answer's message is compared without it in the lines, unless
// KEPLOY_LOG_CONTENT is set.
func TestMatch_TheDecodedMessagesAreNotLogged(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		hdrs := models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
		msg := func(v string) models.GrpcLengthPrefixedMessage {
			return models.GrpcLengthPrefixedMessage{MessageLength: 20, DecodedData: `{"note":"` + v + `"}`}
		}
		tc := &models.TestCase{Name: "grpc-notes", GrpcResp: models.GrpcResp{Headers: hdrs, Body: msg(canary + "-recorded"), Trailers: hdrs}}
		actual := &models.GrpcResp{Headers: hdrs, Body: msg(canary + "-live"), Trailers: hdrs}
		logger, logs := logtest.Observe()
		if pass, _ := Match(tc, actual, nil, false, logger, false); pass {
			t.Fatal("another message matched")
		}
		logtest.Check(t, logs, canary, on, "Both gRPC decoded data are valid JSON, using JSON comparison")
	})
}
