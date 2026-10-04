package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/appstart"
)

func TestAppStartNotesEachStart(t *testing.T) {
	a := &Agent{}
	before := len(appstart.List())
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	for _, body := range []string{`{"port": 8080, "pid": 41}`, `{"port": 8080, "pid": 42, "at": "2026-10-05T10:00:00Z"}`} {
		rec := httptest.NewRecorder()
		a.HandleAppStart(rec, httptest.NewRequest(http.MethodPost, "/agent/app/start", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	got := appstart.List()[before:]
	if len(got) != 2 || got[0].Port != 8080 || got[0].PID != 41 || !got[1].At.Equal(at) {
		t.Fatalf("starts = %+v", got)
	}
}

func TestAppStartRefusesAMissingPort(t *testing.T) {
	a := &Agent{}
	for _, body := range []string{`{}`, `{"port": 0}`, `{"port": 70000}`, `not json`} {
		rec := httptest.NewRecorder()
		a.HandleAppStart(rec, httptest.NewRequest(http.MethodPost, "/agent/app/start", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", body, rec.Code)
		}
	}
}
