package postmanimport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestProcessFormdataBody_HandlesFileAndText(t *testing.T) {
	input := []map[string]interface{}{
		{
			"key":   "username",
			"value": "aditya",
			"type":  "text",
		},
		{
			"key":   "role",
			"value": "admin",
			// type omitted on purpose
		},
		{
			"key":  "avatar",
			"type": "file",
			"src":  "/path/to/avatar.png",
		},
		{
			"key":  "document",
			"type": "file",
			// src omitted on purpose
		},
	}

	got := processFormdataBody(input)

	if len(got) != 4 {
		t.Fatalf("expected 4 form entries, got %d", len(got))
	}

	// 1. Text field
	if got[0].Key != "username" || len(got[0].Values) != 1 || got[0].Values[0] != "aditya" {
		t.Errorf("entry 0: expected Key=username, Values=[aditya]; got %+v", got[0])
	}

	// 2. Text field (omitted type)
	if got[1].Key != "role" || len(got[1].Values) != 1 || got[1].Values[0] != "admin" {
		t.Errorf("entry 1: expected Key=role, Values=[admin]; got %+v", got[1])
	}

	// 3. File field with src
	if got[2].Key != "avatar" || len(got[2].Paths) != 1 || got[2].Paths[0] != "/path/to/avatar.png" {
		t.Errorf("entry 2: expected Key=avatar, Paths=[/path/to/avatar.png]; got %+v", got[2])
	}

	// 4. File field without src
	if got[3].Key != "document" {
		t.Errorf("entry 3: expected Key=document; got %+v", got[3])
	}
}

func TestProcessFormdataBody_ConcurrentAndRace(t *testing.T) {
	t.Parallel()

	input := []map[string]interface{}{
		{"key": "username", "value": "aditya", "type": "text"},
		{"key": "role", "value": "admin"},
		{"key": "avatar", "type": "file", "src": "/path/to/avatar.png"},
		{"key": "document", "type": "file"},
	}

	const goroutines = 50
	done := make(chan bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			got := processFormdataBody(input)
			if len(got) != 4 {
				t.Errorf("expected 4 entries, got %d", len(got))
			}
			if got[2].Key != "avatar" || len(got[2].Paths) != 1 || got[2].Paths[0] != "/path/to/avatar.png" {
				t.Errorf("file path mismatch: %+v", got[2])
			}
			done <- true
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}
}
