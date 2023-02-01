package regression

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/pkg/models"
	mockPlatform "go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/service/testCase"
	"go.uber.org/zap"
)

const (
	defaultCompany = "default_company"
	defaultUser    = "default_user"
)

var (
	now            = time.Now()
	testReportPath string
	tcsPath        string
	mockPath       string
	err            error
	logger         *zap.Logger
	rSvc           *Regression
	testReportFS   TestReportFS
	mockFS         models.MockFS
)

var (
	grpcTcs = []models.TestCase{{
		ID:       "1",
		Created:  1674553692,
		Updated:  1674553692,
		Captured: 1674553692,
		CID:      defaultCompany,
		AppID:    "test-1",
		GrpcReq: models.GrpcReq{
			Body:   "Lorem Ipsum",
			Method: "services.Service.Add",
		},
		GrpcResp: models.GrpcResp{
			Body: `{"message":"Failed", "ts":1674553692}`,
			Err:  "nil",
		},
		Mocks: []*proto.Mock{
			{
				Version: string(models.V1Beta2),
				Name:    "mock-1",
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
	}}
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
					Name:    "mock-1",
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
)

func TestMain(m *testing.M) {
	logger, _ = zap.NewProduction()
	defer logger.Sync()

	testReportPath, err = os.Getwd()
	if err != nil {
		logger.Error("failed to get the current absolute path", zap.Error(err))
	}
	tcsPath = testReportPath + "/tests"
	mockPath = testReportPath + "/mocks"
	testReportPath += "/reports"

	mockFS = mockPlatform.NewMockExportFS(false)
	testReportFS = mockPlatform.NewTestReportFS(false)
	rSvc = New(nil, nil, testReportFS, nil, http.Client{}, logger, true, mockFS)
	m.Run()
}

func TestDeNoise(t *testing.T) {

	for _, tt := range []struct {
		input struct {
			id   string
			app  string
			body string
			h    http.Header
			path string
			kind models.Kind
			tcs  []models.TestCase
		}
		result error
	}{
		// http response contains a noisy field
		{
			input: struct {
				id   string
				app  string
				body string
				h    http.Header
				path string
				kind models.Kind
				tcs  []models.TestCase
			}{
				id:   "test-1",
				app:  "test-1",
				body: `{"message": "failed"}`,
				h: http.Header{
					"Pass": []string{"true"},
				},
				path: tcsPath,
				kind: models.HTTP,
				tcs:  httpTcs,
			},
			result: nil,
		},
		// grpc response contains a noisy field("body.ts")
		{
			input: struct {
				id   string
				app  string
				body string
				h    http.Header
				path string
				kind models.Kind
				tcs  []models.TestCase
			}{
				id:   "test-1",
				app:  "test-1",
				tcs:  grpcTcs,
				kind: models.GRPC_EXPORT,
				body: `{"message":"Failed", "ts":1674553123}`,
				path: tcsPath,
			},
			result: nil,
		},
		// error no tcs yaml exists to be denoised. This throws an error
		{
			input: struct {
				id   string
				app  string
				body string
				h    http.Header
				path string
				kind models.Kind
				tcs  []models.TestCase
			}{
				id:   "test-1",
				app:  "test-1",
				tcs:  []models.TestCase{},
				kind: models.GRPC_EXPORT,
				body: `{"message":"Failed", "ts":1674553123}`,
				path: tcsPath,
			},
			result: errors.New("open " + tcsPath + "/test-1.yaml: no such file or directory"),
		},
	} {
		// setup. Write the tcs yaml which is to be tested
		tcSvc := testCase.New(nil, logger, false, nil, http.Client{}, true, mockFS)
		tcSvc.Insert(context.Background(), tt.input.tcs, tcsPath, mockPath, defaultCompany)

		// update the tcs yaml with noised fields
		ctx := context.Background()
		ctx = context.WithValue(ctx, "reqType", tt.input.kind)
		actErr := rSvc.DeNoise(ctx, defaultCompany, tt.input.id, tt.input.app, tt.input.body, tt.input.h, tt.input.path)
		if (actErr == nil && tt.result != nil) || (actErr != nil && tt.result == nil) || (actErr != nil && tt.result != nil && actErr.Error() != tt.result.Error()) {
			t.Fatal("Actual output from DeNoise does not matches with expected.", "Expected", tt.result, "Actual", actErr)
		}

		tearDown()
	}
}

func TestTestGrpc(t *testing.T) {
	for _, tt := range []struct {
		input struct {
			startRun models.TestRun
			runId    string
			totalTcs int
			tcs      []models.TestCase
			resp     models.GrpcResp
			stopRun  models.TestRun
		}
		result struct {
			startTestOutput struct {
				err error
			}
			testOutput struct {
				pass bool
				err  error
			}
			stopTestOutput struct {
				err error
			}
		}
	}{
		// reponse matches the tccs yaml grpc response.
		{
			input: struct {
				startRun models.TestRun
				runId    string

				totalTcs int
				tcs      []models.TestCase
				resp     models.GrpcResp
				stopRun  models.TestRun
			}{
				startRun: models.TestRun{
					ID:      "2a6b4382-176d-4c06-921e-36ce6bc0ecb1",
					Status:  models.TestRunStatusRunning,
					Created: now.Unix(),
					Updated: now.Unix(),
					CID:     defaultCompany,
					App:     "sample",
					User:    defaultUser,
					Total:   1,
				},
				runId:    "2a6b4382-176d-4c06-921e-36ce6bc0ecb1",
				totalTcs: 1,
				tcs:      grpcTcs,
				resp: models.GrpcResp{
					Body: `{"message":"Failed", "ts":1674553692}`,
					Err:  "nil",
				},
				stopRun: models.TestRun{
					ID:      "2a6b4382-176d-4c06-921e-36ce6bc0ecb1",
					Updated: now.Unix(),
					Status:  models.TestRunStatusPassed,
				},
			},
			result: struct {
				startTestOutput struct{ err error }
				testOutput      struct {
					pass bool
					err  error
				}
				stopTestOutput struct {
					err error
				}
			}{
				startTestOutput: struct{ err error }{
					err: nil,
				},
				testOutput: struct {
					pass bool
					err  error
				}{
					pass: true,
					err:  nil,
				},
				stopTestOutput: struct{ err error }{
					err: nil,
				},
			},
		},
		// response do not matches with tcs yaml grpc response
		{
			input: struct {
				startRun models.TestRun
				runId    string

				totalTcs int
				tcs      []models.TestCase
				resp     models.GrpcResp
				stopRun  models.TestRun
			}{
				startRun: models.TestRun{
					ID:      "3a6b4382-176d-4c06-921e-36ce6bc0ecb2",
					Status:  models.TestRunStatusRunning,
					Created: now.Unix(),
					Updated: now.Unix(),
					CID:     defaultCompany,
					App:     "sample-1",
					User:    defaultUser,
					Total:   1,
				},
				runId:    "3a6b4382-176d-4c06-921e-36ce6bc0ecb2",
				totalTcs: 1,
				tcs:      grpcTcs,
				resp: models.GrpcResp{
					Body: `{"message":"Failed", "ts":1674553699}`,
					Err:  "nil",
				},
				stopRun: models.TestRun{
					ID:      "3a6b4382-176d-4c06-921e-36ce6bc0ecb2",
					Updated: now.Unix(),
					Status:  models.TestRunStatusFailed,
				},
			},
			result: struct {
				startTestOutput struct{ err error }
				testOutput      struct {
					pass bool
					err  error
				}
				stopTestOutput struct {
					err error
				}
			}{
				startTestOutput: struct{ err error }{
					err: nil,
				},
				testOutput: struct {
					pass bool
					err  error
				}{
					pass: false,
					err:  nil,
				},
				stopTestOutput: struct{ err error }{
					err: nil,
				},
			},
		},
	} {
		// setup. Write the tcs yaml which is to be tested
		tcSvc := testCase.New(nil, logger, false, nil, http.Client{}, true, mockFS)
		tcSvc.Insert(context.Background(), tt.input.tcs, tcsPath, mockPath, defaultCompany)

		// Start Testrun
		actErr := rSvc.PutTest(context.Background(), tt.input.startRun, true, tt.input.runId, tcsPath, mockPath, testReportPath, tt.input.totalTcs)
		if (actErr == nil && tt.result.startTestOutput.err != nil) || (actErr != nil && tt.result.startTestOutput.err == nil) || (actErr != nil && tt.result.startTestOutput.err != nil && actErr.Error() != tt.result.startTestOutput.err.Error()) {
			t.Fatal("failed at startTest", "Expected", tt.result.startTestOutput.err, "Actual", actErr)
		}

		// Test the actual grpc response with stored response in tcs yaml
		actPass, actErr := rSvc.TestGrpc(context.Background(), tt.input.resp, defaultCompany, tt.input.startRun.App, tt.input.runId, "test-1", tcsPath, mockPath)
		if actPass != tt.result.testOutput.pass {
			t.Fatal("output from TestGrpc does not matches", "Expected", tt.result.testOutput.pass, "Actual", actPass)
		}
		if (actErr == nil && tt.result.testOutput.err != nil) || (actErr != nil && tt.result.testOutput.err == nil) || (actErr != nil && tt.result.testOutput.err != nil && actErr.Error() != tt.result.testOutput.err.Error()) {
			t.Fatal("failed at TestGrpc", "Expected", tt.result.testOutput.err, "Actual", actErr)
		}

		// End Testrun with test summary
		actErr = rSvc.PutTest(context.Background(), tt.input.stopRun, true, tt.input.runId, tcsPath, mockPath, testReportPath, tt.input.totalTcs)
		if (actErr == nil && tt.result.stopTestOutput.err != nil) || (actErr != nil && tt.result.stopTestOutput.err == nil) || (actErr != nil && tt.result.stopTestOutput.err != nil && actErr.Error() != tt.result.stopTestOutput.err.Error()) {
			t.Fatal("failed at stopTest", "Expected", tt.result.stopTestOutput.err, "Actual", actErr)
		}

		tearDown()
	}
}

func TestTest(t *testing.T) {
	for _, tt := range []struct {
		input struct {
			startRun models.TestRun
			runId    string

			totalTcs int
			tcs      []models.TestCase
			resp     models.HttpResp
			stopRun  models.TestRun
		}
		result struct {
			startTestOutput struct {
				err error
			}
			testOutput struct {
				pass bool
				err  error
			}
			stopTestOutput struct {
				err error
			}
		}
	}{
		{
			input: struct {
				startRun models.TestRun
				runId    string

				totalTcs int
				tcs      []models.TestCase
				resp     models.HttpResp
				stopRun  models.TestRun
			}{
				startRun: models.TestRun{
					ID:      "2a6b4382-176d-4c06-921e-36ce6bc0ecb1",
					Status:  models.TestRunStatusRunning,
					Created: now.Unix(),
					Updated: now.Unix(),
					CID:     defaultCompany,
					App:     "sample",
					User:    defaultUser,
					Total:   1,
				},
				runId:    "2a6b4382-176d-4c06-921e-36ce6bc0ecb1",
				totalTcs: 1,
				tcs:      httpTcs,
				resp: models.HttpResp{
					StatusCode: 200,
					Header: http.Header{
						"Pass": []string{"true"},
					},
					Body: `{"message": "passed"}`,
				},
				stopRun: models.TestRun{
					ID:      "2a6b4382-176d-4c06-921e-36ce6bc0ecb1",
					Updated: now.Unix(),
					Status:  models.TestRunStatusPassed,
				},
			},
			result: struct {
				startTestOutput struct{ err error }
				testOutput      struct {
					pass bool
					err  error
				}
				stopTestOutput struct {
					err error
				}
			}{
				startTestOutput: struct{ err error }{
					err: nil,
				},
				testOutput: struct {
					pass bool
					err  error
				}{
					pass: true,
					err:  nil,
				},
				stopTestOutput: struct{ err error }{
					err: nil,
				},
			},
		},
		{
			input: struct {
				startRun models.TestRun
				runId    string

				totalTcs int
				tcs      []models.TestCase
				resp     models.HttpResp
				stopRun  models.TestRun
			}{
				startRun: models.TestRun{
					ID:      "3a6b4382-176d-4c06-921e-36ce6bc0ecb2",
					Status:  models.TestRunStatusRunning,
					Created: now.Unix(),
					Updated: now.Unix(),
					CID:     defaultCompany,
					App:     "sample-1",
					User:    defaultUser,
					Total:   1,
				},
				runId:    "3a6b4382-176d-4c06-921e-36ce6bc0ecb2",
				totalTcs: 1,
				tcs:      httpTcs,
				resp: models.HttpResp{
					StatusCode: 200,
					Header: http.Header{
						"Pass": []string{"false"},
					},
					Body: `{"message": "failed"}`,
				},
				stopRun: models.TestRun{
					ID:      "3a6b4382-176d-4c06-921e-36ce6bc0ecb2",
					Updated: now.Unix(),
					Status:  models.TestRunStatusFailed,
				},
			},
			result: struct {
				startTestOutput struct{ err error }
				testOutput      struct {
					pass bool
					err  error
				}
				stopTestOutput struct {
					err error
				}
			}{
				startTestOutput: struct{ err error }{
					err: nil,
				},
				testOutput: struct {
					pass bool
					err  error
				}{
					pass: false,
					err:  nil,
				},
				stopTestOutput: struct{ err error }{
					err: nil,
				},
			},
		},
	} {
		// setup. Write the tcs yaml which is to be tested
		tcSvc := testCase.New(nil, logger, false, nil, http.Client{}, true, mockFS)
		tcSvc.Insert(context.Background(), tt.input.tcs, tcsPath, mockPath, defaultCompany)

		// Start Testrun
		actErr := rSvc.PutTest(context.Background(), tt.input.startRun, true, tt.input.runId, tcsPath, mockPath, testReportPath, tt.input.totalTcs)
		if (actErr == nil && tt.result.startTestOutput.err != nil) || (actErr != nil && tt.result.startTestOutput.err == nil) || (actErr != nil && tt.result.startTestOutput.err != nil && actErr.Error() != tt.result.startTestOutput.err.Error()) {
			t.Fatal("failed at startTest", "Expected", tt.result.startTestOutput.err, "Actual", actErr)
		}

		// Test the actual http response with stored response in tcs yaml
		actPass, actErr := rSvc.Test(context.Background(), defaultCompany, tt.input.startRun.App, tt.input.runId, "test-1", tcsPath, mockPath, tt.input.resp)
		if actPass != tt.result.testOutput.pass {
			t.Fatal("output from TestGrpc does not matches", "Expected", tt.result.testOutput.pass, "Actual", actPass)
		}
		if (actErr == nil && tt.result.testOutput.err != nil) || (actErr != nil && tt.result.testOutput.err == nil) || (actErr != nil && tt.result.testOutput.err != nil && actErr.Error() != tt.result.testOutput.err.Error()) {
			t.Fatal("failed at TestGrpc", "Expected", tt.result.testOutput.err, "Actual", actErr)
		}

		// End Testrun with test summary
		actErr = rSvc.PutTest(context.Background(), tt.input.stopRun, true, tt.input.runId, tcsPath, mockPath, testReportPath, tt.input.totalTcs)
		if (actErr == nil && tt.result.stopTestOutput.err != nil) || (actErr != nil && tt.result.stopTestOutput.err == nil) || (actErr != nil && tt.result.stopTestOutput.err != nil && actErr.Error() != tt.result.stopTestOutput.err.Error()) {
			t.Fatal("failed at stopTest", "Expected", tt.result.stopTestOutput.err, "Actual", actErr)
		}

		tearDown()
	}
}

func tearDown() {
	if _, err := os.ReadDir("tests"); err == nil {
		os.RemoveAll("tests")
	}
	if _, err := os.ReadDir("mocks"); err == nil {
		os.RemoveAll("mocks")
	}
	if _, err := os.ReadDir("reports"); err == nil {
		os.RemoveAll("reports")
	}
}
