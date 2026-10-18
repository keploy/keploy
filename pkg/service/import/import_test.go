package postmanimport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReplaceTemplateVars(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		variables map[string]string
		expected  string
	}{
		{
			name:      "replaces a hyphenated variable",
			input:     "{{base-url}}/users",
			variables: map[string]string{"base-url": "https://example.com"},
			expected:  "https://example.com/users",
		},
		{
			name:      "replaces an underscored variable",
			input:     "{{base_url}}/users",
			variables: map[string]string{"base_url": "https://example.com"},
			expected:  "https://example.com/users",
		},
		{
			name:      "allows whitespace around a hyphenated variable",
			input:     "{{ base-url }}/users",
			variables: map[string]string{"base-url": "https://example.com"},
			expected:  "https://example.com/users",
		},
		{
			name:      "keeps an unknown variable unchanged",
			input:     "{{missing-url}}/users",
			variables: map[string]string{"base-url": "https://example.com"},
			expected:  "{{missing-url}}/users",
		},
		{
			name:  "replaces multiple variables",
			input: "{{scheme}}://{{api-host}}/{{resource_name}}",
			variables: map[string]string{
				"scheme":        "https",
				"api-host":      "example.com",
				"resource_name": "users",
			},
			expected: "https://example.com/users",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, replaceTemplateVars(test.input, test.variables))
		})
	}
}

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
