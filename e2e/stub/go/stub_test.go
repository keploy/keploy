// Package stub_test provides end-to-end tests for the keploy stub record/replay functionality.
package stub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	mockServerPort = "9000"
	proxyPort      = "16799"
	dnsPort        = "26799"
	baseURL        = "http://localhost:" + mockServerPort
)

// TestStubRecordReplay tests the complete stub record and replay cycle.
func TestStubRecordReplay(t *testing.T) {
	if os.Getenv("KEPLOY_E2E_TEST") != "true" {
		t.Skip("Skipping E2E test. Set KEPLOY_E2E_TEST=true to run.")
	}

	// Setup test environment
	testDir := t.TempDir()
	stubPath := filepath.Join(testDir, "stubs")
	
	// Build keploy binary
	keployBin := buildKeploy(t)
	
	// Build and start mock server
	mockServerBin := buildMockServer(t)
	mockServerCmd := startMockServer(t, mockServerBin)
	defer stopProcess(mockServerCmd)
	
	// Wait for mock server to be ready
	waitForServer(t, baseURL+"/health", 10*time.Second)
	
	// Build test client
	testClientBin := buildTestClient(t)
	
	// Test 1: Record stubs
	t.Run("RecordStubs", func(t *testing.T) {
		recordCmd := exec.Command(keployBin, "stub", "record",
			"-c", testClientBin+" all",
			"-p", stubPath,
			"--name", "test-stubs",
			"--proxy-port", proxyPort,
			"--dns-port", dnsPort,
			"--record-timer", "30s",
		)
		recordCmd.Env = append(os.Environ(), "API_BASE_URL="+baseURL)
		recordCmd.Stdout = os.Stdout
		recordCmd.Stderr = os.Stderr
		
		if err := recordCmd.Run(); err != nil {
			t.Fatalf("Failed to run stub record: %v", err)
		}
		
		// Verify stubs were created
		stubDir := filepath.Join(stubPath, "test-stubs")
		if _, err := os.Stat(stubDir); os.IsNotExist(err) {
			t.Fatalf("Stub directory was not created: %s", stubDir)
		}
		
		mocksFile := filepath.Join(stubDir, "mocks.yaml")
		if _, err := os.Stat(mocksFile); os.IsNotExist(err) {
			t.Fatalf("Mocks file was not created: %s", mocksFile)
		}
		
		// Read and verify mocks content
		content, err := os.ReadFile(mocksFile)
		if err != nil {
			t.Fatalf("Failed to read mocks file: %v", err)
		}
		
		if len(content) == 0 {
			t.Fatal("Mocks file is empty")
		}
		
		t.Logf("Recorded mocks:\n%s", string(content))
	})
	
	// Stop mock server to test replay without actual server
	stopProcess(mockServerCmd)
	
	// Test 2: Replay stubs (without actual server)
	t.Run("ReplayStubs", func(t *testing.T) {
		replayCmd := exec.Command(keployBin, "stub", "replay",
			"-c", testClientBin+" all",
			"-p", stubPath,
			"--name", "test-stubs",
			"--proxy-port", proxyPort,
			"--dns-port", dnsPort,
			"--delay", "2",
		)
		replayCmd.Env = append(os.Environ(), "API_BASE_URL="+baseURL)
		replayCmd.Stdout = os.Stdout
		replayCmd.Stderr = os.Stderr
		
		if err := replayCmd.Run(); err != nil {
			t.Fatalf("Failed to run stub replay: %v", err)
		}
	})
}

// TestStubRecordAutoName tests stub recording with auto-generated names.
func TestStubRecordAutoName(t *testing.T) {
	if os.Getenv("KEPLOY_E2E_TEST") != "true" {
		t.Skip("Skipping E2E test. Set KEPLOY_E2E_TEST=true to run.")
	}

	testDir := t.TempDir()
	stubPath := filepath.Join(testDir, "stubs")
	
	keployBin := buildKeploy(t)
	mockServerBin := buildMockServer(t)
	mockServerCmd := startMockServer(t, mockServerBin)
	defer stopProcess(mockServerCmd)
	
	waitForServer(t, baseURL+"/health", 10*time.Second)
	
	testClientBin := buildTestClient(t)
	
	// Record without specifying name
	recordCmd := exec.Command(keployBin, "stub", "record",
		"-c", testClientBin+" health",
		"-p", stubPath,
		"--proxy-port", proxyPort,
		"--dns-port", dnsPort,
		"--record-timer", "10s",
	)
	recordCmd.Env = append(os.Environ(), "API_BASE_URL="+baseURL)
	recordCmd.Stdout = os.Stdout
	recordCmd.Stderr = os.Stderr
	
	if err := recordCmd.Run(); err != nil {
		t.Fatalf("Failed to run stub record: %v", err)
	}
	
	// Verify a stub directory was created with auto-generated name
	entries, err := os.ReadDir(stubPath)
	if err != nil {
		t.Fatalf("Failed to read stub path: %v", err)
	}
	
	if len(entries) == 0 {
		t.Fatal("No stub directory was created")
	}
	
	// Should have a directory with "stub-" prefix
	found := false
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "stub-") {
			found = true
			t.Logf("Auto-generated stub name: %s", entry.Name())
			break
		}
	}
	
	if !found {
		t.Fatal("No auto-generated stub directory found")
	}
}

