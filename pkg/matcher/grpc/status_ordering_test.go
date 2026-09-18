package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMatch_GrpcStatusFlipMustFail is a latent bug that this streaming work
// turns load-bearing.
//
// `matched := len(differences) == 0` is evaluated BEFORE the grpc-status
// trailer comparison writes differences["trailers.grpc-status"]. So a
// response whose body is byte-identical but whose grpc-status flipped from 0
// (OK) to 13 (INTERNAL) is reported as a PASS — result.StatusCode.Normal is
// correctly false, and the returned bool says the test passed anyway.
//
// It matters more once streams replay: today SimulateGRPC reads trailers
// before io.EOF, so they come back empty and grpc-status is fabricated as "0"
// — the actual status is almost always 0 and the bug rarely shows. Draining a
// stream properly makes real trailers arrive, at which point a stream that
// dies partway carries a real non-zero status and would be reported green.
func TestMatch_GrpcStatusFlipMustFail(t *testing.T) {
	body := models.GrpcLengthPrefixedMessage{MessageLength: 7, DecodedData: `1: {"alpha"}`}
	hdrs := func(status string) models.GrpcHeaders {
		return models.GrpcHeaders{
			PseudoHeaders:   map[string]string{},
			OrdinaryHeaders: map[string]string{"grpc-status": status},
		}
	}

	tc := &models.TestCase{
		Name: "grpc-status-flip",
		GrpcResp: models.GrpcResp{
			Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}},
			Body:     body,
			Trailers: hdrs("0"), // recorded OK
		},
	}
	actual := &models.GrpcResp{
		Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}},
		Body:     body,       // identical body
		Trailers: hdrs("13"), // INTERNAL
	}

	pass, result := Match(tc, actual, nil, false, zap.NewNop(), false)
	if result != nil && result.StatusCode.Normal {
		t.Fatalf("StatusCode.Normal = true for 0 vs 13; the comparison itself is broken")
	}
	if pass {
		t.Fatal("Match reported PASS for a response whose grpc-status flipped 0 -> 13. " +
			"`matched` is computed before the grpc-status comparison contributes its " +
			"difference, so a failed RPC with an identical body prints \"Testrun passed\".")
	}
}
