package postmanimport

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
)

func TestItemsContainerUnmarshal_DoesNotTreatFoldersAsRequests(t *testing.T) {
	data := []byte(`[
		{
			"name": "Users",
			"item": [
				{
					"name": "List users",
					"request": {
						"method": "GET",
						"header": [],
						"url": "https://api.example.test/users"
					},
					"response": []
				}
			]
		}
	]`)

	var got ItemsContainer
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("UnmarshalJSON returned error: %v", err)
	}

	if len(got.PostmanItems) != 1 {
		t.Fatalf("PostmanItems length = %d, want 1", len(got.PostmanItems))
	}
	if got.PostmanItems[0].Name != "Users" {
		t.Fatalf("folder name = %q, want Users", got.PostmanItems[0].Name)
	}
	if len(got.TestDataItems) != 0 {
		t.Fatalf("TestDataItems length = %d, want 0; folder was classified as a request", len(got.TestDataItems))
	}
}

func TestItemsContainerUnmarshal_KeepsRootRequests(t *testing.T) {
	data := []byte(`[
		{
			"name": "Health check",
			"request": {
				"method": "GET",
				"header": [],
				"url": "https://api.example.test/health"
			},
			"response": []
		}
	]`)

	var got ItemsContainer
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("UnmarshalJSON returned error: %v", err)
	}

	if len(got.PostmanItems) != 0 {
		t.Fatalf("PostmanItems length = %d, want 0", len(got.PostmanItems))
	}
	if len(got.TestDataItems) != 1 {
		t.Fatalf("TestDataItems length = %d, want 1", len(got.TestDataItems))
	}
	if got.TestDataItems[0].Name != "Health check" {
		t.Fatalf("request name = %q, want Health check", got.TestDataItems[0].Name)
	}
}

// An imported test case is written only inside the recording: a tests folder
// that links out is refused, with why, and nothing is written there.
func TestAnImportedTestIsWrittenOnlyInsideTheRecording(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	testsPath := filepath.Join(root, "set", "tests")
	if err := os.MkdirAll(filepath.Dir(testsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "outside"), testsPath); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	pi := NewPostmanImporter(context.Background(), zap.NewNop())
	item := TestData{Name: "x", Request: PostmanRequest{Method: "GET", URL: "http://127.0.0.1:9/x"}, Response: []PostmanResponse{{}}}
	counter := 0
	err := pi.writeTestData(item, root, testsPath, map[string]string{}, &counter)
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("writeTestData: %v, want it refused as outside the recording", err)
	}
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
		t.Fatalf("the import left %v (%v) outside the recording", entries, err)
	}
}

func TestProcessUrlencodedBody_PreservesFieldBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		body     []map[string]interface{}
		wantBody string
		want     url.Values
	}{
		{
			name: "reserved characters in values stay inside their own field",
			body: []map[string]interface{}{
				{"key": "name", "value": "A&B+C"},
				{"key": "email", "value": "dev+test@example.com"},
			},
			wantBody: "name=A%26B%2BC&email=dev%2Btest%40example.com",
			want: url.Values{
				"name":  {"A&B+C"},
				"email": {"dev+test@example.com"},
			},
		},
		{
			name: "reserved characters in keys stay inside their own field",
			body: []map[string]interface{}{
				{"key": "a&b=c", "value": "1#2"},
			},
			wantBody: "a%26b%3Dc=1%232",
			want: url.Values{
				"a&b=c": {"1#2"},
			},
		},
		{
			name: "plain fields are left readable",
			body: []map[string]interface{}{
				{"key": "name", "value": "Alice"},
			},
			wantBody: "name=Alice",
			want:     url.Values{"name": {"Alice"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processUrlencodedBody(tt.body)
			assert.Equal(t, tt.wantBody, got)

			parsed, err := url.ParseQuery(got)
			require.NoError(t, err)
			assert.Equal(t, tt.want, parsed)
		})
	}
}

func TestConstructRequest_UrlencodedBodyReachesTheTestcaseEncoded(t *testing.T) {
	req := &PostmanRequest{
		Method: "POST",
		URL:    "https://api.example.test/form",
		Body: PostmanRequestBody{
			Mode: "urlencoded",
			Urlencoded: []map[string]interface{}{
				{"key": "name", "value": "A&B+C"},
				{"key": "email", "value": "dev+test@example.com"},
			},
		},
	}

	got := constructRequest(req, nil)

	assert.Equal(t, "name=A%26B%2BC&email=dev%2Btest%40example.com", got.Body)
	parsed, err := url.ParseQuery(got.Body)
	require.NoError(t, err)
	assert.Equal(t, url.Values{
		"name":  {"A&B+C"},
		"email": {"dev+test@example.com"},
	}, parsed)
}

func TestReplaceTemplateVarsResolvesPostmanVariableNames(t *testing.T) {
	tests := []struct {
		name  string
		input string
		vars  map[string]string
		want  string
	}{
		{
			name:  "hyphenated key",
			input: "{{base-url}}/users",
			vars:  map[string]string{"base-url": "https://example.test"},
			want:  "https://example.test/users",
		},
		{
			name:  "dotted key",
			input: "{{api.base}}/users",
			vars:  map[string]string{"api.base": "https://example.test"},
			want:  "https://example.test/users",
		},
		{
			name:  "underscore key",
			input: "{{base_url}}/users",
			vars:  map[string]string{"base_url": "https://example.test"},
			want:  "https://example.test/users",
		},
		{
			name:  "padded hyphenated key",
			input: "{{ base-url }}/users",
			vars:  map[string]string{"base-url": "https://example.test"},
			want:  "https://example.test/users",
		},
		{
			name:  "unknown hyphenated key stays verbatim",
			input: "{{base-url}}/users",
			vars:  map[string]string{"other-key": "https://example.test"},
			want:  "{{base-url}}/users",
		},
		{
			name:  "name with a space stays verbatim",
			input: "{{base url}}/users",
			vars:  map[string]string{"base url": "https://example.test"},
			want:  "{{base url}}/users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceTemplateVars(tt.input, tt.vars)
			if got != tt.want {
				t.Fatalf("replaceTemplateVars(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConstructRequestResolvesHyphenatedVariableInURLAndCurl(t *testing.T) {
	req := &PostmanRequest{
		Method: "GET",
		URL:    map[string]interface{}{"raw": "{{base-url}}/users"},
	}

	got := constructRequest(req, map[string]string{"base-url": "https://example.test"})

	if got.URL != "https://example.test/users" {
		t.Fatalf("URL = %q, want %q", got.URL, "https://example.test/users")
	}

	curl := pkg.MakeCurlCommand(got)
	if strings.Contains(curl, "{{base-url}}") {
		t.Fatalf("curl command still carries the unresolved placeholder: %q", curl)
	}
	if !strings.Contains(curl, "https://example.test/users") {
		t.Fatalf("curl command does not carry the resolved url: %q", curl)
	}
}
