package routes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/netnames"
	"go.keploy.io/server/v3/pkg/models"
)

// The agent hands the keploy CLI each of its asks for a fresh name list on one
// held stream, as the ask comes, and a list posted for an ask ends the wait of
// the DNS miss that made it. The stream stays open between asks, so the CLI
// is present while it lists the names.
func TestNetworkNamesAskStreamsEachAsk(t *testing.T) {
	store := netnames.New()
	a := &Agent{netNames: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+models.NetworkNamesAskPath, a.HandleNetworkNamesAsk)
	mux.HandleFunc("POST "+models.NetworkNamesPath, a.HandleNetworkNames)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+models.NetworkNamesAskPath+"?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the ask stream answered %s", res.Status)
	}
	for {
		present, changed := store.Present()
		if present {
			break
		}
		<-changed
	}
	lines := bufio.NewScanner(res.Body)
	var last uint64
	for _, name := range []string{"svc-one", "svc-two"} {
		done := make(chan struct{})
		go func() { defer close(done); store.Refresh(context.Background()) }()
		if !lines.Scan() {
			t.Fatalf("the stream ended before the ask for %s: %v", name, lines.Err())
		}
		var ask netnames.Ask
		if err := json.Unmarshal(lines.Bytes(), &ask); err != nil {
			t.Fatalf("an ask %q: %v", lines.Text(), err)
		}
		if ask.Asked != last+1 {
			t.Fatalf("handed ask %d, want %d", ask.Asked, last+1)
		}
		last = ask.Asked
		body, _ := json.Marshal(netnames.List{Answers: ask.Asked, Networks: []string{"stack_default"}, Names: []string{name}})
		post, err := srv.Client().Post(srv.URL+models.NetworkNamesPath, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = post.Body.Close()
		if post.StatusCode != http.StatusNoContent {
			t.Fatalf("the list for ask %d answered %s", ask.Asked, post.Status)
		}
		<-done
		if !store.Owns(name) {
			t.Fatalf("%s, on the list for ask %d, is not owned", name, ask.Asked)
		}
	}
}
