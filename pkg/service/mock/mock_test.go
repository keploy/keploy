package mock

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/pkg/models"
	mockPlatform "go.keploy.io/server/pkg/platform/fs"
	"go.uber.org/zap"
)

var (
	logger  *zap.Logger
	path    string
	mockSrv *Mock
	err     error
)

func TestMain(m *testing.M) {
	logger, _ = zap.NewProduction()
	defer logger.Sync()
	path, err = os.Getwd()
	if err != nil {
		logger.Error("failed to get the current absolute path", zap.Error(err))
	}
	path += "/mocks"

	mockFS := mockPlatform.NewMockExportFS(false)
	mockSrv = NewMockService(mockFS, logger)
	m.Run()
	tearDown()
}

func TestService(t *testing.T) {

	for _, tt := range []struct {
		input struct {
			doc        *proto.Mock
			meta       interface{}
			name       string
			appendDocs []*proto.Mock
			remove     []string
			replace    map[string]string
		}
		result struct {
			putErr    error
			getAllErr error
			existsErr error
			FinalErr  []error
		}
	}{
		// 1. Write a mock of SQL.
		// 2. Update the writen output in yaml to new output from appendDocs array
		{
			input: struct {
				doc        *proto.Mock
				meta       interface{}
				name       string
				appendDocs []*proto.Mock
				remove     []string
				replace    map[string]string
			}{
				name: "mock-1",
				doc: &proto.Mock{
					Version: string(models.V1Beta2),
					Name:    "mock-1",
					Kind:    string(models.SQL),
					Spec: &proto.Mock_SpecSchema{
						Metadata: map[string]string{
							"name":      "SQL",
							"operation": "QueryContext.Close",
							"type":      "SQL_DB",
						},
						Type: "table",
						Table: &proto.Table{
							Cols: []*proto.SqlCol{
								{
									Name:      "total",
									Type:      "int64",
									Precision: 0,
									Scale:     0,
								},
								{
									Name:      "id",
									Type:      "int64",
									Precision: 0,
									Scale:     0,
								},
							},
							Rows: []string{"[`5` | `1` | ]"},
						},
						Int: 0,
						Err: []string{"nil", "nil"},
					},
				},
				meta: map[string]string{
					"name":      "SQL",
					"operation": "QueryContext.Close",
					"type":      "SQL_DB",
				},
				appendDocs: []*proto.Mock{
					{
						Version: string(models.V1Beta2),
						Name:    "mock-1",
						Kind:    string(models.SQL),
						Spec: &proto.Mock_SpecSchema{
							Metadata: map[string]string{
								"name":      "SQL",
								"operation": "QueryContext.Close",
								"type":      "SQL_DB",
							},
							Type: "table",
							Table: &proto.Table{
								Cols: []*proto.SqlCol{
									{
										Name:      "total",
										Type:      "int64",
										Precision: 0,
										Scale:     0,
									},
									{
										Name:      "id",
										Type:      "int64",
										Precision: 0,
										Scale:     0,
									},
								},
								Rows: []string{"[`2` | `1` | ]"},
							},
							Int: 0,
							Err: []string{"nil", "nil"},
						},
					},
				},
			},
			result: struct {
				putErr    error
				getAllErr error
				existsErr error
				FinalErr  []error
			}{
				putErr:    nil,
				getAllErr: nil,
				existsErr: nil,
				FinalErr:  []error{nil},
			},
		},
		// Attempt to write mock of invalid kind which fails with expected error
		{
			input: struct {
				doc        *proto.Mock
				meta       interface{}
				name       string
				appendDocs []*proto.Mock
				remove     []string
				replace    map[string]string
			}{
				name: "mock-2",
				doc: &proto.Mock{
					Version: string(models.V1Beta2),
					Name:    "mock-2",
					Kind:    "Invalid",
				},
				appendDocs: []*proto.Mock{
					{
						Version: string(models.V1Beta2),
						Name:    "mock-2",
						Kind:    "Invalid",
					},
				},
			},
			result: struct {
				putErr    error
				getAllErr error
				existsErr error
				FinalErr  []error
			}{
				putErr:    errors.New("mock with name mock-2 is not of a valid kind"),
				getAllErr: errors.New("open " + path + "/mock-2.yaml: no such file or directory"),
				existsErr: nil,
				FinalErr:  []error{errors.New("mock with name mock-2 is not of a valid kind")},
			},
		},
		// 1. Writes mock of kind Http with binary request/response body (valid utf-8 encoded)
		// 2. Adds a SQL mock at 0th position in mock-3.yaml file. This runs insertAt function
		{
			input: struct {
				doc        *proto.Mock
				meta       interface{}
				name       string
				appendDocs []*proto.Mock
				remove     []string
				replace    map[string]string
			}{
				name: "mock-3",
				doc: &proto.Mock{
					Version: string(models.V1Beta2),
					Name:    "mock-3",
					Kind:    string(models.HTTP),
					Spec: &proto.Mock_SpecSchema{
						Metadata: map[string]string{
							"type":   "HTTP",
							"method": "POST",
						},
						Req: &proto.HttpReq{
							Method:     "POST",
							ProtoMajor: 0,
							ProtoMinor: 0,
							URL:        "https://youtube.com/url",
							BodyData:   []byte("sample request data"),
							Body:       "sample request data",
							Header: map[string]*proto.StrArr{
								"Accept":         {Value: []string{"*/*"}},
								"Content-Length": {Value: []string{"28"}},
								"Content-Type":   {Value: []string{"application/json"}},
							},
						},
						Res: &proto.HttpResp{
							StatusCode: 200,
							Header: map[string]*proto.StrArr{
								"Connection":     {Value: []string{"Close"}},
								"Content-Length": {Value: []string{"16"}},
								"Content-Type":   {Value: []string{"application/json"}},
							},
							BodyData: []byte(`{"message": "passed"}`),
							Body:     `{"message": "passed"}`,
						},
						Created: time.Now().Unix(),
						Objects: []*proto.Mock_Object{},
					},
				},
				remove: []string{"all.header.Content-Type", "invalid_format"},
				replace: map[string]string{
					"header.Accept": "all",
					"domain":        "google.com",
					"method":        "PATCH",
					"proto_major":   "0",
					"proto_minor":   "0",
					"header":        "Invalid_format", // format should header.<key>
				},
				appendDocs: []*proto.Mock{
					{
						Version: string(models.V1Beta2),
						Name:    "mock-3",
						Kind:    string(models.SQL),
						Spec: &proto.Mock_SpecSchema{
							Metadata: map[string]string{
								"name":      "SQL",
								"operation": "QueryContext.Close",
								"type":      "SQL_DB",
							},
							Type: "table",
							Table: &proto.Table{
								Cols: []*proto.SqlCol{
									{
										Name:      "total",
										Type:      "int64",
										Precision: 0,
										Scale:     0,
									},
									{
										Name:      "id",
										Type:      "int64",
										Precision: 0,
										Scale:     0,
									},
								},
								Rows: []string{"[`5` | `1` | ]"},
							},
							Int: 0,
							Err: []string{"nil", "nil"},
						},
					},
					{
						Version: string(models.V1Beta2),
						Name:    "mock-3",
						Kind:    string(models.HTTP),
						Spec: &proto.Mock_SpecSchema{
							Metadata: map[string]string{
								"type":   "HTTP",
								"method": "POST",
							},
							Req: &proto.HttpReq{
								Method:     "POST",
								ProtoMajor: 0,
								ProtoMinor: 0,
								URL:        "/url",
								BodyData:   []byte("sample request data"),
								Body:       "sample request data",
								Header: map[string]*proto.StrArr{
									"Accept": {Value: []string{"*/*"}},
								},
							},
							Res: &proto.HttpResp{
								StatusCode: 200,
								Header: map[string]*proto.StrArr{
									"Connection": {Value: []string{"Close"}},
								},
								BodyData: []byte(`{"message": "passed"}`),
								Body:     `{"message": "passed"}`,
							},
							Created: time.Now().Unix(),
							Objects: []*proto.Mock_Object{},
						},
					},
				},
			},
			result: struct {
				putErr    error
				getAllErr error
				existsErr error
				FinalErr  []error
			}{
				putErr:   nil,
				FinalErr: []error{nil, nil},
			},
		},
		// 1. Write yaml of kind http which contains binary request-response body(not in utf-8 encoded)
		// 2. Attempts to rewrite the same mock again. But the mock-4.yaml is not edited.
		{
			input: struct {
				doc        *proto.Mock
				meta       interface{}
				name       string
				appendDocs []*proto.Mock
				remove     []string
				replace    map[string]string
			}{
				name: "mock-4",
				doc: &proto.Mock{
					Version: string(models.V1Beta2),
					Name:    "mock-4",
					Kind:    string(models.HTTP),
					Spec: &proto.Mock_SpecSchema{
						Metadata: map[string]string{
							"type":   "HTTP",
							"method": "POST",
						},
						Req: &proto.HttpReq{
							Method:     "POST",
							ProtoMajor: 0,
							ProtoMinor: 0,
							URL:        "/url",
							BodyData:   []byte{0x80, 0x81, 0x82, 0x83},
							Header: map[string]*proto.StrArr{
								"Accept":  {Value: []string{"*/*"}},
								"Connect": {Value: []string{"alive"}},
							},
						},
						Res: &proto.HttpResp{
							StatusCode: 200,
							Header: map[string]*proto.StrArr{
								"Connection": {Value: []string{"Close"}},
							},
							BodyData: []byte{0x80, 0x81, 0x82, 0x83},
						},
						Created: time.Now().Unix(),
						Objects: []*proto.Mock_Object{},
						Assertions: map[string]*proto.StrArr{
							"noise": {Value: []string{"header.Connect"}},
						},
					},
				},
				appendDocs: []*proto.Mock{
					{
						Version: string(models.V1Beta2),
						Name:    "mock-4",
						Kind:    string(models.HTTP),
						Spec: &proto.Mock_SpecSchema{
							Metadata: map[string]string{
								"type":   "HTTP",
								"method": "POST",
							},
							Req: &proto.HttpReq{
								Method:     "POST",
								ProtoMajor: 0,
								ProtoMinor: 0,
								URL:        "/url",
								BodyData:   []byte{0x80, 0x81, 0x82, 0x83},
								Header: map[string]*proto.StrArr{
									"Accept":  {Value: []string{"*/*"}},
									"Connect": {Value: []string{"alive"}},
								},
							},
							Res: &proto.HttpResp{
								StatusCode: 200,
								Header: map[string]*proto.StrArr{
									"Connection": {Value: []string{"Close"}},
								},
								BodyData: []byte{0x80, 0x81, 0x82, 0x83},
							},
							Created: time.Now().Unix(),
							Objects: []*proto.Mock_Object{},
							Assertions: map[string]*proto.StrArr{
								"noise": {Value: []string{"header.Connect"}},
							},
						},
					},
				},
			},
			result: struct {
				putErr    error
				getAllErr error
				existsErr error
				FinalErr  []error
			}{
				putErr:   nil,
				FinalErr: []error{nil},
			},
		},
		// To delete unwanted mocks from yaml docs. This testcase will run
		// trimMocks function for mock-3.yaml
		{
			input: struct {
				doc        *proto.Mock
				meta       interface{}
				name       string
				appendDocs []*proto.Mock
				remove     []string
				replace    map[string]string
			}{
				name: "mock-3",
				appendDocs: []*proto.Mock{
					{
						Version: string(models.V1Beta2),
						Name:    "mock-3",
						Kind:    string(models.HTTP),
						Spec: &proto.Mock_SpecSchema{
							Metadata: map[string]string{
								"type":   "HTTP",
								"method": "POST",
							},
							Req: &proto.HttpReq{
								Method:     "POST",
								ProtoMajor: 0,
								ProtoMinor: 0,
								URL:        "/url",
								BodyData:   []byte("sample request data"),
								Body:       "sample request data",
								Header: map[string]*proto.StrArr{
									"Accept": {Value: []string{"*/*"}},
								},
							},
							Res: &proto.HttpResp{
								StatusCode: 200,
								Header: map[string]*proto.StrArr{
									"Connection": {Value: []string{"Close"}},
								},
								BodyData: []byte(`{"message": "passed"}`),
								Body:     `{"message": "passed"}`,
							},
							Created: time.Now().Unix(),
							Objects: []*proto.Mock_Object{},
						},
					},
				},
			},
			result: struct {
				putErr    error
				getAllErr error
				existsErr error
				FinalErr  []error
			}{
				putErr:   nil,
				FinalErr: []error{nil},
			},
		},
	} {
		var actErr error
		if tt.input.doc != nil {
			actErr = mockSrv.Put(context.Background(), path, tt.input.doc, tt.input.meta, tt.input.remove, tt.input.replace)
			if (actErr == nil && tt.result.putErr != nil) || (actErr != nil && tt.result.putErr == nil) || (actErr != nil && tt.result.putErr != nil && actErr.Error() != tt.result.putErr.Error()) {
				t.Fatal("test failed at Put", "Expected error", tt.result.putErr, "Actual error", actErr)
			}
		}

		_, actErr = mockSrv.GetAll(context.Background(), path, tt.input.name)
		if (actErr == nil && tt.result.getAllErr != nil) || (actErr != nil && tt.result.getAllErr == nil) || (actErr != nil && tt.result.getAllErr != nil && actErr.Error() != tt.result.getAllErr.Error()) {
			t.Fatal("test failed at GetAll", "Expected error", tt.result.getAllErr, "Actual error", actErr)
		}

		_, actErr = mockSrv.FileExists(context.Background(), path+"/"+tt.input.name+".yaml", true)
		if (actErr == nil && tt.result.existsErr != nil) || (actErr != nil && tt.result.existsErr == nil) || (actErr != nil && tt.result.existsErr != nil && actErr.Error() != tt.result.existsErr.Error()) {
			t.Fatal("test failed at FileExists", "Expected error", tt.result.getAllErr, "Actual error", actErr)
		}
		for i, v := range tt.input.appendDocs {
			actErr = mockSrv.Put(context.Background(), path, v, tt.input.meta, []string{}, map[string]string{})
			if (actErr == nil && tt.result.FinalErr[i] != nil) || (actErr != nil && tt.result.FinalErr[i] == nil) || (actErr != nil && tt.result.FinalErr[i] != nil && actErr.Error() != tt.result.FinalErr[i].Error()) {
				t.Fatal("test failed at Put after FileExists", "Expected error", tt.result.putErr, "Actual error", actErr)
			}
		}
	}
}

func tearDown() {
	if _, err := os.ReadDir("mocks"); err == nil {
		os.RemoveAll("mocks")
	}
}
