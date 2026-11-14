package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These tests point the package-level telemetry URL at a local sink, so none
// of them may run in parallel.

// teleSink stands in for the telemetry endpoint and records every request
// posted to it. Until release is called, a sink made with holdAnswers keeps
// each request waiting for its answer, or until the client gives up.
type teleSink struct {
	mu          sync.Mutex
	raw         []rawRequest
	arrived     chan struct{}
	hold        chan struct{}
	releaseOnce sync.Once
}

type rawRequest struct {
	method      string
	contentType string
	body        []byte
}

// receivedRequest is one request the sink saw, with its JSON body decoded.
type receivedRequest struct {
	method      string
	contentType string
	event       sentEvent
}

// sentEvent is the JSON body of one telemetry request.
type sentEvent struct {
	EventType      string          `json:"eventType"`
	InstallationID string          `json:"installationId"`
	KeployVersion  string          `json:"keploy_version"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	Meta           json.RawMessage `json:"meta"`
}

func newTeleSink(t *testing.T, holdAnswers bool) *teleSink {
	t.Helper()
	s := &teleSink{arrived: make(chan struct{}, 128), hold: make(chan struct{})}
	if !holdAnswers {
		s.release()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.raw = append(s.raw, rawRequest{method: r.Method, contentType: r.Header.Get("Content-Type"), body: body})
		s.mu.Unlock()
		select {
		case s.arrived <- struct{}{}:
		default:
		}
		select {
		case <-s.hold:
		case <-r.Context().Done():
		}
	}))
	prevURL := teleURL
	teleURL = srv.URL
	t.Cleanup(func() {
		s.release()
		srv.Close()
		teleURL = prevURL
	})
	return s
}

func (s *teleSink) release() {
	s.releaseOnce.Do(func() { close(s.hold) })
}

func (s *teleSink) waitForRequest(t *testing.T) {
	t.Helper()
	select {
	case <-s.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no telemetry request reached the sink")
	}
}

func (s *teleSink) requests(t *testing.T) []receivedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	got := make([]receivedRequest, 0, len(s.raw))
	for _, r := range s.raw {
		var ev sentEvent
		require.NoError(t, json.Unmarshal(r.body, &ev), "telemetry body is not JSON: %s", r.body)
		got = append(got, receivedRequest{method: r.method, contentType: r.contentType, event: ev})
	}
	return got
}

func (s *teleSink) eventTypes(t *testing.T) []string {
	t.Helper()
	var types []string
	for _, r := range s.requests(t) {
		types = append(types, r.event.EventType)
	}
	return types
}

// sendMarker sends one "Marker" event from a fresh enabled client and waits
// for it. A request sent earlier by the client under test arrives before or
// alongside it, so checking the sink afterwards shows whether one was sent.
func sendMarker() {
	tel := NewTelemetry(zap.NewNop(), Options{Enabled: true})
	tel.sendTracked("Marker")
	tel.Shutdown()
}

func TestTelemetry_DisabledSendsNothing(t *testing.T) {
	sink := newTeleSink(t, false)
	tel := NewTelemetry(zap.NewNop(), Options{Enabled: false, Version: "3.2.1", InstallationID: "inst-123"})

	tel.Ping(t.Context())
	tel.TestSetRun(3, 1, "test-set-0", "PASSED")
	tel.TestRun(3, 1, 2, 5, "PASSED", nil)
	tel.MockTestRun(9)
	tel.RecordSessionCompleted(5, 7, 1200, "completed", "")
	tel.TestRunAborted("no test sets found")
	tel.RecordedTestSuite("test-set-0", 4, map[string]int{"Http": 3}, nil)
	tel.RecordedTestAndMocks()
	tel.RecordedMocks(map[string]int{"Http": 3})
	tel.RecordedTestCaseMock("Http")
	tel.SendTelemetry("Custom")
	tel.Shutdown()
	sendMarker()

	assert.Equal(t, []string{"Marker"}, sink.eventTypes(t))
}

func TestTelemetry_EventPayload(t *testing.T) {
	tests := []struct {
		name     string
		send     func(tel *Telemetry)
		wantType string
		wantMeta string
	}{
		{
			name:     "TestRun",
			send:     func(tel *Telemetry) { tel.TestRun(3, 1, 2, 5, "PASSED", map[string]interface{}{"mode": "docker"}) },
			wantType: "TestRun",
			wantMeta: `{"Passed-Tests":3,"Failed-Tests":1,"Test-Sets":2,"Mocks-Consumed":5,"Run-Status":"PASSED","mode":"docker","language":"go"}`,
		},
		{
			name:     "TestSetRun",
			send:     func(tel *Telemetry) { tel.TestSetRun(3, 1, "test-set-0", "FAILED") },
			wantType: "TestSetRun",
			wantMeta: `{"Passed-Tests":3,"Failed-Tests":1,"Test-Set":"test-set-0","Run-Status":"FAILED","language":"go"}`,
		},
		{
			name:     "MockTestRun",
			send:     func(tel *Telemetry) { tel.MockTestRun(9) },
			wantType: "MockTestRun",
			wantMeta: `{"Utilized-Mocks":9,"language":"go"}`,
		},
		{
			name:     "RecordSessionCompleted",
			send:     func(tel *Telemetry) { tel.RecordSessionCompleted(5, 7, 1200, "completed", "") },
			wantType: "RecordSessionCompleted",
			wantMeta: `{"test_count":5,"mock_count":7,"duration_ms":1200,"status":"completed","stop_reason":"completed","language":"go"}`,
		},
		{
			name:     "TestRunAborted",
			send:     func(tel *Telemetry) { tel.TestRunAborted("no test sets found") },
			wantType: "TestRunAborted",
			wantMeta: `{"status":"aborted","stop_reason":"no_testsets","language":"go"}`,
		},
		{
			name: "RecordedTestSuite",
			send: func(tel *Telemetry) {
				tel.RecordedTestSuite("test-set-0", 4, map[string]int{"Http": 3, "Postgres": 2}, map[string]interface{}{"mode": "native"})
			},
			wantType: "RecordedTestSuite",
			wantMeta: `{"test-set":"test-set-0","tests":4,"mocks":{"Http":3,"Postgres":2},"mode":"native","language":"go"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := newTeleSink(t, false)
			globals := &sync.Map{}
			globals.Store("language", "go")
			tel := NewTelemetry(zap.NewNop(), Options{Enabled: true, Version: "3.2.1", InstallationID: "inst-123", GlobalMap: globals})

			tt.send(tel)
			tel.Shutdown()

			got := sink.requests(t)
			require.Len(t, got, 1)
			assert.Equal(t, http.MethodPost, got[0].method)
			assert.Equal(t, "application/json; charset=utf-8", got[0].contentType)
			ev := got[0].event
			assert.Equal(t, tt.wantType, ev.EventType)
			assert.Equal(t, "inst-123", ev.InstallationID)
			assert.Equal(t, "3.2.1", ev.KeployVersion)
			assert.Equal(t, runtime.GOOS, ev.OS)
			assert.Equal(t, runtime.GOARCH, ev.Arch)
			assert.JSONEq(t, tt.wantMeta, string(ev.Meta))
		})
	}
}

