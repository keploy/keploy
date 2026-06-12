package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// mockNetError creates a mock network error with the given message
type mockNetError struct {
	msg string
}

func (m *mockNetError) Error() string {
	return m.msg
}

func (m *mockNetError) Timeout() bool   { return false }
func (m *mockNetError) Temporary() bool { return false }

func TestNewProxyError_PortInUse(t *testing.T) {
	err := &mockNetError{msg: "bind: address already in use"}
	proxyErr := NewProxyError("Test Proxy", 16789, err)

	if proxyErr == nil {
		t.Fatal("Expected ProxyError, got nil")
	}

	if proxyErr.Code != ErrCodePortInUse {
		t.Errorf("Expected error code %s, got %s", ErrCodePortInUse, proxyErr.Code)
	}

	if proxyErr.Port != 16789 {
		t.Errorf("Expected port 16789, got %d", proxyErr.Port)
	}

	errMsg := proxyErr.Error()
	if !strings.Contains(errMsg, "Port 16789 is already in use") {
		t.Errorf("Error message should mention port being in use: %s", errMsg)
	}

	if !strings.Contains(errMsg, "lsof") {
		t.Errorf("Error message should suggest using lsof: %s", errMsg)
	}

	if len(proxyErr.Solutions) == 0 {
		t.Error("Expected solutions to be provided")
	}
}

func TestNewProxyError_PermissionDenied(t *testing.T) {
	tests := []struct {
		name           string
		port           uint32
		expectedHint   string
		shouldHaveSudo bool
	}{
		{
			name:           "Privileged port",
			port:           80,
			expectedHint:   "privileged port",
			shouldHaveSudo: true,
		},
		{
			name:           "Unprivileged port",
			port:           8080,
			expectedHint:   "Insufficient permissions",
			shouldHaveSudo: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &mockNetError{msg: "bind: permission denied"}
			proxyErr := NewProxyError("Test Proxy", tt.port, err)

			if proxyErr == nil {
				t.Fatal("Expected ProxyError, got nil")
			}

			if proxyErr.Code != ErrCodePermissionDenied {
				t.Errorf("Expected error code %s, got %s", ErrCodePermissionDenied, proxyErr.Code)
			}

			errMsg := proxyErr.Error()
			if !strings.Contains(strings.ToLower(errMsg), strings.ToLower(tt.expectedHint)) {
				t.Errorf("Error message should contain hint '%s': %s", tt.expectedHint, errMsg)
			}

			if tt.shouldHaveSudo && !strings.Contains(errMsg, "sudo") {
				t.Errorf("Error message should suggest sudo: %s", errMsg)
			}
		})
	}
}

func TestNewProxyError_NetworkUnreachable(t *testing.T) {
	err := &mockNetError{msg: "network is unreachable"}
	proxyErr := NewProxyError("Test Proxy", 16789, err)

	if proxyErr == nil {
		t.Fatal("Expected ProxyError, got nil")
	}

	if proxyErr.Code != ErrCodeNetworkUnreachable {
		t.Errorf("Expected error code %s, got %s", ErrCodeNetworkUnreachable, proxyErr.Code)
	}

	errMsg := proxyErr.Error()
	if !strings.Contains(errMsg, "network connectivity") {
		t.Errorf("Error message should mention network connectivity: %s", errMsg)
	}
}

func TestNewDNSProxyError(t *testing.T) {
	err := &mockNetError{msg: "bind: address already in use"}
	proxyErr := NewDNSProxyError("tcp", 26789, err)

	if proxyErr == nil {
		t.Fatal("Expected ProxyError, got nil")
	}

	if proxyErr.Component != "TCP DNS Server" {
		t.Errorf("Expected component 'TCP DNS Server', got '%s'", proxyErr.Component)
	}

	errMsg := proxyErr.Error()
	if !strings.Contains(errMsg, "TCP DNS Server") {
		t.Errorf("Error message should mention TCP DNS Server: %s", errMsg)
	}
}

func TestProxyError_ErrorFormat(t *testing.T) {
	err := &mockNetError{msg: "bind: address already in use"}
	proxyErr := NewProxyError("Test Component", 8080, err)

	if proxyErr == nil {
		t.Fatal("Expected ProxyError, got nil")
	}

	errMsg := proxyErr.Error()

	requiredSections := []string{
		"Failed to start Test Component",
		"port 8080",
		"Hint:",
		"Possible solutions:",
		"1.",
	}

	for _, section := range requiredSections {
		if !strings.Contains(errMsg, section) {
			t.Errorf("Error message should contain '%s':\n%s", section, errMsg)
		}
	}
}

func TestProxyError_MultipleSolutions(t *testing.T) {
	err := &mockNetError{msg: "bind: address already in use"}
	proxyErr := NewProxyError("Test Proxy", 16789, err)

	if proxyErr == nil {
		t.Fatal("Expected ProxyError, got nil")
	}

	if len(proxyErr.Solutions) < 2 {
		t.Errorf("Expected at least 2 solutions, got %d", len(proxyErr.Solutions))
	}

	errMsg := proxyErr.Error()
	for i := 1; i <= len(proxyErr.Solutions); i++ {
		expectedNum := fmt.Sprintf("%d.", i)
		if !strings.Contains(errMsg, expectedNum) {
			t.Errorf("Error message should contain numbered solution '%s'", expectedNum)
		}
	}
}
