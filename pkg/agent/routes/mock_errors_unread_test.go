package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type mockErrorsSvc struct {
	agent.Service // nil: any other call panics loudly
	calls         []models.UnmatchedCall
	err           error
}

func (s mockErrorsSvc) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return s.calls, s.err
}

// An incomplete read keeps the body every client decodes, the calls that were
// read, and says it is incomplete in a header. Any other failure is still a
// 500, and a complete read carries no header.
func TestGetMockErrors_IncompleteReadIsAHeaderNotAFailure(t *testing.T) {
	read := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /a"}}
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantHeader bool
	}{
		{"complete", nil, http.StatusOK, false},
		{"incomplete", fmt.Errorf("%w: keploy's error drain stopped\nbefore this read", models.ErrUnmatchedCallsUnread), http.StatusOK, true},
		{"failed", errors.New("boom"), http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{logger: zap.NewNop(), svc: mockErrorsSvc{calls: read, err: tc.err}}
			rec := httptest.NewRecorder()
			a.GetMockErrors(rec, httptest.NewRequest(http.MethodGet, "/agent/mockerrors", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d", rec.Code, tc.wantStatus)
			}
			header := rec.Header().Get(models.UnmatchedCallsUnreadHeader)
			if (header != "") != tc.wantHeader {
				t.Fatalf("%s header = %q, want present=%v", models.UnmatchedCallsUnreadHeader, header, tc.wantHeader)
			}
			if strings.ContainsAny(header, "\r\n") {
				t.Fatalf("the header carries a line break: %q", header)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var got []models.UnmatchedCall
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("the body is not the list a client decodes: %v", err)
			}
			if len(got) != 1 || got[0].ActualSummary != "GET /a" {
				t.Fatalf("body %+v, want the calls that were read", got)
			}
		})
	}
}
