package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func clientOf(uri string) *AgentClient {
	cfg := &config.Config{}
	cfg.Agent.AgentURI = uri
	return New(zap.NewNop(), nil, cfg)
}

// A stream's request is made anew each time, for a restarted agent: a POST with
// its JSON body whole each time, a GET with neither body nor Content-Type.
func TestStreamRequestOfMakesTheSameRequestEachTime(t *testing.T) {
	ctx := context.Background()
	a := clientOf("http://agent")
	newRequest := a.streamRequestOf(http.MethodPost, "/outgoing", []byte(`{"a":1}`))
	for i := 0; i < 2; i++ {
		req, err := newRequest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if req.Method != http.MethodPost || req.URL.String() != "http://agent/outgoing" {
			t.Errorf("request %d: %s %s", i, req.Method, req.URL)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("request %d: Content-Type = %q", i, got)
		}
		b, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != `{"a":1}` {
			t.Errorf("request %d: body = %q", i, b)
		}
	}

	req, err := a.streamRequestOf(http.MethodGet, "/pcap", nil)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := req.Header["Content-Type"]; ok {
		t.Errorf("a GET's Content-Type = %q", got)
	}
	if req.Body != nil && req.Body != http.NoBody {
		t.Errorf("a GET has a body: %v", req.Body)
	}
}

// A request that cannot be made is an error, said as such, not a request that
// goes out without what it asks.
func TestIncomingRequestFailsAsItsRequestDoes(t *testing.T) {
	a := clientOf("http://bad host")
	if req, err := a.incomingRequest([]byte("{}"))(context.Background()); err == nil {
		t.Fatalf("no error, request %v", req)
	}
	_, err := a.GetIncoming(context.Background(), models.IncomingOptions{})
	if err == nil || !strings.Contains(err.Error(), "error creating request for incoming request") {
		t.Fatalf("GetIncoming: %v", err)
	}
}

// A request that cannot be made will not be made by trying again, unlike one
// that met an agent mid-start: reopenStream says so at once.
func TestReopenStreamDoesNotRetryARequestItCannotMake(t *testing.T) {
	boom := errors.New("no request")
	calls := 0
	_, err := clientOf("http://agent").reopenStream(context.Background(), "incoming", func(context.Context) (*http.Request, error) {
		calls++
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("tried %d times", calls)
	}
}
