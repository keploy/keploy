package postmanimport

import (
	"encoding/json"
	"testing"
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

func TestPostmanItem_CollectRequests_NestedFolders(t *testing.T) {
	data := []byte(`[
		{
			"name": "Users",
			"item": [
				{
					"name": "Admin Subfolder",
					"item": [
						{
							"name": "Get Admin",
							"request": {
								"method": "GET",
								"header": [],
								"url": "https://api.example.test/admin"
							},
							"response": []
						},
						{
							"name": "Super Admin Subfolder",
							"item": [
								{
									"name": "Delete Super Admin",
									"request": {
										"method": "DELETE",
										"header": [],
										"url": "https://api.example.test/superadmin"
									},
									"response": []
								}
							]
						}
					]
				},
				{
					"name": "List Users",
					"request": {
						"method": "GET",
						"header": [],
						"url": "https://api.example.test/users"
					},
					"response": []
				},
				{
					"name": "Empty Subfolder",
					"item": []
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

	reqs := got.PostmanItems[0].CollectRequests()
	if len(reqs) != 3 {
		t.Fatalf("CollectRequests length = %d, want 3", len(reqs))
	}

	expectedNames := []string{"Get Admin", "Delete Super Admin", "List Users"}
	for i, name := range expectedNames {
		if reqs[i].Name != name {
			t.Errorf("reqs[%d].Name = %q, want %q", i, reqs[i].Name, name)
		}
	}
}

