package testCase

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/pkg/models"
	mockPlatform "go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.uber.org/zap"
)

const defaultCompany = "default_company"

var (
	tcsPath  string
	mockPath string
	err      error
	logger   *zap.Logger
	tcSvc    *TestCase
)

var (
	httpTcs = []models.TestCase{
		{
			ID:       "1",
			Created:  1674553692,
			Updated:  1674553692,
			Captured: 1674553692,
			CID:      defaultCompany,
			AppID:    "test-1",
			URI:      "/url",
			HttpReq: models.HttpReq{
				Method:     "GET",
				ProtoMajor: 0,
				ProtoMinor: 0,
				URL:        "/url",
			},
			HttpResp: models.HttpResp{
				StatusCode: 200,
				Header: http.Header{
					"Pass": []string{"true"},
				},
				Body: `{"message": "passed"}`,
			},
			Mocks: []*proto.Mock{
				{
					Version: string(models.V1Beta2),
					Name:    "mock-2",
					Kind:    string(models.GENERIC),
					Spec: &proto.Mock_SpecSchema{
						Metadata: map[string]string{
							"operation": "find",
						},
						Objects: []*proto.Mock_Object{
							{
								Type: "error",
								Data: []byte("123"),
							},
						},
					},
				},
			},
			Type: string(models.HTTP),
		},
	}
	grpcTcs = []models.TestCase{
		{
			ID:       "1",
			Created:  1674553692,
			Updated:  1674553692,
			Captured: 1674553692,
			CID:      defaultCompany,
			AppID:    "test-2",
			GrpcReq: models.GrpcReq{
				Body:   "Lorem Ipsum",
				Method: "services.Service.Add",
			},
			GrpcResp: models.GrpcResp{
				Body: "success",
				Err:  "nil",
			},
			Mocks: []*proto.Mock{
				{
					Version: string(models.V1Beta2),
					Name:    "mock-2",
					Kind:    string(models.GENERIC),
					Spec: &proto.Mock_SpecSchema{
						Metadata: make(map[string]string),
						Objects: []*proto.Mock_Object{
							{
								Type: "error",
								Data: []byte("123"),
							},
						},
					},
				},
			},
			Type: string(models.GRPC_EXPORT),
		},
	}
)

func TestMain(m *testing.M) {
	logger, _ = zap.NewProduction()
	defer logger.Sync()
	tcsPath, err = os.Getwd()
	if err != nil {
		logger.Error("failed to get the current absolute path", zap.Error(err))
	}
	mockPath = tcsPath + "/mocks"
	tcsPath += "/tests"

	mockFS := mockPlatform.NewMockExportFS(false)
	analyticsConfig := telemetry.NewTelemetry(nil, false, false, true, nil, logger)
	tcSvc = New(nil, logger, false, analyticsConfig, http.Client{}, true, mockFS)

	m.Run()
}

func TestInsert(t *testing.T) {

	for _, tt := range []struct {
		input struct {
			testCasePath string
			mockPath     string
			t            []models.TestCase
		}
		result struct {
			id  []string
			err error
		}
	}{
		//keys and values matches
		{
			input: struct {
				testCasePath string
				mockPath     string
				t            []models.TestCase
			}{
				testCasePath: "",
				mockPath:     "",
				t:            httpTcs,
			},
			result: struct {
				id  []string
				err error
			}{
				id:  nil,
				err: errors.New("path directory not found. got testcase path:  and mock path: "),
			},
		},
		{
			input: struct {
				testCasePath string
				mockPath     string
				t            []models.TestCase
			}{
				testCasePath: tcsPath,
				mockPath:     mockPath,
				t:            httpTcs,
			},
			result: struct {
				id  []string
				err error
			}{
				id:  []string{"test-1"},
				err: nil,
			},
		},
		{
			input: struct {
				testCasePath string
				mockPath     string
				t            []models.TestCase
			}{
				testCasePath: tcsPath,
				mockPath:     mockPath,
				t:            grpcTcs,
			},
			result: struct {
				id  []string
				err error
			}{
				id:  []string{"test-2"},
				err: nil,
			},
		},
	} {
		actId, actErr := tcSvc.Insert(context.Background(), tt.input.t, tt.input.testCasePath, tt.input.mockPath, defaultCompany, []string{}, map[string]string{})
		if (actErr == nil && tt.result.err != nil) || (actErr != nil && tt.result.err == nil) || (actErr != nil && tt.result.err != nil && actErr.Error() != tt.result.err.Error()) {
			t.Fatal("Err from Insert does not matches", "Expected", tt.result.err, "Actual", actErr)
		}
		if len(tt.result.id) != len(actId) {
			t.Fatal("length of the actual ids is not equal with expected", "Expected ids", tt.result.id, "Actual ids", actId)
		}
		tearDown(tt.input.t[0].ID)
	}
}

