package http_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/pkg/service/mock"
)

// relayedAgentSvc counts the connections relayed live, for the client's round
// trip.
type relayedAgentSvc struct {
	agentsvc.Service
	rows models.RelayedLives
	err  error
}

func (s relayedAgentSvc) RelayedLive(context.Context) (models.RelayedLives, error) {
	return s.rows, s.err
}

// The client reads what the agent relayed live, row for row, an empty list
// for none; an agent older than the route (404), or one that cannot count
// (501), is "unknown"; a 500 is an error that is not. A new keploy with an
// older agent then claims no isolation it cannot prove.
func TestGetRelayedLiveReadsWhatTheAgentRelayed(t *testing.T) {
	want := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 2}}
	got, err := newClient(t, agentWith(t, relayedAgentSvc{rows: want})).GetRelayedLive(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read %+v, %v; want %+v", got, err, want)
	}
	if got, err := newClient(t, agentWith(t, relayedAgentSvc{})).GetRelayedLive(context.Background()); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("none relayed: %+v, %v; want an empty list", got, err)
	}
	if _, err := newClient(t, agentWith(t, relayedAgentSvc{err: models.ErrRelayedLiveUnsupported})).GetRelayedLive(context.Background()); !errors.Is(err, models.ErrRelayedLiveUnsupported) {
		t.Fatalf("an agent that cannot count: %v", err)
	}
	if _, err := newClient(t, agentWith(t, relayedAgentSvc{err: errors.New("boom")})).GetRelayedLive(context.Background()); err == nil || errors.Is(err, models.ErrRelayedLiveUnsupported) {
		t.Fatalf("a failed read: %v, want an error that is not 'unsupported'", err)
	}
	older := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(older.Close)
	if _, err := newClient(t, older.URL+"/agent").GetRelayedLive(context.Background()); !errors.Is(err, models.ErrRelayedLiveUnsupported) {
		t.Fatalf("an agent older than the route: %v", err)
	}
	// A 200 whose body is null counts nothing: it is "unknown", never "none".
	null := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	}))
	t.Cleanup(null.Close)
	if got, err := newClient(t, null.URL+"/agent").GetRelayedLive(context.Background()); !errors.Is(err, models.ErrRelayedLiveUnsupported) || got != nil {
		t.Fatalf("a null answer: %+v, %v; want unknown", got, err)
	}
	// The client is what keploy mock replay's isolation check type-asserts
	// for: renamed, the check would read every agent as an older one.
	var _ mock.RelayedLiveReader = newClient(t, older.URL+"/agent")
}
