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
	"go.keploy.io/server/v3/pkg/models"
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

func TestExtractHeaders_SafeTypeAssertions(t *testing.T) {
	cases := []struct {
		name    string
		headers []map[string]interface{}
		want    map[string]string
	}{
		{
			name: "valid string headers",
			headers: []map[string]interface{}{
				{"key": "Content-Type", "value": "application/json"},
				{"key": "Authorization", "value": "Bearer token"},
			},
			want: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer token",
			},
		},
		{
			name: "handles non-string or nil keys/values gracefully without panicking",
			headers: []map[string]interface{}{
				{"key": "Valid", "value": "ok"},
				{"key": nil, "value": "missing-key"},
				{"key": "MissingVal", "value": nil},
				{"key": 123, "value": "int-key"},
				{"key": "IntVal", "value": 456},
			},
			want: map[string]string{
				"Valid": "ok",
			},
		},
		{
			name:    "empty headers",
			headers: nil,
			want:    map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractHeaders(tc.headers)
			if len(got) != len(tc.want) {
				t.Fatalf("extractHeaders() returned %d items, want %d", len(got), len(tc.want))
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("extractHeaders()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestExtractURL_SafeTypeAssertions(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  string
	}{
		{
			name:  "plain string url",
			input: "https://api.example.com/v1",
			want:  "https://api.example.com/v1",
		},
		{
			name: "map with raw string",
			input: map[string]interface{}{
				"raw": "https://api.example.com/v2",
			},
			want: "https://api.example.com/v2",
		},
		{
			name: "map with non-string raw value",
			input: map[string]interface{}{
				"raw": 12345,
			},
			want: "",
		},
		{
			name: "map without raw key",
			input: map[string]interface{}{
				"host": "api.example.com",
			},
			want: "",
		},
		{
			name:  "unsupported type (int/nil)",
			input: 42,
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractURL(tc.input); got != tc.want {
				t.Errorf("extractURL(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestProcessUrlencodedBody_SafeTypeAssertions(t *testing.T) {
	cases := []struct {
		name string
		body []map[string]interface{}
		want string
	}{
		{
			name: "valid urlencoded items",
			body: []map[string]interface{}{
				{"key": "user", "value": "alice"},
				{"key": "role", "value": "admin"},
			},
			want: "user=alice&role=admin",
		},
		{
			name: "skips malformed items without panicking",
			body: []map[string]interface{}{
				{"key": "a", "value": "1"},
				{"key": "b", "value": nil},
				{"key": nil, "value": "2"},
				{"key": "c", "value": "3"},
			},
			want: "a=1&c=3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processUrlencodedBody(tc.body); got != tc.want {
				t.Errorf("processUrlencodedBody() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProcessFormdataBody_SafeTypeAssertions(t *testing.T) {
	cases := []struct {
		name string
		body []map[string]interface{}
		want []models.FormData
	}{
		{
			name: "valid formdata items",
			body: []map[string]interface{}{
				{"key": "field1", "value": "val1"},
				{"key": "field2", "value": "val2"},
			},
			want: []models.FormData{
				{Key: "field1", Values: []string{"val1"}},
				{Key: "field2", Values: []string{"val2"}},
			},
		},
		{
			name: "skips malformed items without panicking",
			body: []map[string]interface{}{
				{"key": "field1", "value": "val1"},
				{"key": nil, "value": "val2"},
				{"key": "field3", "value": nil},
			},
			want: []models.FormData{
				{Key: "field1", Values: []string{"val1"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := processFormdataBody(tc.body)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractGlobalVariables_SafeTypeAssertions(t *testing.T) {
	pi := NewPostmanImporter(context.Background(), zap.NewNop())

	vars := []map[string]interface{}{
		{"key": "baseUrl", "value": "https://api.example.com"},
		{"key": "apiVersion", "value": "v1"},
		{"key": "fullUrl", "value": "{{baseUrl}}/{{apiVersion}}"},
		{"key": "disabledVar", "value": "shouldBeSkipped", "disabled": true},
		{"key": "weirdDisabled", "value": "notABool", "disabled": "true"},
		{"key": nil, "value": "badKey"},
		{"key": "badValue", "value": 123},
	}

	got := pi.extractGlobalVariables(vars)
	if got["baseUrl"] != "https://api.example.com" {
		t.Errorf("baseUrl = %q, want https://api.example.com", got["baseUrl"])
	}
	if got["apiVersion"] != "v1" {
		t.Errorf("apiVersion = %q, want v1", got["apiVersion"])
	}
	if got["fullUrl"] != "https://api.example.com/v1" {
		t.Errorf("fullUrl = %q, want https://api.example.com/v1", got["fullUrl"])
	}
	if _, exists := got["disabledVar"]; exists {
		t.Errorf("disabledVar was not skipped")
	}
	if _, exists := got["badValue"]; exists {
		t.Errorf("badValue with non-string value should not be included")
	}
	if got["weirdDisabled"] != "notABool" {
		t.Errorf("weirdDisabled with non-bool disabled flag should be included")
	}
	if len(got) != 4 {
		t.Errorf("extractGlobalVariables() returned %d entries, want 4: %v", len(got), got)
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

func TestProcessFormdataBody_FileAndExportedEntries(t *testing.T) {
	var body []map[string]interface{}
	err := json.Unmarshal([]byte(`[
		{"key": "name", "value": "om", "type": "text"},
		{"key": "avatar", "type": "file", "src": "/tmp/avatar.png"},
		{"key": "docs", "type": "file", "src": ["a.pdf", "b.pdf"]},
		{"key": "tags", "values": ["x", "y"]},
		{"key": "empty_arr", "values": []},
		{"key": "mixed_arr", "values": ["x", 123, null, "y"]},
		{"key": "all_non_str", "values": [123, null]},
		{"key": "empty_str_arr", "values": [""]},
		{"key": "bad_values", "values": 42},
		{"key": "disabled_vals", "values": ["skip"], "disabled": true},
		{"key": "both_keys", "value": "", "values": ["a", "b"]},
		{"key": "fallback_to_value", "value": "only", "values": []}
	]`), &body)
	require.NoError(t, err)
	body = append(body, map[string]interface{}{
		"key":    "in_memory_slice",
		"values": []string{"m1", "m2"},
	})

	got := processFormdataBody(body)

	want := []models.FormData{
		{Key: "name", Values: []string{"om"}},
		{Key: "avatar", Paths: []string{"/tmp/avatar.png"}},
		{Key: "docs", Paths: []string{"a.pdf", "b.pdf"}},
		{Key: "tags", Values: []string{"x", "y"}},
		{Key: "mixed_arr", Values: []string{"x", "y"}},
		{Key: "empty_str_arr", Values: []string{""}},
		{Key: "both_keys", Values: []string{"a", "b"}},
		{Key: "fallback_to_value", Values: []string{"only"}},
		{Key: "in_memory_slice", Values: []string{"m1", "m2"}},
	}
	assert.Equal(t, want, got)
}

func TestProcessFormdataBody_HandlesFileAndText(t *testing.T) {
	tests := []struct {
		name  string
		input []map[string]interface{}
		want  []models.FormData
	}{
		{
			name: "text and file fields including omitted src",
			input: []map[string]interface{}{
				{"key": "username", "value": "aditya", "type": "text"},
				{"key": "role", "value": "admin"},
				{"key": "empty_val", "value": "", "type": "text"},
				{"key": "avatar", "type": "file", "src": "/path/to/avatar.png"},
				{"key": "document", "type": "file"},
			},
			want: []models.FormData{
				{Key: "username", Values: []string{"aditya"}},
				{Key: "role", Values: []string{"admin"}},
				{Key: "empty_val", Values: []string{""}},
				{Key: "avatar", Paths: []string{"/path/to/avatar.png"}},
				{Key: "document", Paths: nil},
			},
		},
		{
			name: "postman v2.1 multi-file src array filters empty and non-string entries",
			input: []map[string]interface{}{
				{
					"key":  "attachments",
					"type": "file",
					"src":  []interface{}{"files/a.pdf", "", 123, nil, "files/b.png"},
				},
				{
					"key":  "empty_array",
					"type": "file",
					"src":  []interface{}{"", 42},
				},
			},
			want: []models.FormData{
				{Key: "attachments", Paths: []string{"files/a.pdf", "files/b.png"}},
				{Key: "empty_array", Paths: nil},
			},
		},
		{
			name: "file fields with nil, empty, or non-string src omit empty paths",
			input: []map[string]interface{}{
				{"key": "nil_src", "type": "file", "src": nil},
				{"key": "empty_src", "type": "file", "src": ""},
				{"key": "int_src", "type": "file", "src": 99},
			},
			want: []models.FormData{
				{Key: "nil_src", Paths: nil},
				{Key: "empty_src", Paths: nil},
				{Key: "int_src", Paths: nil},
			},
		},
		{
			name: "skips disabled, malformed keys, and non-string values",
			input: []map[string]interface{}{
				{"key": "disabled_text", "value": "skip", "disabled": true},
				{"key": "disabled_file", "type": "file", "src": "a.bin", "disabled": true},
				{"key": "non_bool_disabled", "value": "keep", "disabled": "true"},
				{"key": "", "value": "empty-key"},
				{"key": nil, "value": "nil-key"},
				{"key": 123, "value": "int-key"},
				{"key": "nil_val", "value": nil},
				{"key": "int_val", "value": 456},
				{"key": "non_string_type", "type": 7, "value": "fallback-text"},
			},
			want: []models.FormData{
				{Key: "non_bool_disabled", Values: []string{"keep"}},
				{Key: "non_string_type", Values: []string{"fallback-text"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := processFormdataBody(tc.input)
			assert.Equal(t, tc.want, got)
		})
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