func TestTelemetry_RecordingCountersFlushInOneBatchAndShutdownWaitsForIt(t *testing.T) {
	sink := newTeleSink(t, true)
	tel := NewTelemetry(zap.NewNop(), Options{Enabled: true})
	for range 50 {
		tel.RecordedTestCaseMock("Http")
	}
	for range 3 {
		tel.RecordedTestAndMocks()
	}

	shutdownDone := make(chan struct{})
	go func() {
		tel.Shutdown()
		close(shutdownDone)
	}()

	sink.waitForRequest(t)
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the batch request was still waiting for its answer")
	default:
	}
	sink.release()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the batch request was answered")
	}

	got := sink.requests(t)
	require.Len(t, got, 1, "53 recording calls are sent as one batch, not one request each")
	assert.Equal(t, "RecordedBatch", got[0].event.EventType)
	assert.JSONEq(t, `{"recorded_tests":3,"recorded_mocks":50}`, string(got[0].event.Meta))
}

func TestTelemetry_SendAfterShutdownSendsNothing(t *testing.T) {
	tests := []struct {
		name string
		send func(tel *Telemetry)
	}{
		{name: "SendTelemetry", send: func(tel *Telemetry) { tel.SendTelemetry("AfterShutdown") }},
		{name: "TestRun", send: func(tel *Telemetry) { tel.TestRun(3, 1, 2, 5, "PASSED", nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := newTeleSink(t, false)
			tel := NewTelemetry(zap.NewNop(), Options{Enabled: true})
			tel.Shutdown()

			tt.send(tel)
			sendMarker()

			assert.Equal(t, []string{"Marker"}, sink.eventTypes(t))
		})
	}
}

func TestTelemetry_ShutdownWithAnUnansweredRequestReturnsAfterTheClientTimeout(t *testing.T) {
	sink := newTeleSink(t, true) // never answers while the test runs
	tel := NewTelemetry(zap.NewNop(), Options{Enabled: true})
	tel.TestRunAborted("no test sets found")
	sink.waitForRequest(t)

	shutdownDone := make(chan struct{})
	go func() {
		tel.Shutdown()
		close(shutdownDone)
	}()

	// The client gives up on a request after 2s. The 4s on top keep a busy
	// runner, or one running with coverage on, from failing this, and still
	// fail a Shutdown that waits on the request for good.
	select {
	case <-shutdownDone:
	case <-time.After(6 * time.Second):
		t.Fatal("Shutdown was still waiting 6s after it started on a request the server never answers")
	}
}
