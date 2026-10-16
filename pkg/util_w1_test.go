package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// slowApp is an app whose answer comes only once the test releases it: a
// loaded machine, or an app that waits on a slow dependency.
func slowApp(t *testing.T) (url string, entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		in <- struct{}{}
		<-out
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	var released bool
	rel := func() {
		if !released {
			released = true
			close(out)
		}
	}
	t.Cleanup(func() { rel(); srv.Close() })
	return srv.URL, in, rel
}

// Without a bound of the user's (Unbounded, under the proven switch: row W1)
// a test waits for the app's answer, however long it takes: an answer that
// comes after what used to be keploy's default is the app's answer. With
// one, the bound holds, as before.
func TestAnUnboundedTestWaitsForTheAppsAnswer(t *testing.T) {
	for _, unbounded := range []bool{true, false} {
		url, entered, release := slowApp(t)
		tc := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/x"}}
		done := make(chan error, 1)
		go func() {
			_, err := SimulateHTTP(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 1, Unbounded: unbounded})
			done <- err
		}()
		<-entered
		if unbounded {
			// The app answers past the 1 s APITimeout (induced load).
			time.Sleep(1500 * time.Millisecond)
			release()
			if err := <-done; err != nil {
				t.Fatalf("an unbounded test gave up on the app's late answer: %v", err)
			}
			continue
		}
		if err := <-done; err == nil {
			t.Fatal("a test bounded by APITimeout waited past it")
		}
		release()
	}
}

// The same for a gRPC call: unbounded, the response drain ends on the app's
// end of the stream, however late it comes.
func TestAnUnboundedGRPCTestWaitsForTheAppsEndOfStream(t *testing.T) {
	hold := make(chan struct{})
	addr := startStreamingServer(t, &streamingServer{respond: [][]byte{pbMsg("first")}, holdOpen: hold})
	tc := grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}})
	done := make(chan error, 1)
	go func() {
		_, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 1, Unbounded: true})
		done <- err
	}()
	// The app ends its stream past the 1 s APITimeout (induced load).
	time.Sleep(1500 * time.Millisecond)
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("an unbounded gRPC test gave up on the app's late end of stream: %v", err)
	}
}
