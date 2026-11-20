//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// heldMock is a recorded call whose app read the head and 5 bytes of the
// body: a timeout after the head, or a stopped_reading, marked or not as a
// call whose app ended its side first (models.Fault.AppEndedFirst).
func heldMock(kind string, marked bool) *models.Mock {
	f := &models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5, AppEndedFirst: marked}
	if kind == "stopped_reading" {
		f = &models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5, ContentLength: 10, AppEndedFirst: marked}
	}
	return &models.Mock{Name: "mock-held", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://pay.example/charge", ProtoMajor: 1, ProtoMinor: 1,
			Header: map[string]string{"Host": "pay.example"}},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Length": "10"}, Body: "0123456789"},
		Fault:    f,
	}}
}

// Through handleConnection, as the proxy serves an accepted connection (the
// socket as accepted, or tracked as the accept loop keeps it): an app that
// sends its request and then its FIN back to back, as a half-closing client
// does, reaches the proxy with its FIN before the answer is written or after
// it, as the scheduler has it. A held answer marked as one whose app ended
// its side first is held past that FIN either way: the app reads what it read
// at record, then nothing until its own timeout, and the connection is held
// until the run's end. So is it when the app keeps its side open and closes
// after the answer. Without the mark, a FIN after the answer lets the
// connection go, as before.
func TestReplay_ThroughTheProxyAMarkedHoldOutlastsTheAppsFIN(t *testing.T) {
	const request = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"
	const want = "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n01234"
	for _, kind := range []string{"timeout", "stopped_reading"} {
		for _, row := range []struct {
			name              string
			marked, halfClose bool
		}{
			{"marked/the app half-closes", true, true},
			{"marked/the app closes after the answer", true, false},
			{"not marked/the app closes after the answer", false, false},
		} {
			for _, tracked := range []bool{false, true} {
				for _, proven := range []string{"", "1"} {
					name := kind + "/" + row.name + map[bool]string{true: "/tracked", false: ""}[tracked] + "/KEPLOY_PROVEN=" + proven
					t.Run(name, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						app, src := acceptedConn(t)
						p := replayProxyTo(t, "10.96.0.9:80", models.OutgoingOptions{}, heldMock(kind, row.marked))
						p.errChannel = make(chan error, 8)
						p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: httpint.New(zap.NewNop())}
						p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
						var conn net.Conn = src
						if tracked {
							conn = (&inflight{}).track(src.Conn)
						}
						ctx, cancel := context.WithCancel(context.Background())
						t.Cleanup(cancel)
						done := make(chan error, 1)
						go func() { done <- p.handleConnection(ctx, conn) }()
						if _, err := io.WriteString(app, request); err != nil {
							t.Fatal(err)
						}
						if row.halfClose {
							if err := app.CloseWrite(); err != nil {
								t.Fatal(err)
							}
						}
						got := make([]byte, len(want))
						_ = app.SetReadDeadline(time.Now().Add(10 * time.Second)) // turns a hang into a failure
						if _, err := io.ReadFull(app, got); err != nil || string(got) != want {
							t.Fatalf("the app read %q (%v); want %q", got, err, want)
						}
						// The app's own timeout: nothing more, and no end, comes.
						_ = app.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
						if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
							t.Fatalf("after the answer the app read %d byte(s), %v; want nothing until its own timeout", n, err)
						}
						if !row.halfClose {
							if err := app.CloseWrite(); err != nil {
								t.Fatal(err)
							}
						}
						if !row.marked {
							select {
							case <-done:
							case <-time.After(10 * time.Second): // turns a hang into a failure
								t.Fatal("handleConnection did not return at the app's FIN after the answer")
							}
							return
						}
						select {
						case err := <-done:
							t.Fatalf("handleConnection returned %v past the app's FIN, "+
								"before the run's end; want the connection held", err)
						case <-time.After(200 * time.Millisecond):
						}
						cancel()
						select {
						case <-done:
						case <-time.After(10 * time.Second): // turns a hang into a failure
							t.Fatal("handleConnection did not return at the run's end")
						}
					})
				}
			}
		}
	}
}
