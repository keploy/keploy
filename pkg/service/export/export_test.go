package export

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	postmanimport "go.keploy.io/server/v3/pkg/service/import"
)

// postmanItem is one exported request as Postman reads it. URL and body stay
// raw so a case can state the exact JSON a user gets.
type postmanItem struct {
	Name    string `json:"name"`
	Request struct {
		Method string              `json:"method"`
		Header []map[string]string `json:"header"`
		Body   json.RawMessage     `json:"body"`
		URL    json.RawMessage     `json:"url"`
	} `json:"request"`
}

func TestConvertKeployHTTPToPostmanCollection(t *testing.T) {
	jsonContent := map[string]string{"Content-Type": "application/json"}

	tests := []struct {
		name        string
		http        models.HTTPSchema
		wantName    string
		wantMethod  string
		wantURL     string
		wantHeaders []map[string]string
		wantBody    string
	}{
		{
			name: "POST with a JSON body, two headers and query params",
			http: models.HTTPSchema{
				Request: models.HTTPReq{
					Method:    models.Method("POST"),
					URL:       "http://localhost:8080/api/users?source=web&page=2",
					URLParams: map[string]string{"source": "web", "page": "2"},
					Header:    map[string]string{"Content-Type": "application/json", "Accept": "*/*"},
					Body:      `{"name":"alice"}`,
				},
				Response: models.HTTPResp{StatusCode: 201, StatusMessage: "Created", Header: jsonContent, Body: `{"id":1}`},
			},
			wantName:   "api-users",
			wantMethod: "POST",
			wantURL: `{
				"raw": "http://localhost:8080/api/users?source=web&page=2",
				"protocol": "http",
				"host": ["localhost"],
				"port": "8080",
				"path": ["api/users"],
				"query": {"source": "web", "page": "2"}
			}`,
			wantHeaders: []map[string]string{
				{"key": "Content-Type", "value": "application/json"},
				{"key": "Accept", "value": "*/*"},
			},
			wantBody: `{
				"mode": "raw",
				"raw": "{\"name\":\"alice\"}",
				"urlencoded": null,
				"formdata": null,
				"options": {"raw": {"headerFamily": "json", "language": "json"}}
			}`,
		},
		{
			name: "GET with no body",
			http: models.HTTPSchema{
				Request: models.HTTPReq{
					Method: models.Method("GET"),
					URL:    "https://api.example.com/v1/users/1",
					Header: map[string]string{"Accept": "application/json"},
				},
				Response: models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Header: jsonContent, Body: `{"id":1}`},
			},
			wantName:   "v1-users-1",
			wantMethod: "GET",
			wantURL: `{
				"raw": "https://api.example.com/v1/users/1",
				"protocol": "https",
				"host": ["api.example.com"],
				"port": "",
				"path": ["v1/users/1"],
				"query": null
			}`,
			wantHeaders: []map[string]string{
				{"key": "Accept", "value": "application/json"},
			},
			// The body block is always written; for a GET it carries no content.
			// The JSON options come from the response's Content-Type.
			wantBody: `{
				"mode": "raw",
				"raw": "",
				"urlencoded": null,
				"formdata": null,
				"options": {"raw": {"headerFamily": "json", "language": "json"}}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := ConvertKeployHTTPToPostmanCollection(zap.NewNop(), &tt.http)
			require.NotNil(t, item)

			data, err := json.Marshal(item)
			require.NoError(t, err)
			var got postmanItem
			require.NoError(t, json.Unmarshal(data, &got))

			assert.Equal(t, tt.wantName, got.Name)
			assert.Equal(t, tt.wantMethod, got.Request.Method)
			assert.JSONEq(t, tt.wantURL, string(got.Request.URL))
			assert.ElementsMatch(t, tt.wantHeaders, got.Request.Header)
			assert.JSONEq(t, tt.wantBody, string(got.Request.Body))
		})
	}
}

// exportedFolder is one test set in the exported collection: its name and the
// method of each request in it, keyed by request name.
type exportedFolder struct {
	name     string
	requests map[string]string
}

func TestExport(t *testing.T) {
	testSet0 := exportedFolder{
		name: "test-set-0",
		// test-1.yaml and test-2.json
		requests: map[string]string{"api-users": "POST", "api-users-1": "GET"},
	}

	tests := []struct {
		name        string
		extraLayout func(t *testing.T, keployDir string)
		want        []exportedFolder
	}{
		{
			name: "one YAML and one JSON test case",
			want: []exportedFolder{testSet0},
		},
		{
			name: "a test set without a tests directory is skipped",
			extraLayout: func(t *testing.T, keployDir string) {
				require.NoError(t, os.Mkdir(filepath.Join(keployDir, "test-set-1"), 0o755))
				copyTestCase(t, keployDir, "test-set-2")
			},
			want: []exportedFolder{
				testSet0,
				{name: "test-set-2", requests: map[string]string{"api-users": "POST"}},
			},
		},
		{
			name: "the reports directory is ignored",
			extraLayout: func(t *testing.T, keployDir string) {
				copyTestCase(t, keployDir, "reports")
			},
			want: []exportedFolder{testSet0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			// testdata holds the test sets; keploy record writes them under keploy/.
			require.NoError(t, os.CopyFS(filepath.Join(dir, "keploy"), os.DirFS("testdata")))
			if tt.extraLayout != nil {
				tt.extraLayout(t, filepath.Join(dir, "keploy"))
			}
			t.Chdir(dir)

			require.NoError(t, Export(t.Context(), zap.NewNop()))

			data, err := os.ReadFile(filepath.Join(dir, "output.json"))
			require.NoError(t, err)
			var got postmanimport.PostmanCollectionStruct
			require.NoError(t, json.Unmarshal(data, &got))

			assert.Equal(t, filepath.Base(dir), got.Info.Name)
			assert.Equal(t, "https://schema.getpostman.com/json/collection/v2.0.0/collection.json", got.Info.Schema)
			assert.NotEmpty(t, got.Info.PostmanID)
			assert.Empty(t, got.Items.TestDataItems, "requests sit inside a folder per test set")

			folders := make([]exportedFolder, 0, len(got.Items.PostmanItems))
			for _, folder := range got.Items.PostmanItems {
				requests := map[string]string{}
				for _, item := range folder.Item {
					requests[item.Name] = item.Request.Method
				}
				folders = append(folders, exportedFolder{name: folder.Name, requests: requests})
			}
			assert.Equal(t, tt.want, folders)
		})
	}
}

func TestExport_WithoutKeployDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	err := Export(t.Context(), zap.NewNop())

	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.NoFileExists(t, filepath.Join(dir, "output.json"))
}

// copyTestCase places the testdata YAML test case in keployDir/<folder>/tests.
func copyTestCase(t *testing.T, keployDir, folder string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(keployDir, "test-set-0", "tests", "test-1.yaml"))
	require.NoError(t, err)
	testsDir := filepath.Join(keployDir, folder, "tests")
	require.NoError(t, os.MkdirAll(testsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(testsDir, "test-1.yaml"), data, 0o644))
}
