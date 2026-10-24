package postmanimport

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
