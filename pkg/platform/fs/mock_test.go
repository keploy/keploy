package fs

import (
	"context"
	"fmt"
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"testing"
)

/* ------------------- MOCKING AREA ---------------------- */

// FakeYamlHandler struct will handle real IO implementation, so we don't actually
// need to interact with a real file system, which is slow, lead to unexpect
// behaviour and can mess with important files.
type FakeYamlHandler struct {
	WriteErr   error
	ReadErr    error
	DirEntries []fs.DirEntry
	ReadDirErr error
}

func (f FakeYamlHandler) Write(path string, obj interface{}) error {
	return f.WriteErr
}

func (f FakeYamlHandler) Read(path string, obj interface{}) error {
	// Check if path is /home/test/ or /home/mock/. This is important because
	// if path is /home/mock/ it means the caller wants to deserialize a mock
	// file, so we use MockYaml raw string to be deserialized into obj.
	switch filepath.Dir(path) {

	case "/home/test":
		err := yaml.Unmarshal([]byte(TestYaml), obj)
		if err != nil {
			log.Fatal("Error while unmarshalling test yaml")
		}

		// Must return end of file error so it stops adding yaml bytes at obj
		return io.EOF

	case "/home/mock":
		err := yaml.Unmarshal([]byte(MockYaml), obj)
		if err != nil {
			log.Fatal("Error while unmarshalling mock yaml")
		}

		return io.EOF
	}

	return f.WriteErr
}

func (f FakeYamlHandler) ReadDir(path string) ([]os.DirEntry, error) {
	return f.DirEntries, f.ReadDirErr
}

// Here we will mock FakeDirEntry with the aim of handle the Name() function
// of it so when a caller wants to see which yaml files are inside a given path
// we don't need to create real files. This will be important to test the
// mockExport.ReadAll() function because it needs to get a list of tests inside
// the folder.
type FakeDirEntry struct{}

func (d FakeDirEntry) Name() string {
	return "test-1.yaml"
}
func (d FakeDirEntry) IsDir() bool {
	return false
}
func (d FakeDirEntry) Type() fs.FileMode {
	return 0
}
func (d FakeDirEntry) Info() (fs.FileInfo, error) {
	return nil, nil
}

func TestMockFS(t *testing.T) {

	y := FakeYamlHandler{
		nil, nil, []fs.DirEntry{FakeDirEntry{}}, nil,
	}
	mFS := NewMockExportFS(false, &y)

	for _, tt := range []struct {
		input struct {
			tcPath   string
			mockPath string
			name     string
			objects  []models.Mock
		}
		result struct {
			err      error
			dirEntry []os.DirEntry
			tcs      []models.TestCase
		}
	}{{

		input: struct {
			tcPath   string
			mockPath string
			name     string
			objects  []models.Mock
		}{tcPath: "/home/test", mockPath: "/home/mock", name: "mock-1", objects: []models.Mock{
			{
				Version: "1",
				Kind:    "Http",
				Name:    "mock-1",
				Spec:    yaml.Node{}},
		}}, result: struct {
			err      error
			dirEntry []os.DirEntry
			tcs      []models.TestCase
		}{err: nil, tcs: []models.TestCase{}},
	}} {
		res := mFS.Write(context.Background(), tt.input.tcPath, tt.input.objects[0])
		if res != tt.result.err {
			t.Fatal(fmt.Sprintf("Not suppose to get "+
				"error on mockExport.Write. Expect: %v Actual %v", tt.result.err, res))
		}

		res = mFS.WriteAll(context.Background(), tt.input.tcPath, tt.input.name, tt.input.objects)
		if res != tt.result.err {
			t.Fatal(fmt.Sprintf("Not suppose to get "+
				"error on mockExport.WriteAll. Expect: %v Actual %v", tt.result.err, res))
		}

		// This will of course not read from the real FS instead we've mocked the inners calls for
		// the YamlHandler so it use the TestYaml and MockYaml raw string below
		tcs, err := mFS.ReadAll(context.Background(), tt.input.tcPath, tt.input.mockPath, "")
		if err != tt.result.err {
			t.Fatal(fmt.Sprintf("Not suppose to get "+
				"error on mockExport.ReadAll. Expect: %v Actual %v", tt.result.err, res))
		}

		// For future PR's we can think in not just check if it's not nil but also if its equal with
		// a real tcs model that we should expect by converting the raw yaml.
		if tcs == nil {
			t.Fatal(fmt.Sprintf("Not suppose to tcs be nil "))
		}
	}

}

