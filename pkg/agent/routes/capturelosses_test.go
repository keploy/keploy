package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// lossSvc implements only CaptureLosses; the embedded nil Service makes any
// other call panic loudly.
type lossSvc struct {
	agent.Service
	losses models.CaptureLosses
	err    error
}

func (s lossSvc) CaptureLosses(context.Context) (models.CaptureLosses, error) { return s.losses, s.err }

// GET /agent/capturelosses over the real routes: 200 with every kind counted
// (an empty list, never null, when nothing was lost), 501 from an agent that
// cannot count, 500 for a read that failed. A caller fails a test on a loss,
// so "none" and "cannot say" must differ.
func TestHandleCaptureLossesAnswersTheCountOrWhyNot(t *testing.T) {
	loss := models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 2, What: "to 10.0.0.1:443"}}
	for _, tc := range []struct {
		name string
		svc  agent.Service
		code int
		want models.CaptureLosses
	}{
		{"nothing lost", lossSvc{}, http.StatusOK, models.CaptureLosses{}},
		{"a loss", lossSvc{losses: loss}, http.StatusOK, loss},
		{"cannot count", lossSvc{err: models.ErrCaptureLossesUnsupported}, http.StatusNotImplemented, nil},
		{"a service without the count", struct{ agent.Service }{}, http.StatusNotImplemented, nil},
		{"a failed read", lossSvc{err: errors.New("bad file descriptor")}, http.StatusInternalServerError, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/capturelosses", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var got models.CaptureLosses
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got == nil || len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
				t.Fatalf("body %s (%v), want %+v", rec.Body.String(), err, tc.want)
			}
		})
	}
}
