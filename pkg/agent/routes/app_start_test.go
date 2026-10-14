package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestAppStartAcceptsAPortAndThePid(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	rec := httptest.NewRecorder()
	a.HandleAppStart(rec, httptest.NewRequest(http.MethodPost, "/agent/app/start", strings.NewReader(`{"port": 8080, "pid": 41}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppStartAcceptsThePidAloneRightAfterTheAppIsStarted(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	rec := httptest.NewRecorder()
	a.HandleAppStart(rec, httptest.NewRequest(http.MethodPost, "/agent/app/start", strings.NewReader(`{"pid": 41}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppStartRefusesABadPortOrAMissingPid(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	for _, body := range []string{`{}`, `{"port": -1, "pid": 1}`, `{"port": 70000, "pid": 1}`, `not json`, `{"port": 8080}`} {
		rec := httptest.NewRecorder()
		a.HandleAppStart(rec, httptest.NewRequest(http.MethodPost, "/agent/app/start", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", body, rec.Code)
		}
	}
}
