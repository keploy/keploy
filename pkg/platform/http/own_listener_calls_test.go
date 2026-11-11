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

// The set's end asks the agent again without draining anything: what it names
// is kept in the same spelling and bound as a consumed-mocks answer's; an
// agent that cannot say (404, 501) leaves what the last read named, and one
// that fails is an error that leaves it too.
func TestReadOwnListenerCalls_KeepsWhatTheAgentNamesWithoutDraining(t *testing.T) {
	var (
		header []string
		path   string
		code   = http.StatusOK
		body   = `[]`
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		for _, h := range header {
			w.Header().Add(models.OwnListenerCallsHeader, h)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	header = []string{"127.0.0.1:6100"}
	if _, err := a.GetConsumedMocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	header = nil
	body = `["[::ffff:172.18.0.3]:6100","localhost:6100","10.0.0.9:8080"]`
	if err := a.ReadOwnListenerCalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != models.OwnListenerCallsPath {
		t.Fatalf("asked %q, want %q", path, models.OwnListenerCallsPath)
	}
	if got, want := a.OwnListenerCalls(), []string{"172.18.0.3:6100", "10.0.0.9:8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	many := make([]string, 0, models.MaxOwnListenerCalls+3)
	for i := 0; i < cap(many); i++ {
		many = append(many, fmt.Sprintf("%q", fmt.Sprintf("10.0.0.1:%d", 1000+i)))
	}
	body = "[" + strings.Join(many, ",") + "]"
	if err := a.ReadOwnListenerCalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(a.OwnListenerCalls()); got != models.MaxOwnListenerCalls {
		t.Fatalf("kept %d destinations, want the bound %d", got, models.MaxOwnListenerCalls)
	}
	body = `["10.0.0.9:8080"]`
	if err := a.ReadOwnListenerCalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		code    int
		wantErr bool
	}{{http.StatusNotFound, false}, {http.StatusNotImplemented, false}, {http.StatusInternalServerError, true}} {
		code, body = c.code, `{"error":"x"}`
		if err := a.ReadOwnListenerCalls(context.Background()); (err != nil) != c.wantErr {
			t.Fatalf("status %d: error %v, want an error %v", c.code, err, c.wantErr)
		}
		if got := a.OwnListenerCalls(); !reflect.DeepEqual(got, []string{"10.0.0.9:8080"}) {
			t.Fatalf("status %d left %v, want what the last read named", c.code, got)
		}
	}
}
