package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func resetPingGate(t *testing.T) {
	t.Helper()
	pingGateMu.Lock()
	origInterval := pingInterval
	origNow := nowFunc
	lastPingAt = time.Time{}
	pingGateMu.Unlock()
	t.Cleanup(func() {
		pingGateMu.Lock()
		pingInterval = origInterval
		nowFunc = origNow
		lastPingAt = time.Time{}
		pingGateMu.Unlock()
	})
}

func TestPingIsRateLimitedProcessWide(t *testing.T) {
	resetPingGate(t)

	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	if !allowPing(base) {
		t.Fatal("first Ping must be allowed")
	}
	if allowPing(base.Add(5 * time.Minute)) {
		t.Fatal("second Ping 5m later must be suppressed under 1h rate limit")
	}
	if allowPing(base.Add(59 * time.Minute)) {
		t.Fatal("Ping at 59m must still be suppressed")
	}
	if !allowPing(base.Add(time.Hour)) {
		t.Fatal("Ping at 1h must be allowed")
	}
}

func TestSendTelemetryIncludesUTCSentAtAndRateLimitsDuplicatePingLoops(t *testing.T) {
	resetPingGate(t)

	var (
		mu       sync.Mutex
		received []models.TeleEvent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev models.TeleEvent
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode TeleEvent: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	origURL := teleURL
	teleURL = srv.URL + "/analytics"
	t.Cleanup(func() { teleURL = origURL })

	fixed := time.Date(2026, 11, 28, 10, 15, 30, 0, time.FixedZone("IST", 5*3600+30*60))
	pingGateMu.Lock()
	nowFunc = func() time.Time { return fixed }
	pingGateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	// Two Telemetry instances in the same process (matching enterprise + OSS
	// GetService) must send at most one Ping within the rate-limit window.
	t1 := NewTelemetry(zap.NewNop(), Options{Enabled: true, Version: "3.7.30", InstallationID: "inst-1"})
	t2 := NewTelemetry(zap.NewNop(), Options{Enabled: true, Version: "3.7.30", InstallationID: "inst-1"})
	t1.Ping(ctx)
	t2.Ping(ctx)

	// Also emit a tracked event and drain so we deterministically verify sent_at.
	t1.TestRun(3, 0, 1, 5, "pass", nil)
	t1.Shutdown()
	t2.Shutdown()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Give any stray duplicate Ping goroutine a moment to arrive if broken.
	time.Sleep(100 * time.Millisecond)
	cancel()

	mu.Lock()
	defer mu.Unlock()
	pings := 0
	var testRun *models.TeleEvent
	for i := range received {
		ev := &received[i]
		if !strings.HasSuffix(ev.SentAt, "Z") {
			t.Fatalf("event %s sent_at = %q, want UTC RFC3339 ending in Z", ev.EventType, ev.SentAt)
		}
		parsed, err := time.Parse(time.RFC3339, ev.SentAt)
		if err != nil {
			t.Fatalf("event %s sent_at %q invalid RFC3339: %v", ev.EventType, ev.SentAt, err)
		}
		if parsed.Unix() != fixed.UTC().Unix() {
			t.Fatalf("event %s sent_at = %s, want %s", ev.EventType, parsed, fixed.UTC().Format(time.RFC3339))
		}
		if ev.CreatedAt != fixed.UTC().Unix() {
			t.Fatalf("event %s createdAt = %d, want %d", ev.EventType, ev.CreatedAt, fixed.UTC().Unix())
		}
		if ev.EventType == "Ping" {
			pings++
		}
		if ev.EventType == models.TeleEventTestRun {
			testRun = ev
		}
	}
	if pings != 1 {
		t.Fatalf("got %d Ping events from 2 concurrent Telemetry instances, want 1", pings)
	}
	if testRun == nil {
		t.Fatal("expected TestRun event to be received")
	}
}
