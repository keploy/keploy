package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// Each consumed-mocks read keeps what the agent's answer named of the set's
// calls it passed through as the app's own: in one spelling, never one that
// is no ip:port, at most models.MaxOwnListenerCalls; an answer that names none
// (an older agent's, or the next set's) leaves none.
func TestGetConsumedMocks_KeepsTheCallsTheAgentPassedThroughAsTheAppsOwn(t *testing.T) {
	var header []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range header {
			w.Header().Add(models.OwnListenerCallsHeader, h)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	if got := a.OwnListenerCalls(); got != nil {
		t.Fatalf("before any read: %v, want none", got)
	}
	read := func() []string {
		t.Helper()
		if _, err := a.GetConsumedMocks(context.Background()); err != nil {
			t.Fatal(err)
		}
		return a.OwnListenerCalls()
	}
	header = []string{"127.0.0.1:6100, [::ffff:172.18.0.3]:6100,localhost:6100", "no-port,[::1]:80"}
	if got, want := read(), []string{"127.0.0.1:6100", "172.18.0.3:6100", "[::1]:80"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	many := make([]string, 0, models.MaxOwnListenerCalls+3)
	for i := 0; i < cap(many); i++ {
		many = append(many, fmt.Sprintf("10.0.0.1:%d", 1000+i))
	}
	header = []string{strings.Join(many, ",")}
	if got := len(read()); got != models.MaxOwnListenerCalls {
		t.Fatalf("kept %d destinations, want the bound %d", got, models.MaxOwnListenerCalls)
	}
	header = nil
	if got := read(); len(got) != 0 {
		t.Fatalf("an answer that names none left %v", got)
	}
}
