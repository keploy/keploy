package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

type ownListenerSvc struct {
	testEndSvc
	calls []string
	err   error
}

func (s ownListenerSvc) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	return nil, s.err
}

func (s ownListenerSvc) OwnListenerCalls() []string { return s.calls }

// /consumedmocks names the set's calls passed through as the app's own in
// models.OwnListenerCallsHeader; none when there are none, when the service
// cannot say, or when the read failed.
func TestGetConsumedMocks_NamesTheCallsPassedThroughAsTheAppsOwn(t *testing.T) {
	var setEnd bool
	for _, c := range []struct {
		name string
		svc  agent.Service
		want string
		code int
	}{
		{"two destinations", ownListenerSvc{calls: []string{"127.0.0.1:6100", "172.18.0.3:6100"}}, "127.0.0.1:6100,172.18.0.3:6100", http.StatusOK},
		{"none", ownListenerSvc{}, "", http.StatusOK},
		{"a service that cannot say", testEndSvc{setEnd: &setEnd}, "", http.StatusOK},
		{"a failed read", ownListenerSvc{calls: []string{"127.0.0.1:6100"}, err: context.Canceled}, "", http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &Agent{logger: zap.NewNop(), svc: c.svc}
			rec := httptest.NewRecorder()
			a.GetConsumedMocks(rec, httptest.NewRequest(http.MethodGet, "/agent/consumedmocks", nil))
			if rec.Code != c.code {
				t.Fatalf("status %d, want %d", rec.Code, c.code)
			}
			got, named := rec.Header()[http.CanonicalHeaderKey(models.OwnListenerCallsHeader)]
			if c.want == "" {
				if named {
					t.Fatalf("named %q, want no header", got)
				}
				return
			}
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("named %q, want %q", got, c.want)
			}
		})
	}
}

// models.OwnListenerCallsPath names the same destinations without draining
// anything: a JSON list, empty when there are none, and 501 from a service
// that cannot say, so the caller keeps what its consumed-mocks reads named.
func TestHandleOwnListenerCalls_NamesTheCallsPassedThroughAsTheAppsOwn(t *testing.T) {
	var setEnd bool
	for _, c := range []struct {
		name string
		svc  agent.Service
		code int
		want string
	}{
		{"two destinations", ownListenerSvc{calls: []string{"127.0.0.1:6100", "172.18.0.3:6100"}}, http.StatusOK, `["127.0.0.1:6100","172.18.0.3:6100"]`},
		{"none", ownListenerSvc{}, http.StatusOK, `[]`},
		{"a service that cannot say", testEndSvc{setEnd: &setEnd}, http.StatusNotImplemented, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &Agent{logger: zap.NewNop(), svc: c.svc}
			rec := httptest.NewRecorder()
			a.HandleOwnListenerCalls(rec, httptest.NewRequest(http.MethodGet, "/agent"+models.OwnListenerCallsPath, nil))
			if rec.Code != c.code {
				t.Fatalf("status %d, want %d", rec.Code, c.code)
			}
			if c.want != "" && strings.TrimSpace(rec.Body.String()) != c.want {
				t.Fatalf("body %s, want %s", rec.Body.String(), c.want)
			}
		})
	}
}
