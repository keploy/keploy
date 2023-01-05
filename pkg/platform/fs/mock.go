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
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
)

type mockExport struct {
	isTestMode bool
	tests      sync.Map
}

func NewMockExportFS(isTestMode bool) *mockExport {
	return &mockExport{
		isTestMode: isTestMode,
		tests:      sync.Map{},
	}
}

func (fe *mockExport) Exists(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(filepath.Join(path)); err != nil {
		return false, err
	}
	return true, nil
}

func (fe *mockExport) ReadAll(ctx context.Context, testCasePath, mockPath string) ([]models.TestCase, error) {
	if !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		return nil, fmt.Errorf("file path should be absolute. got testcase path: %s and mock path: %s", pkg.SanitiseInput(testCasePath), pkg.SanitiseInput(mockPath))
	}
	dir, err := os.OpenFile(testCasePath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("failed to open the directory containing testcases yaml files. path: %s  error: %s", pkg.SanitiseInput(testCasePath), err.Error())
	}

	var (
		res = []models.TestCase{}
	)
	files, err := dir.ReadDir(0)
	if err != nil {
		return nil, fmt.Errorf("failed to read the names of testcases yaml files from path directory. path: %s  error: %s", pkg.SanitiseInput(testCasePath), err.Error())
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		tcs, err := read(testCasePath, name, false)
		if err != nil {
			return nil, err
		}

		tests, err := toTestCase(tcs, name, mockPath)
		if err != nil {
			return nil, err
		}
		res = append(res, tests...)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Captured < res[j].Captured
	})

	return res, nil
}

func (fe *mockExport) Read(ctx context.Context, path, name string, libMode bool) ([]models.Mock, error) {
	return read(path, name, libMode)
}

func (fe *mockExport) Write(ctx context.Context, path string, doc models.Mock) error {
	if fe.isTestMode {
		return nil
	}
	isFileEmpty, err := createMockFile(path, doc.Name)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(path, doc.Name+".yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to open the file. error: %v", err.Error())
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to embed document into yaml file. error: %s", err.Error())
	}
	defer file.Close()
	return nil
}

func (fe *mockExport) WriteAll(ctx context.Context, path, fileName string, docs []models.Mock) error {
	if fe.isTestMode {
		return nil
	}
	_, err := createMockFile(path, fileName)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(path, fileName+".yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to open the file. error: %s", err.Error())
	}

	for i, j := range docs {
		data := []byte("---\n")
		if i == 0 {
			data = []byte{}
		}
		d, err := yaml.Marshal(j)
		if err != nil {
			return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
		}
		data = append(data, d...)

		_, err = file.Write(data)
		if err != nil {
			return fmt.Errorf("failed to embed document into yaml file. error: %s", err.Error())
		}
	}

	defer file.Close()
	return nil
}

func toTestCase(tcs []models.Mock, fileName, mockPath string) ([]models.TestCase, error) {
	res := []models.TestCase{}
	for _, j := range tcs {
		spec := models.HttpSpec{}
		err := j.Spec.Decode(&spec)
		if err != nil {
			return res, fmt.Errorf("failed to decode the yaml spec field of testcase. file: %s  error: %s", pkg.SanitiseInput(fileName), err.Error())
		}
		nameCheck := strings.Split(spec.Mocks[0], "-")[0]
		var mockName string;
		if(nameCheck == "mock"){
			mockName = "mock-" + strings.Split(fileName, "-")[1]
		} else {
			mockName = fileName
		}
		mocks, _ := read(mockPath, mockName, false)

		// TODO: what to log when the testcase dont have any mocks. Either the testcase don't have a mock or it have but keploy is unable to read the mock yaml

		noise, ok := spec.Assertions["noise"]
		if !ok {
			noise = []string{}
		}
		doc, err := grpcMock.Decode(mocks)
		if err != nil {
			return res, err
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
				Form:       spec.Request.Form,
			},
			HttpResp: models.HttpResp{
				StatusCode: spec.Response.StatusCode,
				Header:     grpcMock.ToHttpHeader(spec.Response.Header),
				Body:       spec.Response.Body,
				Binary:     spec.Response.Binary,
			},
			Noise:    noise,
			Mocks:    doc,
			Captured: spec.Created,
		})
	}
	return res, nil
}

func read(path, name string, libMode bool) ([]models.Mock, error) {
	if !pkg.IsValidPath(path) {
		return nil, fmt.Errorf("file path should be absolute. got path: %s", pkg.SanitiseInput(path))
	}
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	arr := []models.Mock{}
	for {
		var doc models.Mock
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		if !libMode || doc.Name == name {
			arr = append(arr, doc)
		}
	}
	return arr, nil
}

func createMockFile(path string, fileName string) (bool, error) {
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
