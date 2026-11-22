package replay

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

// The health poll's connection to the app closes once the wait for the app
// is over. Kept open, it is one more of keploy's connections the app holds as
// it stops: an app that waits for its open connections then (Python's
// ThreadingTCPServer joins each one's thread) cannot stop, and keploy's stop
// at a set's end kills it, and the set is not judged.
func TestTheHealthPollsConnectionClosesOnceTheAppIsReady(t *testing.T) {
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.Start()
	defer srv.Close()

	if !waitForAppReady(context.Background(), zap.NewNop(), newCfg(srv.URL, 5*time.Second), httpProbeTarget{}) {
		t.Fatal("the app answered its health URL, and keploy did not find it ready")
	}
	// The bound only turns a connection left open into a failure.
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("the health poll's connection is still open to the app once the app was found ready")
	}
}
