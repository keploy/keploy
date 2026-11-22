package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// statsReaderSvc implements only the piece HandleMockStats needs. The embedded
// nil Service makes any other call panic loudly rather than silently pass.
type statsReaderSvc struct {
	agent.Service // nil: any call other than MockStats panics loudly
	stats         models.MockStats
}

func (s statsReaderSvc) MockStats(_ context.Context) (models.MockStats, error) {
	return s.stats, nil
}

// TestHandleMockStatsReportsWhenItCannotAnswer is the guard for a caller that
// cannot otherwise tell "no mocks are stored" from "this agent has no stats
// reader". Answering 200 with a zero count conflates the two, and the replay
// guard that reads this endpoint treats a zero count against a non-empty stored
// corpus as a REPLACED agent — so a 200 here fails every docker-compose test set
// on any agent build without the reader.
//
// Its sibling HandleServedMocks already returns 501 for exactly this reason.
func TestHandleMockStatsReportsWhenItCannotAnswer(t *testing.T) {
	a := &Agent{logger: zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))} // svc has no MockStats

	rec := httptest.NewRecorder()
	a.HandleMockStats(rec, httptest.NewRequest(http.MethodGet, "/agent/mock/stats", nil))

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501 so a caller can tell 'cannot report' from 'nothing stored'", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("the 501 must say why")
	}
}

// And a service that CAN report still answers normally — without this, returning
// 501 unconditionally would pass the test above.
func TestHandleMockStatsAnswersWhenItCan(t *testing.T) {
	a := &Agent{
		logger: zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)),
		svc:    statsReaderSvc{stats: models.MockStats{Loaded: 5, Consumed: 2, Missed: 1}},
	}

	rec := httptest.NewRecorder()
	a.HandleMockStats(rec, httptest.NewRequest(http.MethodGet, "/agent/mock/stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var got models.MockStats
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Loaded != 5 {
		t.Fatalf("loaded = %d; want 5", got.Loaded)
	}
}