// Copied from test-1.yaml
const TestYaml = `
---
version: api.keploy.io/v1beta2
kind: Http
name: test-1
spec:
    metadata: {}
    req:
        method: POST
        proto_major: 1
        proto_minor: 1
        url: /api/regression/testcase
        header:
            Accept-Encoding: gzip
            Content-Length: "1667"
            Content-Type: application/json
            User-Agent: Go-http-client/1.1
        body: '{"captured":1674553625,"app_id":"grpc-nested-app","uri":"","http_req":{"method":"","proto_major":0,"proto_minor":0,"url":"","url_params":null,"header":null,"body":"","binary":"","form":null},"http_resp":{"status_code":0,"header":null,"body":"","status_message":"","proto_major":0,"proto_minor":0,"binary":""},"grpc_req":{"body":"{\"x\":1,\"y\":23}","method":"api.Adder.Add"},"grpc_resp":{"body":"{\"result\":81,\"data\":{\"name\":\"Fabio Di Gentanio\",\"team\":{\"name\":\"Ducati\",\"championships\":\"0\",\"points\":\"1001\"}}}","error":""},"deps":[{"name":"mongodb","type":"NO_SQL_DB","meta":{"InsertOneOptions":"[]","document":"x:1  y:23","name":"mongodb","operation":"InsertOne","type":"NO_SQL_DB"},"data":["LP+BAwEBD0luc2VydE9uZVJlc3VsdAH/ggABAQEKSW5zZXJ0ZWRJRAEQAAAAT/+CATNnby5tb25nb2RiLm9yZy9tb25nby1kcml2ZXIvYnNvbi9wcmltaXRpdmUuT2JqZWN0SUT/gwEBAQhPYmplY3RJRAH/hAABBgEYAAAY/4QUAAxj/8//qRkRZP/hI//V//IO/9sA","Cv+FBQEC/4gAAAAF/4YAAQE="]}],"test_case_path":"/Users/ritikjain/Desktop/go-practice/skp-workspace/go/grpc-example-app/keploy/tests","mock_path":"/Users/ritikjain/Desktop/go-practice/skp-workspace/go/grpc-example-app/keploy/mocks","mocks":[{"Version":"api.keploy.io/v1beta2","Kind":"Generic","Spec":{"Metadata":{"InsertOneOptions":"[]","document":"x:1  y:23","name":"mongodb","operation":"InsertOne","type":"NO_SQL_DB"},"Objects":[{"Type":"*mongo.InsertOneResult","Data":"LP+BAwEBD0luc2VydE9uZVJlc3VsdAH/ggABAQEKSW5zZXJ0ZWRJRAEQAAAAT/+CATNnby5tb25nb2RiLm9yZy9tb25nby1kcml2ZXIvYnNvbi9wcmltaXRpdmUuT2JqZWN0SUT/gwEBAQhPYmplY3RJRAH/hAABBgEYAAAY/4QUAAxj/8//qRkRZP/hI//V//IO/9sA"},{"Type":"*keploy.KError","Data":"Cv+FBQEC/4gAAAAF/4YAAQE="}]}}],"type":"gRPC"}'
        body_type: utf-8
    resp:
        status_code: 200
        header:
            Content-Type: application/json; charset=utf-8
            Vary: Origin
        body: |
            {"id":"415e534e-10f5-488b-a781-19a4bede11b5"}
        body_type: utf-8
        status_message: ""
        proto_major: 1
        proto_minor: 1
    objects:
        - type: error
          data: H4sIAAAAAAAA/wEAAP//AAAAAAAAAAA=
    mocks:
        - mock-1-0
    assertions:
        noise:
            - header.Content-Type
            - body.id
    created: 1674553625
`

const MockYaml = `
---
version: api.keploy.io/v1beta2
kind: Generic
name: mock-1-0
spec:
    metadata:
        UpdateOptions: '[{<nil> <nil> <nil> <nil> 0x140005779c4}]'
        filter: map[_id:415e534e-10f5-488b-a781-19a4bede11b5]
        name: mongodb
        operation: UpdateOne
        type: NO_SQL_DB
        update: '[{$set {415e534e-10f5-488b-a781-19a4bede11b5 1674553625 1674553625 1674553625 default_company grpc-nested-app  { 0 0  map[] map[]   []} {0 map[]   0 0 } {{"x":1,"y":23} api.Adder.Add} {{"result":81,"data":{"name":"Fabio Di Gentanio","team":{"name":"Ducati","championships":"0","points":"1001"}}} } [{mongodb NO_SQL_DB map[InsertOneOptions:[] document:x:1  y:23 name:mongodb operation:InsertOne type:NO_SQL_DB] [[44 255 129 3 1 1 15 73 110 115 101 114 116 79 110 101 82 101 115 117 108 116 1 255 130 0 1 1 1 10 73 110 115 101 114 116 101 100 73 68 1 16 0 0 0 79 255 130 1 51 103 111 46 109 111 110 103 111 100 98 46 111 114 103 47 109 111 110 103 111 45 100 114 105 118 101 114 47 98 115 111 110 47 112 114 105 109 105 116 105 118 101 46 79 98 106 101 99 116 73 68 255 131 1 1 1 8 79 98 106 101 99 116 73 68 1 255 132 0 1 6 1 24 0 0 24 255 132 20 0 12 99 255 207 255 169 25 17 100 255 225 35 255 213 255 242 14 255 219 0] [10 255 133 5 1 2 255 136 0 0 0 5 255 134 0 1 1]]}] map[] map[] [] [] gRPC}}]'
    objects:
        - type: '*mongo.UpdateResult'
          data: H4sIAAAAAAAA/4r738jMyMgTWpCSWJIalFpcmlPC+L+JgZGFkcc3sSQ5IzXFOb80r4SRhYGR1zc/JTMtE1kktKA4tagESYQLJuLpwijAwMBg+r+JmYmRrbikKDMvnUeNQcXE0DTV1NgkVdfQIM1U18TCIkk30dzCUNfQMtEkKTUl1dAwyZQBEAAA//+Ly36olQAAAA==
        - type: '*keploy.KError'
          data: H4sIAAAAAAAA/+L638zKyPS/jYGBgfV/CwMjIyAAAP//7jjoexEAAAA=

`
