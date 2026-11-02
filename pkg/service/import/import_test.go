package postmanimport

import (
	"context"
	"encoding/json"
	"testing"

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
		want int
	}{
		{
			name: "valid formdata items",
			body: []map[string]interface{}{
				{"key": "field1", "value": "val1"},
				{"key": "field2", "value": "val2"},
			},
			want: 2,
		},
		{
			name: "skips malformed items without panicking",
			body: []map[string]interface{}{
				{"key": "field1", "value": "val1"},
				{"key": nil, "value": "val2"},
				{"key": "field3", "value": nil},
			},
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := processFormdataBody(tc.body)
			if len(got) != tc.want {
				t.Fatalf("processFormdataBody() returned %d items, want %d", len(got), tc.want)
			}
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
	if got["weirdDisabled"] != "notABool" {
		t.Errorf("weirdDisabled with non-bool disabled flag should be included")
	}
}

func TestReplaceTemplateVars(t *testing.T) {
	vars := map[string]string{
		"host": "localhost",
		"port": "8080",
	}

	cases := []struct {
		input string
		want  string
	}{
		{"http://{{host}}:{{port}}/api", "http://localhost:8080/api"},
		{"http://{{ host }}:{{ port }}/api", "http://localhost:8080/api"},
		{"http://{{missing}}/api", "http://{{missing}}/api"},
		{"plain string", "plain string"},
	}

	for _, tc := range cases {
		if got := replaceTemplateVars(tc.input, vars); got != tc.want {
			t.Errorf("replaceTemplateVars(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

