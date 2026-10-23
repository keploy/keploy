package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// relayedSvc implements only RelayedLive; the embedded nil Service makes any
// other call panic loudly.
type relayedSvc struct {
	agent.Service
	rows models.RelayedLives
	err  error
}

func (s relayedSvc) RelayedLive(context.Context) (models.RelayedLives, error) { return s.rows, s.err }

// GET /agent/relayed over the real routes: 200 with every connection relayed
// live (an empty list, never null, when none was), 501 from an agent that
// cannot count them, 500 for a read that failed. keploy mock replay claims a
// run isolated only on a 200 that lists nothing, so "none" and "cannot say"
// must differ.
func TestHandleRelayedLiveAnswersTheCountOrWhyNot(t *testing.T) {
	redis := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 3}}
	for _, tc := range []struct {
		name string
		svc  agent.Service
		code int
		want models.RelayedLives
	}{
		{"nothing relayed", relayedSvc{}, http.StatusOK, models.RelayedLives{}},
		{"Redis relayed", relayedSvc{rows: redis}, http.StatusOK, redis},
		{"cannot count", relayedSvc{err: models.ErrRelayedLiveUnsupported}, http.StatusNotImplemented, nil},
		{"a service without the count", struct{ agent.Service }{}, http.StatusNotImplemented, nil},
		{"a failed read", relayedSvc{err: errors.New("boom")}, http.StatusInternalServerError, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/relayed", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var got models.RelayedLives
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("body %s (%v), want %+v", rec.Body.String(), err, tc.want)
			}
		})
	}
}
