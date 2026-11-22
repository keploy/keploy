package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got == nil || len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got[0], tc.want[0])) {
				t.Fatalf("body %s (%v), want %+v", rec.Body.String(), err, tc.want)
			}
		})
	}
}

// ownedSvc implements only TakeOwnedCaptureLosses.
type ownedSvc struct {
	agent.Service
	owned []models.OwnedCaptureLoss
	err   error
}

func (s ownedSvc) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	return s.owned, s.err
}

// POST /agent/capturelosses/owned over the real routes: 200 with what a
// cut-short end owned, by test (an empty list, never null), 501 from an
// agent that has none to tell apart, 500 for a take that failed. keploy test
// fails a set on these, so "none" and "cannot say" must differ.
func TestHandleOwnedCaptureLossesAnswersTheTakeOrWhyNot(t *testing.T) {
	owned := []models.OwnedCaptureLoss{{Test: "between test cases", Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:50912"}}}
	for _, tc := range []struct {
		name string
		svc  agent.Service
		code int
		want []models.OwnedCaptureLoss
	}{
		{"none owned", ownedSvc{}, http.StatusOK, []models.OwnedCaptureLoss{}},
		{"a gap's loss", ownedSvc{owned: owned}, http.StatusOK, owned},
		{"a service without the take", struct{ agent.Service }{}, http.StatusNotImplemented, nil},
		{"a failed take", ownedSvc{err: errors.New("bad file descriptor")}, http.StatusInternalServerError, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agent/capturelosses/owned", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var got []models.OwnedCaptureLoss
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got == nil || len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got[0], tc.want[0])) {
				t.Fatalf("body %s (%v), want %+v", rec.Body.String(), err, tc.want)
			}
		})
	}
}

// drainSvc implements only DrainCaptureLosses.
type drainSvc struct {
	agent.Service
	err error
}

func (s drainSvc) DrainCaptureLosses(context.Context) error { return s.err }

// POST /agent/capturelosses/drain over the real routes: 204 once the proxy
// counted every close the application's stop made, 501 from an agent that does
// not drain, 500 with the reason for a drain that could not finish. keploy
// test leaves a set unjudged on a failed drain, so the three must differ.
func TestHandleDrainCaptureLossesAnswersTheDrainOrWhyNot(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  agent.Service
		code int
	}{
		{"drained", drainSvc{}, http.StatusNoContent},
		{"a service without the drain", struct{ agent.Service }{}, http.StatusNotImplemented},
		{"a drain that could not finish", drainSvc{err: errors.New("keploy's proxy had not finished counting the close of 1 connection(s)")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			DefaultRoutes{}.New(r, tc.svc, zap.NewNop())
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agent/capturelosses/drain", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code == http.StatusInternalServerError && !strings.Contains(rec.Body.String(), "had not finished counting") {
				t.Fatalf("body %s, want the drain's reason", rec.Body.String())
			}
		})
	}
}
