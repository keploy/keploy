package http_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/mock"
)

// unchargedAgentSvc says what of its capture losses no test was charged with.
type unchargedAgentSvc struct {
	lossAgentSvc
	uncharged models.CaptureLosses
	err       error
}

func (s unchargedAgentSvc) UnchargedCaptureLosses(context.Context) (models.CaptureLosses, error) {
	return s.uncharged, s.err
}

// The client reads what the agent could not capture that no test was charged
// with, field for field, through GET /agent/capturelosses/uncharged; an agent
// older than the route (404) or one that cannot count (501) is "unknown"; a
// 500 is an error that is not. keploy mock record type-asserts for it.
func TestGetUnchargedCaptureLossesReadsWhatNoTestWasChargedWith(t *testing.T) {
	var _ mock.UnchargedLossReader = newClient(t, agentWith(t, unchargedAgentSvc{}))
	want := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 2, What: "to db:3306"}}
	got, err := newClient(t, agentWith(t, unchargedAgentSvc{uncharged: want})).GetUnchargedCaptureLosses(context.Background())
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("read %+v, %v; want %+v", got, err, want)
	}
	if got, err := newClient(t, agentWith(t, unchargedAgentSvc{})).GetUnchargedCaptureLosses(context.Background()); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nothing uncharged: %+v, %v; want an empty list", got, err)
	}
	if _, err := newClient(t, agentWith(t, lossAgentSvc{})).GetUnchargedCaptureLosses(context.Background()); !errors.Is(err, models.ErrCaptureLossesUnsupported) {
		t.Fatalf("an agent that cannot say: %v", err)
	}
	if _, err := newClient(t, agentWith(t, unchargedAgentSvc{err: errors.New("bad file descriptor")})).GetUnchargedCaptureLosses(context.Background()); err == nil || errors.Is(err, models.ErrCaptureLossesUnsupported) {
		t.Fatalf("a failed read: %v, want an error that is not 'unsupported'", err)
	}
	older := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(older.Close)
	if _, err := newClient(t, older.URL+"/agent").GetUnchargedCaptureLosses(context.Background()); !errors.Is(err, models.ErrCaptureLossesUnsupported) {
		t.Fatalf("an agent older than the route: %v", err)
	}
}
