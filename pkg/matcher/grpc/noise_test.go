// Package grpc noise tests.
package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func grpcTestCase(decoded string, noise map[string][]string) *models.TestCase {
	return &models.TestCase{
		Name: "grpc-noise",
		GrpcResp: models.GrpcResp{
			Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":status": "200"},
				OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
			},
			Body: models.GrpcLengthPrefixedMessage{
				CompressionFlag: 0,
				MessageLength:   uint32(len(decoded)),
				DecodedData:     decoded,
			},
			Trailers: models.GrpcHeaders{
				OrdinaryHeaders: map[string]string{"grpc-status": "0"},
			},
		},
		Noise: noise,
	}
}

func grpcActual(decoded string) *models.GrpcResp {
	return &models.GrpcResp{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		},
		Body: models.GrpcLengthPrefixedMessage{
			CompressionFlag: 0,
			MessageLength:   uint32(len(decoded)),
			DecodedData:     decoded,
		},
		Trailers: models.GrpcHeaders{
			OrdinaryHeaders: map[string]string{"grpc-status": "0"},
		},
	}
}

// TestMatch_SectionedBodyNoise covers the gRPC matcher's half of the sectioned
// noise contract. It had no noise coverage at all before.
func TestMatch_SectionedBodyNoise(t *testing.T) {
	const recorded = `{"stock":99,"name":"Laptop"}`

	t.Run("listed path is ignored", func(t *testing.T) {
		tc := grpcTestCase(recorded, map[string][]string{"body": {"stock"}})
		actual := grpcActual(`{"stock":100,"name":"Laptop"}`)
		// MessageLength is derived from the payload and differs by design here;
		// only the decoded-body verdict is under test.
		_, res := Match(tc, actual, map[string]map[string][]string{}, false, zap.NewNop(), false)
		if !decodedDataNormal(res) {
			t.Errorf("expected the decoded body to match: only the listed noisy path differs")
		}
	})

	t.Run("unlisted field is still compared", func(t *testing.T) {
		tc := grpcTestCase(recorded, map[string][]string{"body": {"stock"}})
		actual := grpcActual(`{"stock":100,"name":"Tablet"}`)
		_, res := Match(tc, actual, map[string]map[string][]string{}, false, zap.NewNop(), false)
		if decodedDataNormal(res) {
			t.Errorf("expected a decoded-body mismatch: name differs and is not noise")
		}
	})
}

// TestMatch_DoesNotMutateSharedNoiseConfig is the regression test for the worst
// instance of the shared-map leak: replay passes its long-lived global noise
// config straight into the gRPC matcher without copying, so a test case's own
// noise used to persist for the remainder of the run.
func TestMatch_DoesNotMutateSharedNoiseConfig(t *testing.T) {
	shared := map[string]map[string][]string{"body": {}, "header": {}}

	tc := grpcTestCase(`{"total":1}`, map[string][]string{
		"body":       {"total"},
		"body.stock": {},
	})
	Match(tc, grpcActual(`{"total":2}`), shared, false, zap.NewNop(), false)

	if len(shared["body"]) != 0 {
		t.Errorf("shared global noise config was mutated: %v", shared["body"])
	}

	// A later test case declaring no noise must still catch the difference.
	clean := grpcTestCase(`{"total":1}`, nil)
	_, res := Match(clean, grpcActual(`{"total":2}`), shared, false, zap.NewNop(), false)
	if decodedDataNormal(res) {
		t.Errorf("noise from an earlier test case leaked into a later one")
	}
}

// decodedDataNormal returns the verdict for the gRPC decoded-body result.
func decodedDataNormal(res *models.Result) bool {
	for _, b := range res.BodyResult {
		if b.Type == models.GrpcData {
			return b.Normal
		}
	}
	return false
}
