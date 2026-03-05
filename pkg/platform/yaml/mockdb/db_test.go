package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMockYaml_BufferedWrite(t *testing.T) {
	// Setup temporary directory
	tmpDir, err := os.MkdirTemp("", "keploy-mockdb-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logger := zap.NewNop()
	mockYaml := New(logger, tmpDir, "mocks")

	ctx := context.Background()
	testSetID := "test-set-1"

	// Insert mocks
	count := 10
	for i := 0; i < count; i++ {
		mock := &models.Mock{
			Version: models.V1Beta1,
			Kind:    models.HTTP,
			Name:    fmt.Sprintf("mock-%d", i),
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type": "config",
				},
				HTTPReq:  &models.HTTPReq{},
				HTTPResp: &models.HTTPResp{},
			},
		}
		if err := mockYaml.InsertMock(ctx, mock, testSetID); err != nil {
			t.Fatalf("InsertMock failed: %v", err)
		}
	}

	// Verify file size - should possibly be smaller than expected if buffer not flushed,
	// or empty if buffer size > written data.
	// But bufio.Writer default size is 4KB. 10 mocks might be small.
	// Let's check file existence at least.
	mockFile := filepath.Join(tmpDir, testSetID, "mocks.yaml")
	if _, err := os.Stat(mockFile); err != nil {
		t.Fatalf("Mock file not created: %v", err)
	}

	// Close to flush
	if err := mockYaml.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Read file and verify content
	content, err := os.ReadFile(mockFile)
	if err != nil {
		t.Fatalf("Failed to read mock file: %v", err)
	}

	// Check if all mocks are present
	strContent := string(content)
	for i := 0; i < count; i++ {
		if !strings.Contains(strContent, fmt.Sprintf("name: mock-%d", i)) {
			t.Errorf("Mock %d not found in file content", i)
		}
	}

	// Check version header
	if !strings.Contains(strContent, "version:") {
		t.Error("Version header missing")
	}
}

func TestMockYaml_FileSwitching(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "keploy-mockdb-test-switch")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logger := zap.NewNop()
	mockYaml := New(logger, tmpDir, "mocks")
	ctx := context.Background()

	// Set 1
	if err := mockYaml.InsertMock(ctx, &models.Mock{
		Kind: models.HTTP,
		Name: "m1",
		Spec: models.MockSpec{HTTPReq: &models.HTTPReq{}, HTTPResp: &models.HTTPResp{}},
	}, "set1"); err != nil {
		t.Fatal(err)
	}

	// Switch to Set 2
	if err := mockYaml.InsertMock(ctx, &models.Mock{
		Kind: models.HTTP,
		Name: "m2",
		Spec: models.MockSpec{HTTPReq: &models.HTTPReq{}, HTTPResp: &models.HTTPResp{}},
	}, "set2"); err != nil {
		t.Fatal(err)
	}

	// Switch back to Set 1 (should append)
	if err := mockYaml.InsertMock(ctx, &models.Mock{
		Kind: models.HTTP,
		Name: "m3",
		Spec: models.MockSpec{HTTPReq: &models.HTTPReq{}, HTTPResp: &models.HTTPResp{}},
	}, "set1"); err != nil {
		t.Fatal(err)
	}

	mockYaml.Close()

	// Verify set1 has mock-0 (m1) and mock-2 (m3)
	content1, _ := os.ReadFile(filepath.Join(tmpDir, "set1", "mocks.yaml"))
	s1 := string(content1)
	if !strings.Contains(s1, "name: mock-0") || !strings.Contains(s1, "name: mock-2") {
		t.Errorf("Set1 missing mocks: %s", s1)
	}

	// Verify set2 has mock-1 (m2)
	content2, _ := os.ReadFile(filepath.Join(tmpDir, "set2", "mocks.yaml"))
	s2 := string(content2)
	if !strings.Contains(s2, "name: mock-1") {
		t.Errorf("Set2 missing mocks: %s", s2)
	}
}

func TestMockYaml_ConcurrentAccess(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "keploy-mockdb-test-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	logger := zap.NewNop()
	mockYaml := New(logger, tmpDir, "mocks")
	ctx := context.Background()
	testSetID := "concurrent-set"

	var wg sync.WaitGroup
	count := 100

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			mock := &models.Mock{
				Kind: models.HTTP,
				Name: fmt.Sprintf("mock-%d", id),
				Spec: models.MockSpec{
					HTTPReq:  &models.HTTPReq{},
					HTTPResp: &models.HTTPResp{},
				},
			}
			if err := mockYaml.InsertMock(ctx, mock, testSetID); err != nil {
				t.Errorf("Concurrent insert failed: %v", err)
			}
		}(i)
	}

	wg.Wait()
	mockYaml.Close()

	content, _ := os.ReadFile(filepath.Join(tmpDir, testSetID, "mocks.yaml"))
	s := string(content)
	// Just check if we have enough "name:" entries roughly, or check length
	// A simpler check:
	lines := strings.Split(s, "\n")
	mockCount := 0
	for _, line := range lines {
		if strings.Contains(line, "name: mock-") {
			mockCount++
		}
	}
	if mockCount != count {
		t.Errorf("Expected %d mocks, found %d", count, mockCount)
	}
}
