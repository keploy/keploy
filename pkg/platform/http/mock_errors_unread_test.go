package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The client hands back the calls an incomplete read returned, with an error
// that says the read was incomplete; a read without the header is complete.
func TestGetMockErrors_ReadsTheIncompleteHeader(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if incomplete {
				w.Header().Set(models.UnmatchedCallsUnreadHeader, "keploy's error drain stopped before this read")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"protocol":"HTTP","actual_summary":"GET /a"}]`))
		}))
		a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
		got, err := a.GetMockErrors(context.Background())
		srv.Close()
		if len(got) != 1 {
			t.Fatalf("incomplete=%v: got %d calls, want the one in the body", incomplete, len(got))
		}
		if incomplete != errors.Is(err, models.ErrUnmatchedCallsUnread) {
			t.Fatalf("incomplete=%v: err = %v", incomplete, err)
		}
		if !incomplete && err != nil {
			t.Fatalf("a complete read failed: %v", err)
		}
	}
}