// TestStubReplayFallbackOnMiss tests the fallback-on-miss functionality.
func TestStubReplayFallbackOnMiss(t *testing.T) {
	if os.Getenv("KEPLOY_E2E_TEST") != "true" {
		t.Skip("Skipping E2E test. Set KEPLOY_E2E_TEST=true to run.")
	}

	testDir := t.TempDir()
	stubPath := filepath.Join(testDir, "stubs")
	stubName := "fallback-test"
	
	keployBin := buildKeploy(t)
	mockServerBin := buildMockServer(t)
	mockServerCmd := startMockServer(t, mockServerBin)
	defer stopProcess(mockServerCmd)
	
	waitForServer(t, baseURL+"/health", 10*time.Second)
	
	testClientBin := buildTestClient(t)
	
	// Record only health endpoint
	recordCmd := exec.Command(keployBin, "stub", "record",
		"-c", testClientBin+" health",
		"-p", stubPath,
		"--name", stubName,
		"--proxy-port", proxyPort,
		"--dns-port", dnsPort,
		"--record-timer", "10s",
	)
	recordCmd.Env = append(os.Environ(), "API_BASE_URL="+baseURL)
	recordCmd.Stdout = os.Stdout
	recordCmd.Stderr = os.Stderr
	
	if err := recordCmd.Run(); err != nil {
		t.Fatalf("Failed to run stub record: %v", err)
	}
	
	// Replay with fallback-on-miss, requesting more endpoints than recorded
	replayCmd := exec.Command(keployBin, "stub", "replay",
		"-c", testClientBin+" all",
		"-p", stubPath,
		"--name", stubName,
		"--proxy-port", proxyPort,
		"--dns-port", dnsPort,
		"--fallback-on-miss",
		"--delay", "2",
	)
	replayCmd.Env = append(os.Environ(), "API_BASE_URL="+baseURL)
	replayCmd.Stdout = os.Stdout
	replayCmd.Stderr = os.Stderr
	
	if err := replayCmd.Run(); err != nil {
		t.Fatalf("Failed to run stub replay with fallback: %v", err)
	}
}

// Helper functions

func buildKeploy(t *testing.T) string {
	t.Helper()
	
	// Get the project root (three levels up from e2e/stub/go)
	projectRoot := filepath.Join("..", "..", "..")
	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		t.Fatalf("Failed to get absolute path: %v", err)
	}
	
	binPath := filepath.Join(t.TempDir(), "keploy")
	
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = absRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to build keploy: %v", err)
	}
	
	return binPath
}

func buildMockServer(t *testing.T) string {
	t.Helper()
	
	mockServerDir := filepath.Join("..", "fixtures", "mock-server")
	binPath := filepath.Join(t.TempDir(), "mock-server")
	
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = mockServerDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to build mock server: %v", err)
	}
	
	return binPath
}

func buildTestClient(t *testing.T) string {
	t.Helper()
	
	testClientDir := filepath.Join("..", "fixtures", "test-client")
	binPath := filepath.Join(t.TempDir(), "test-client")
	
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = testClientDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to build test client: %v", err)
	}
	
	return binPath
}

func startMockServer(t *testing.T, binPath string) *exec.Cmd {
	t.Helper()
	
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), "MOCK_SERVER_PORT="+mockServerPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start mock server: %v", err)
	}
	
	return cmd
}

func stopProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

func waitForServer(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("Server did not become ready within %v", timeout)
		case <-ticker.C:
			resp, err := http.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
	}
}

// Integration test helpers for HTTP assertions

type HTTPClient struct {
	baseURL string
	client  *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *HTTPClient) Get(path string) (map[string]interface{}, error) {
	resp, err := c.client.Get(c.baseURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	
	return result, nil
}

func (c *HTTPClient) Post(path string, data interface{}) (map[string]interface{}, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	
	resp, err := c.client.Post(c.baseURL+path, "application/json", strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w, body: %s", err, string(body))
	}
	
	return result, nil
}
