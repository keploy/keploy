package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const spilledBodyBytes = 16 << 10

// spilledHTTPMock is a per-test HTTP mock, as the HTTP recorder writes it,
// whose response is big enough for the agent's disk store to keep apart from
// the mock (responseSpillMinBytes).
//
// The HTTP recorder tags its mocks HTTP_CLIENT, which DeriveLifetime makes a
// session mock unless KEPLOY_STRICT_MOCK_WINDOW is set to an enabling value in
// the process that runs `keploy test`; with it, the mock is per-test, and only
// per-test mocks go to the disk store. The lifetime is set here as that
// classification sets it.
func spilledHTTPMock(name, method, path string, at time.Time) *models.Mock {
	body := strings.Repeat("x", spilledBodyBytes)
	m := &models.Mock{
		Version: models.GetVersion(),
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": models.HTTPClient},
			HTTPReq: &models.HTTPReq{
				Method:     models.Method(method),
				ProtoMajor: 1,
				ProtoMinor: 1,
				URL:        "http://upstream" + path,
				Header:     map[string]string{"Host": "upstream"},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "text/plain", "Content-Length": strconv.Itoa(len(body))},
				Body:       body,
			},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	return m
}

// stageSpilled passes mocks through the agent's disk store and stages the
// window it hands back, as the agent stages a test under strict mock windows.
// The window's mocks come back without their responses, to be loaded when
// served.
func stageSpilled(t *testing.T, at time.Time, mocks ...*models.Mock) (*DiskMocks, *MockManager, []*models.Mock) {
	t.Helper()
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	for _, m := range mocks {
		if err := disk.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	window, err := disk.LoadWindow(at.Add(-time.Second), at.Add(time.Second))
	if err != nil || len(window) != len(mocks) {
		t.Fatalf("LoadWindow = (%d mocks, %v)", len(window), err)
	}
	for _, m := range window {
		if m.Spec.HTTPResp != nil || !m.HasSpilledResponse() {
			t.Fatalf("precondition: the store must keep the response of %s apart", m.Name)
		}
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(window, nil, at.Add(-time.Second), at.Add(time.Second))
	return disk, mm, window
}

// serveOne sends rawReq through the HTTP integration's replay path and returns
// the response the application reads, or nil and the integration's error when
// none arrives.
func serveOne(t *testing.T, h integrations.Integrations, mm integrations.MockMemDb, rawReq string) (*http.Response, []byte, error) {
	t.Helper()
	app, agent := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- h.MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		agent.Close()
	}()
	// finish closes the application's end and returns what the integration
	// returned; it runs once, whichever way serveOne returns.
	var (
		finished  bool
		servedErr error
	)
	finish := func() error {
		if !finished {
			app.Close()
			servedErr, finished = <-served, true
		}
		return servedErr
	}
	defer finish()
	if _, err := app.Write([]byte(rawReq)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		return nil, nil, finish()
	}
	var body []byte
	if resp.ContentLength < 0 {
		body, _ = io.ReadAll(resp.Body)
	} else {
		body = make([]byte, resp.ContentLength)
		if _, err := io.ReadFull(resp.Body, body); err != nil {
			t.Fatalf("reading the served body: %v", err)
		}
	}
	return resp, body, nil
}

func wantRecordedBody(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if resp.StatusCode != 200 || len(body) != spilledBodyBytes || string(body) != strings.Repeat("x", spilledBodyBytes) {
		t.Fatalf("served %d with a %d-byte body, want 200 with the recorded %d bytes", resp.StatusCode, len(body), spilledBodyBytes)
	}
}

// A per-test HTTP mock whose response the agent's disk store kept apart from it
// is served with that response. The HTTP integration refused a mock with no
// response before it loaded it, so the match consumed the mock and the
// application got no reply. The response is loaded into a copy: the pooled mock
// is left as the store handed it over, since other connections may be reading
// it.
func TestHTTPServesAResponseTheDiskStoreKeptApart(t *testing.T) {
	at := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	_, mm, window := stageSpilled(t, at, spilledHTTPMock("big-response", "GET", "/big", at))

	resp, body, err := serveOne(t, httpint.New(zap.NewNop()), mm, "GET /big HTTP/1.1\r\nHost: upstream\r\n\r\n")
	if resp == nil {
		t.Fatalf("no response for the mocked call; the integration returned %v", err)
	}
	wantRecordedBody(t, resp, body)
	if window[0].Spec.HTTPResp != nil || !window[0].HasSpilledResponse() {
		t.Fatal("serving wrote the loaded response into the pooled mock")
	}
}

// The pass-through path serves a recorded telemetry mock without consuming it,
// and skipped one whose response the store kept apart, answering with an empty
// synthetic 200 instead of the recorded response.
func TestHTTPPassThroughServesAResponseTheDiskStoreKeptApart(t *testing.T) {
	at := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	_, mm, window := stageSpilled(t, at, spilledHTTPMock("traces", "POST", "/v1/traces", at))

	resp, body, err := serveOne(t, httpint.New(zap.NewNop()), mm, "POST /v1/traces HTTP/1.1\r\nHost: upstream\r\nContent-Length: 2\r\n\r\n{}")
	if resp == nil {
		t.Fatalf("no response for the pass-through call; the integration returned %v", err)
	}
	wantRecordedBody(t, resp, body)
	if window[0].Spec.HTTPResp != nil || !window[0].HasSpilledResponse() {
		t.Fatal("serving wrote the loaded response into the pooled mock")
	}
}

// A response that cannot be loaded leaves its mock unconsumed, and the error
// names the mock. The integration used to consume the mock before loading the
// response, so a failed load still reported the mock as served.
func TestHTTPDoesNotConsumeAMockWhoseResponseCannotBeLoaded(t *testing.T) {
	at := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	disk, mm, _ := stageSpilled(t, at, spilledHTTPMock("big-response", "GET", "/big", at))
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}

	resp, _, err := serveOne(t, httpint.New(zap.NewNop()), mm, "GET /big HTTP/1.1\r\nHost: upstream\r\n\r\n")
	if resp != nil {
		t.Fatalf("served %d although the response could not be loaded", resp.StatusCode)
	}
	if err == nil || !strings.Contains(err.Error(), `"big-response"`) {
		t.Fatalf("integration error = %v, want one naming mock \"big-response\"", err)
	}
	if consumed := mm.GetConsumedMocks(); len(consumed) != 0 {
		t.Fatalf("consumed %v although nothing was served", consumed)
	}
	if left, _ := mm.GetPerTestMocksInWindow(); len(left) != 1 {
		t.Fatalf("per-test tier holds %d mocks after the failed serve, want the unconsumed 1", len(left))
	}
}

// A pass-through mock whose response cannot be loaded is skipped, and the
// application gets the synthetic 200 the path falls back to. The failure says
// so once per mock name, at a level a default run shows.
func TestHTTPPassThroughWarnsOnceWhenAResponseCannotBeLoaded(t *testing.T) {
	at := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	disk, mm, _ := stageSpilled(t, at, spilledHTTPMock("traces", "POST", "/v1/traces", at))
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zapcore.InfoLevel)
	h := httpint.New(zap.New(core))
	for i := 0; i < 2; i++ {
		resp, _, err := serveOne(t, h, mm, "POST /v1/traces HTTP/1.1\r\nHost: upstream\r\nContent-Length: 2\r\n\r\n{}")
		if resp == nil || resp.StatusCode != 200 {
			t.Fatalf("call %d: no synthetic 200 (%v)", i, err)
		}
	}
	warned := logs.FilterLevelExact(zapcore.WarnLevel).FilterField(zap.String("mock", "traces")).Len()
	if warned != 1 {
		t.Fatalf("warned %d times about the mock whose response could not be loaded, want once", warned)
	}
}
