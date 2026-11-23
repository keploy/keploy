package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type testEndSvc struct {
	agent.Service // nil: any other call panics loudly
	test          *string
	setEnd        *bool
}

func (s testEndSvc) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	*s.test, _ = models.TestEndRead(ctx)
	return nil, nil
}

func (s testEndSvc) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	*s.setEnd = models.SetEndRead(ctx)
	return nil, nil
}

// ?testend=<name> reads the misses as that test's end, unescaped; no query,
// or an empty name, names no test.
func TestGetMockErrors_TestEndQueryNamesTheTest(t *testing.T) {
	for target, want := range map[string]string{
		"/agent/mockerrors":                         "",
		"/agent/mockerrors?testend=":                "",
		"/agent/mockerrors?testend=test-1":          "test-1",
		"/agent/mockerrors?other=1&testend=a+b%26c": "a b&c",
	} {
		var test string
		a := &Agent{logger: zap.NewNop(), svc: testEndSvc{test: &test}}
		rec := httptest.NewRecorder()
		a.GetMockErrors(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || test != want {
			t.Fatalf("%s: status %d, test's end read of %q, want %q", target, rec.Code, test, want)
		}
	}
}

// Only ?setend=1 reads what was consumed as the set's end.
func TestGetConsumedMocks_SetEndQueryMarksTheRead(t *testing.T) {
	for target, want := range map[string]bool{
		"/agent/consumedmocks":          false,
		"/agent/consumedmocks?setend=0": false,
		"/agent/consumedmocks?setend=1": true,
	} {
		var setEnd bool
		a := &Agent{logger: zap.NewNop(), svc: testEndSvc{setEnd: &setEnd}}
		rec := httptest.NewRecorder()
		a.GetConsumedMocks(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || setEnd != want {
			t.Fatalf("%s: status %d, set's end read %v, want %v", target, rec.Code, setEnd, want)
		}
	}
}
