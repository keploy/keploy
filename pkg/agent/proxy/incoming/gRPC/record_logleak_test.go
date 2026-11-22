package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// Incoming gRPC passthrough capture logs the call's :path by its shape plus
// its per-session tag, never the raw path segments.
func TestRecordIncoming_LogsGRPCMethodAsShapeAndSessionTag(t *testing.T) {
	const s = logleak.Canary
	grpcPath := "/" + s + ".SecretService/GetToken"

	keys := models.NewLogKeySession()
	ctx, cancel := context.WithCancel(models.WithLogKeySession(
		syncMock.NewContext(context.Background(), syncMock.New(nil)),
		keys,
	))
	defer cancel()

	logger, logs := logleak.Observe()
	cliApp, cliProxy := net.Pipe()
	appProxy, appApp := net.Pipe()
	t.Cleanup(func() {
		for _, c := range []net.Conn{cliApp, cliProxy, appProxy, appApp} {
			_ = c.Close()
		}
	})

	tcCh := make(chan *models.TestCase, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecordIncoming(ctx, logger, cliProxy, appProxy, tcCh, 50051, "", false, false)
	}()

	reqBytes := append([]byte(http2.ClientPreface),
		append(
			headersFrameBytes(t, 1, []hpack.HeaderField{
				{Name: ":method", Value: "POST"},
				{Name: ":scheme", Value: "http"},
				{Name: ":path", Value: grpcPath},
				{Name: ":authority", Value: "localhost:50051"},
				{Name: "content-type", Value: "application/grpc"},
			}, false),
			dataFrameBytes(t, 1, true, grpcPayloadBytes([]byte{0x0a, 0x04, 'p', 'i', 'n', 'g'}))...,
		)...,
	)
	respBytes := append(
		append(
			headersFrameBytes(t, 1, []hpack.HeaderField{
				{Name: ":status", Value: "200"},
				{Name: "content-type", Value: "application/grpc"},
			}, false),
			dataFrameBytes(t, 1, false, grpcPayloadBytes([]byte{0x0a, 0x04, 'p', 'o', 'n', 'g'}))...,
		),
		headersFrameBytes(t, 1, []hpack.HeaderField{
			{Name: "grpc-status", Value: "0"},
			{Name: "grpc-message", Value: ""},
		}, true)...,
	)

	go func() {
		_, _ = cliApp.Write(reqBytes)
		_, _ = io.ReadFull(cliApp, make([]byte, len(respBytes)))
		_ = cliApp.Close()
	}()
	if _, err := io.ReadFull(appApp, make([]byte, len(reqBytes))); err != nil {
		t.Fatal(err)
	}
	if _, err := appApp.Write(respBytes); err != nil {
		t.Fatal(err)
	}
	_ = appApp.Close()
	<-done

	said := logs.FilterMessage("captured gRPC test case (passthrough)").All()
	if len(said) != 1 {
		t.Fatalf("precondition: want 1 passthrough capture debug line, got %d (%+v)", len(said), logs.All())
	}
	fields := said[0].ContextMap()
	if got := fields["method"]; got != "/{}/{}" {
		t.Errorf("method field = %q, want /{}/{}", got)
	}
	wantTag := models.URLPathTag(ctx, http.MethodPost, &url.URL{Path: grpcPath})
	if got := fields["path_tag"]; got != wantTag {
		t.Errorf("path_tag field = %q, want %q", got, wantTag)
	}
	logleak.Check(t, logs, s, "incoming gRPC passthrough capture")
}