func TestGetAll(t *testing.T) {
	for _, tt := range []struct {
		input struct {
			testCasePath string
			mockPath     string
		}
		result struct {
			tcs []models.TestCase
			err error
		}
	}{
		{
			input: struct {
				testCasePath string
				mockPath     string
			}{
				testCasePath: tcsPath,
				mockPath:     mockPath,
			},
			result: struct {
				tcs []models.TestCase
				err error
			}{
				tcs: []models.TestCase{
					{
						ID:       "1",
						Created:  1674553692,
						Updated:  1674553692,
						Captured: 1674553692,
						CID:      defaultCompany,
						AppID:    "test-1",
						URI:      "/url",
						HttpReq: models.HttpReq{
							Method:     "GET",
							ProtoMajor: 0,
							ProtoMinor: 0,
							URL:        "/url",
						},
						HttpResp: models.HttpResp{
							StatusCode: 200,
							Header: http.Header{
								"Pass": []string{"true"},
							},
							Body: `{"message": "passed"}`,
						},
						Mocks: []*proto.Mock{
							{
								Version: string(models.V1Beta2),
								Name:    "mock-2",
								Kind:    string(models.GENERIC),
								Spec: &proto.Mock_SpecSchema{
									Metadata: map[string]string{
										"operation": "find",
									},
									Objects: []*proto.Mock_Object{
										{
											Type: "error",
											Data: []byte("123"),
										},
									},
								},
							},
						},
						Type: string(models.HTTP),
					},
				},
			},
		},
		{
			input: struct {
				testCasePath string
				mockPath     string
			}{
				testCasePath: tcsPath,
				mockPath:     mockPath,
			},
			result: struct {
				tcs []models.TestCase
				err error
			}{
				tcs: []models.TestCase{
					{
						ID:       "1",
						Created:  1674553692,
						Updated:  1674553692,
						Captured: 1674553692,
						CID:      defaultCompany,
						AppID:    "test-2",
						GrpcReq: models.GrpcReq{
							Body:   "Lorem Ipsum",
							Method: "services.Service.Add",
						},
						GrpcResp: models.GrpcResp{
							Body: "success",
							Err:  "nil",
						},
						Mocks: []*proto.Mock{
							{
								Version: string(models.V1Beta2),
								Name:    "mock-2",
								Kind:    string(models.GENERIC),
								Spec: &proto.Mock_SpecSchema{
									Metadata: map[string]string{
										"operation": "find",
									},
									Objects: []*proto.Mock_Object{
										{
											Type: "error",
											Data: []byte("123"),
										},
									},
								},
							},
						},
						Type: string(models.GRPC_EXPORT),
					},
				},
			},
		},
	} {
		tcSvc.Insert(context.Background(), tt.result.tcs, tt.input.testCasePath, tt.input.mockPath, defaultCompany, []string{}, map[string]string{})

		actTcs, actErr := tcSvc.GetAll(context.Background(), defaultCompany, "", nil, nil, tt.input.testCasePath, tt.input.mockPath)
		if (actErr == nil && tt.result.err != nil) || (actErr != nil && tt.result.err == nil) || (actErr != nil && tt.result.err != nil && actErr.Error() != tt.result.err.Error()) {
			t.Fatal("Err from GetAll does not matches", "Expected", tt.result.err, "Actual", actErr)
		}
		if len(tt.result.tcs) != len(actTcs) {
			t.Fatal("length of the actual ids is not equal with expected", "Expected ids", tt.result.tcs, "Actual ids", actTcs)
		}
		tearDown(tt.result.tcs[0].ID)
	}
}

func tearDown(tid string) {
	if _, err := os.ReadDir("tests"); err == nil {
		os.RemoveAll("tests")
	}
	if _, err := os.ReadDir("mocks"); err == nil {
		os.RemoveAll("mocks")
	}
}
