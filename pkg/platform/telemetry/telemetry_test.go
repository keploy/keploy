package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestSendEvent_StampsSentAtUTC(t *testing.T) {
	var (
		mu       sync.Mutex
		received []models.TeleEvent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev models.TeleEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("unmarshal event: %v", err)
			return
		}
		mu.Lock()
		received = append(received, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldURL := teleURL
	teleURL = srv.URL + "/analytics"
	t.Cleanup(func() { teleURL = oldURL })

	tel := NewTelemetry(zap.NewNop(), Options{
		Enabled:        true,
		Version:        "3.7.31",
		InstallationID: "test-install-id",
	})
	tel.TestRun(2, 0, 1, 3, "pass", nil)
	tel.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("received %d events, want 1", len(received))
	}
	ev := received[0]
	if ev.SentAt == "" || ev.SentAt[len(ev.SentAt)-1] != 'Z' {
		t.Fatalf("sent_at = %q, want UTC RFC3339 ending with Z", ev.SentAt)
	}
	parsed, err := time.Parse(time.RFC3339, ev.SentAt)
	if err != nil {
		t.Fatalf("sent_at %q is not valid RFC3339: %v", ev.SentAt, err)
	}
	if parsed.Unix() < ev.CreatedAt {
		t.Fatalf("sent_at (%d) before createdAt (%d)", parsed.Unix(), ev.CreatedAt)
	}
}

func TestShouldSendPing_RateLimitsAcrossInvocations(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), ".last_ping")
	oldPath := pingStatePath
	oldInterval := pingInterval
	pingStatePath = func() string { return stateFile }
	pingInterval = time.Hour
	t.Cleanup(func() {
		pingStatePath = oldPath
		pingInterval = oldInterval
	})

	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	if !shouldSendPing(base) {
		t.Fatal("first ping should be allowed")
	}
	if shouldSendPing(base.Add(5 * time.Minute)) {
		t.Fatal("second ping within interval should be suppressed")
	}
	if shouldSendPing(base.Add(59 * time.Minute)) {
		t.Fatal("ping just before interval expiry should be suppressed")
	}
	if !shouldSendPing(base.Add(time.Hour)) {
		t.Fatal("ping after interval should be allowed")
	}
	if shouldSendPing(base.Add(time.Hour + time.Second)) {
		t.Fatal("ping immediately after allowed ping should be suppressed")
	}
}

func TestPing_RespectsRateLimit(t *testing.T) {
	var (
		mu    sync.Mutex
		pings int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev models.TeleEvent
		if err := json.Unmarshal(body, &ev); err == nil && ev.EventType == "Ping" {
			mu.Lock()
			pings++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stateFile := filepath.Join(t.TempDir(), ".last_ping")
	pingMu.Lock()
	oldURL := teleURL
	oldPath := pingStatePath
	oldInterval := pingInterval
	teleURL = srv.URL + "/analytics"
	pingStatePath = func() string { return stateFile }
	pingInterval = time.Hour
	pingMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		pingMu.Lock()
		teleURL = oldURL
		pingStatePath = oldPath
		pingInterval = oldInterval
		pingMu.Unlock()
	})

	// Two consecutive CLI invocations within pingInterval should emit only 1 Ping.
	for i := 0; i < 2; i++ {
		tel := NewTelemetry(zap.NewNop(), Options{
			Enabled:        true,
			Version:        "3.7.31",
			InstallationID: "test-install-id",
		})
		tel.Ping(ctx)
		time.Sleep(80 * time.Millisecond)
		tel.Shutdown()
	}

	mu.Lock()
	got := pings
	mu.Unlock()
	if got != 1 {
		t.Fatalf("received %d Ping events across 2 invocations within 1h, want 1", got)
	}
}
