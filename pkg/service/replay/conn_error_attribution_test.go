package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A transport/connection-level simulate error must be classified so
// CreateFailedTestResult can label it APP_CONNECTION_ERROR instead of letting its
// synthetic status_code=0 masquerade as a STATUS_CODE_CHANGED content regression.
func TestIsAppConnectionErrorMsg(t *testing.T) {
	connErrors := []string{
		`Get "http://localhost:8080/x": dial tcp [::1]:8080: connect: connection refused`,
		`Get "http://localhost:8080/check-time": read tcp [::1]:43106->[::1]:8080: read: connection reset by peer`,
		`Post "http://localhost:8080/x": write tcp 127.0.0.1:5000->127.0.0.1:8080: write: broken pipe`,
		`Get "http://app:8080/x": dial tcp: lookup app on 127.0.0.11:53: no such host`,
		`Get "http://localhost:8080/x": EOF`,
	}
	for _, m := range connErrors {
		if !isAppConnectionErrorMsg(m) {
			t.Errorf("expected connection-error classification for: %q", m)
		}
	}

	notConnErrors := []string{
		`response body mismatch on field user.id`,
		`status code expected 200 got 500`,
		`unexpected end of JSON input`,
		`invalid response type for HTTP test case`,
		`internal error: test case result is nil`,
	}
	for _, m := range notConnErrors {
		if isAppConnectionErrorMsg(m) {
			t.Errorf("did NOT expect connection-error classification for: %q", m)
		}
	}
}

func TestAppendCategoryUnique(t *testing.T) {
	cats := []models.FailureCategory{models.StatusCodeChanged}
	cats = appendCategoryUnique(cats, models.AppConnectionError)
	cats = appendCategoryUnique(cats, models.AppConnectionError) // dup must be a no-op
	if len(cats) != 2 {
		t.Fatalf("expected 2 unique categories, got %d: %v", len(cats), cats)
	}
	found := false
	for _, c := range cats {
		if c == models.AppConnectionError {
			found = true
		}
	}
	if !found {
		t.Fatal("AppConnectionError category not appended")
	}
}
