package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	// "runtime"
	"sort"
	"strings"
	"sync"

	grpcMock "go.keploy.io/server/grpc/mock"
	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
)

type mockExport struct {
	isTestMode  bool
	tests       sync.Map
	yamlHandler models.YamlHandler
}

func NewMockExportFS(isTestMode bool, yamlHandler models.YamlHandler) *mockExport {
	return &mockExport{
		isTestMode:  isTestMode,
		tests:       sync.Map{},
		yamlHandler: yamlHandler,
	}
}

func (fe *mockExport) Exists(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path)); err != nil {
		return false
	}
	return true
}

func (fe *mockExport) ReadAll(ctx context.Context, testCasePath, mockPath, tcsType string) ([]models.TestCase, error) {
	if !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		return nil, fmt.Errorf("file path should be absolute. got testcase path: %s and mock path: %s", pkg.SanitiseInput(testCasePath), pkg.SanitiseInput(mockPath))
	}

	files, err := fe.yamlHandler.ReadDir(testCasePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open the directory containing testcases yaml files. path: %s  error: %s", pkg.SanitiseInput(testCasePath), err.Error())
	}

	var res []models.TestCase
	for _, j := range files {
		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))

		tcs, err := read(fe.yamlHandler, testCasePath, name, false)

		if err != nil {
			return nil, err
		}

		tests, err := fe.toTestCase(tcs, name, mockPath)
		if err != nil {
			return nil, err
		}
		res = append(res, tests...)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Captured < res[j].Captured
	})

	if tcsType != "" {
		filteredTcs := reqTypeFilter(res, tcsType)
		res = filteredTcs
	}

	return res, nil
}

func (fe *mockExport) Read(ctx context.Context, path, name string, libMode bool) ([]models.Mock, error) {
	return read(fe.yamlHandler, path, name, libMode)
}

func (fe *mockExport) Write(ctx context.Context, path string, doc models.Mock) error {
	if fe.isTestMode {
		return nil
	}

	path = filepath.Join(path, doc.Name)

	err := fe.yamlHandler.Write(path, doc)
	if err != nil {
		return fmt.Errorf("failed to embed document into yaml file. error: %s", err.Error())
	}

	return nil
}

func (fe *mockExport) WriteAll(ctx context.Context, path, fileName string, docs []models.Mock) error {
	if fe.isTestMode {
		return nil
	}

	for _, j := range docs {
		path = filepath.Join(path, fileName)
		err := fe.yamlHandler.Write(path, j)

		if err != nil {
			return fmt.Errorf("failed to embed document into yaml file. error: %s", err.Error())
		}
	}

	return nil
}

func (fe *mockExport) toTestCase(tcs []models.Mock, fileName, mockPath string) ([]models.TestCase, error) {
	res := []models.TestCase{}
	for _, j := range tcs {
		var (
			// spec  = models.HttpSpec{}
			mocks = []*proto.Mock{}
		)

		switch j.Kind {
		case models.HTTP:
			spec := models.HttpSpec{}
			err := j.Spec.Decode(&spec)
			if err != nil {
				return res, fmt.Errorf("failed to decode the yaml spec field of testcase. file: %s  error: %s", pkg.SanitiseInput(fileName), err.Error())
			}

			noise, ok := spec.Assertions["noise"]
			if !ok {
				noise = []string{}
			}
			if len(spec.Mocks) > 0 {
				nameCheck := strings.Split(spec.Mocks[0], "-")[0]
				var mockName string
				if nameCheck == "mock" {
					mockName = "mock-" + strings.Split(fileName, "-")[1]
				} else {
					mockName = fileName
				}
				yamlDocs, err := read(fe.yamlHandler, mockPath, mockName, false)
				if err != nil {
					return nil, err
				}
				mocks, err = grpcMock.Decode(yamlDocs)
				if err != nil {
					return nil, err
				}
			}
			res = append(res, models.TestCase{
				ID: j.Name,
				HttpReq: models.HttpReq{
					Method:     spec.Request.Method,
					ProtoMajor: spec.Request.ProtoMajor,
					ProtoMinor: spec.Request.ProtoMinor,
					URL:        spec.Request.URL,
					Header:     grpcMock.ToHttpHeader(spec.Request.Header),
					Body:       spec.Request.Body,
					URLParams:  spec.Request.URLParams,
				},
				HttpResp: models.HttpResp{
					StatusCode: spec.Response.StatusCode,
					Header:     grpcMock.ToHttpHeader(spec.Response.Header),
					Body:       spec.Response.Body,
				},
				Noise:    noise,
				Mocks:    mocks,
				Captured: spec.Created,
				Type:     string(models.HTTP),
			})
		case models.GRPC_EXPORT:
			spec := models.GrpcSpec{}
			err := j.Spec.Decode(&spec)
			if err != nil {
				return res, fmt.Errorf("failed to decode the yaml spec field of testcase. file: %s  error: %s", pkg.SanitiseInput(fileName), err.Error())
			}

			noise, ok := spec.Assertions["noise"]
			if !ok {
				noise = []string{}
			}
			if len(spec.Mocks) > 0 {
				nameCheck := strings.Split(spec.Mocks[0], "-")[0]
				var mockName string
				if nameCheck == "mock" {
					mockName = "mock-" + strings.Split(fileName, "-")[1]
				} else {
					mockName = fileName
				}
				yamlDocs, err := read(fe.yamlHandler, mockPath, mockName, false)
				if err != nil {
					return nil, err
				}
				mocks, err = grpcMock.Decode(yamlDocs)
				if err != nil {
					return nil, err
				}
			}
			res = append(res, models.TestCase{
				ID: j.Name,
				// GrpcReq:    spec.Request.Body,
				GrpcReq: spec.Request,
				// GrpcMethod: spec.Request.Method,
				GrpcResp: spec.Response,
				Noise:    noise,
				Mocks:    mocks,
				Captured: spec.Created,
				Type:     string(models.GRPC_EXPORT),
			})
		default:
			return res, fmt.Errorf("failed to decode the yaml. file: %s  error: Invalid kind of yaml", pkg.SanitiseInput(fileName))
		}
	}
	return res, nil
}

func read(yamlHandler models.YamlHandler, path, name string, libMode bool) ([]models.Mock, error) {
	if !pkg.IsValidPath(path) {
		return nil, fmt.Errorf("file path should be absolute. got path: %s", pkg.SanitiseInput(path))
	}

	var arr []models.Mock
	for {
		var doc models.Mock
		err := yamlHandler.Read(filepath.Join(path, name), &doc)

		if doc.Name == name {
			arr = append(arr, doc)
		}

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}
	}
	return arr, nil
}

func reqTypeFilter(tcs []models.TestCase, reqType string) []models.TestCase {
	var result []models.TestCase
	for i := 0; i < len(tcs); i++ {
		if tcs[i].Type == reqType {
			result = append(result, tcs[i])
		}
	}
	return result
}

func CreateMockFile(path string, fileName string) (bool, error) {
	if !pkg.IsValidPath(path) {
		return false, fmt.Errorf("file path should be absolute. got path: %s", pkg.SanitiseInput(path))
	}
	if _, err := os.Stat(filepath.Join(path, fileName+".yaml")); err != nil {
		err := os.MkdirAll(filepath.Join(path), os.ModePerm)
		if err != nil {
			return false, fmt.Errorf("failed to create a mock dir. error: %v", err.Error())
		}
		_, err = os.Create(filepath.Join(path, fileName+".yaml"))
		if err != nil {
			return false, fmt.Errorf("failed to create a yaml file. error: %v", err.Error())
		}
		return true, nil
	}
	return false, nil
}
