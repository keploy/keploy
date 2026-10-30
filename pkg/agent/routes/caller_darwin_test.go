//go:build darwin

package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// DarwinCallerPID is looked up by source port. When registered, callerPID
// returns the PID for the request's remote port.
func TestCallerPIDReturnsPIDFromRegisteredCallback(t *testing.T) {
	DarwinCallerPID = func(remotePort uint16) (int, bool) {
		if remotePort == 54321 {
			return 1234, true
		}
		return 0, false
	}
	defer func() { DarwinCallerPID = nil }()

	r := httptest.NewRequest(http.MethodPost, "/agent/instance", nil)
	r.RemoteAddr = "127.0.0.1:54321"

	pid, err := callerPID(r)
	if err != nil {
		t.Fatalf("callerPID: %v", err)
	}
	if pid != 1234 {
		t.Fatalf("callerPID = %d, want 1234", pid)
	}
}

// When the callback returns false for a port, callerPID returns an error.
func TestCallerPIDErrorsOnUnknownPort(t *testing.T) {
	DarwinCallerPID = func(uint16) (int, bool) { return 0, false }
	defer func() { DarwinCallerPID = nil }()

	r := httptest.NewRequest(http.MethodPost, "/agent/instance", nil)
	r.RemoteAddr = "127.0.0.1:54322" // valid port the callback doesn't recognize

	_, err := callerPID(r)
	if err == nil {
		t.Fatal("callerPID should error for unknown port")
	}
}

// When no callback is registered, callerPID returns an error.
func TestCallerPIDErrorsWhenNoCallbackRegistered(t *testing.T) {
	DarwinCallerPID = nil

	r := httptest.NewRequest(http.MethodPost, "/agent/instance", nil)
	r.RemoteAddr = "127.0.0.1:54321"

	_, err := callerPID(r)
	if err == nil {
		t.Fatal("callerPID should error when DarwinCallerPID is nil")
	}
}
