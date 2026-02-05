package replay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestGetBackdateTimestamp_GRPCFirstTestCase(t *testing.T) {
	grpcTimestamp := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	httpTimestamp := time.Date(2024, 1, 1, 10, 1, 0, 0, time.UTC)

	testCases := []*models.TestCase{
		{
			Name: "grpc-test-1",
			Kind: models.GRPC_EXPORT,
			GrpcReq: models.GrpcReq{
				Timestamp: grpcTimestamp,
			},
		},
		{
			Name: "http-test-2",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Timestamp: httpTimestamp,
			},
		},
	}

	r := &Replayer{logger: zap.NewNop()}
	backdate := r.backdate(testCases)

	if !backdate.Equal(grpcTimestamp) {
		t.Errorf("Expected backdate to be %v, got %v", grpcTimestamp, backdate)
	}

	if backdate.IsZero() {
		t.Error("Backdate should not be zero time for gRPC test case")
	}
}

func TestGetBackdateTimestamp_HTTPFirstTestCase(t *testing.T) {
	httpTimestamp := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	grpcTimestamp := time.Date(2024, 1, 1, 10, 1, 0, 0, time.UTC)

	testCases := []*models.TestCase{
		{
			Name: "http-test-1",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Timestamp: httpTimestamp,
			},
		},
		{
			Name: "grpc-test-2",
			Kind: models.GRPC_EXPORT,
			GrpcReq: models.GrpcReq{
				Timestamp: grpcTimestamp,
			},
		},
	}

	r := &Replayer{logger: zap.NewNop()}
	backdate := r.backdate(testCases)

	if !backdate.Equal(httpTimestamp) {
		t.Errorf("Expected backdate to be %v, got %v", httpTimestamp, backdate)
	}
}

func TestGetBackdateTimestamp_EmptyTestCases(t *testing.T) {
	testCases := []*models.TestCase{}

	r := &Replayer{logger: zap.NewNop()}
	backdate := r.backdate(testCases)

	if !backdate.IsZero() {
		t.Errorf("Expected zero time for empty test cases, got %v", backdate)
	}
}

func TestGetBackdateTimestamp_NilTestCase(t *testing.T) {
	testCases := []*models.TestCase{nil}

	r := &Replayer{logger: zap.NewNop()}
	backdate := r.backdate(testCases)

	if !backdate.IsZero() {
		t.Errorf("Expected zero time for nil test case, got %v", backdate)
	}
}
