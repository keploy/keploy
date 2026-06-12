package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func noiseTestMock(body string) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"src": "noisetest"},
			HTTPReq: &models.HTTPReq{
				Method: "POST", URL: "http://x/y", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   body,
			},
			HTTPResp: &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: `{"ok":true}`},
		},
	}
}

// PersistMockNoise must write learned req_body_noise onto the on-disk mock
// WITHOUT pruning the other mocks — that's the whole point: detection without
// --remove-unused-mocks used to discard everything learned.
func TestPersistMockNoise_WritesNoiseWithoutPruning(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	ctx := context.Background()
	testSet := "test-set-1"

	// Two mocks; only mock-0 learns noise. mock-1 must survive untouched.
	if err := ys.InsertMock(ctx, noiseTestMock(`{"request_id":"r-1"}`), testSet); err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(ctx, noiseTestMock(`{"other":"o"}`), testSet); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}

	err := ys.PersistMockNoise(ctx, testSet, map[string]models.MockState{
		"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.request_id": {}}},
		// A consumed mock with no learned noise must be a no-op entry.
		"mock-1": {Name: "mock-1"},
	})
	if err != nil {
		t.Fatalf("PersistMockNoise: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, testSet, "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "req_body_noise") || !strings.Contains(content, "body.request_id") {
		t.Errorf("learned noise not persisted; file:\n%s", content)
	}
	// No pruning: both mocks still present.
	if !strings.Contains(content, `"other"`) {
		t.Errorf("unrelated mock was lost during noise persistence; file:\n%s", content)
	}
}

// A state map with no learned noise must not rewrite the file at all.
func TestPersistMockNoise_NoNoiseIsNoOp(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	ctx := context.Background()
	testSet := "test-set-1"

	if err := ys.InsertMock(ctx, noiseTestMock(`{"a":"b"}`), testSet); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, testSet, "mocks.yaml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := ys.PersistMockNoise(ctx, testSet, map[string]models.MockState{
		"mock-0": {Name: "mock-0"},
	}); err != nil {
		t.Fatalf("PersistMockNoise: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("file rewritten despite no learned noise")
	}
}
