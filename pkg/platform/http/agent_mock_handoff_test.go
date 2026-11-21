package http_test

// Guards the widest of the three places a trailing mock could be lost during
// `keploy mock record` teardown: a mock already decoded into the CLI's own
// address space, discarded because the capture context was cancelled before the
// consumer took it.
//
// The consumer persists on an uncancellable context, so it can always accept;
// the only reason it might not have yet is that it is busy writing the previous
// mock — and the first InsertMock does mkdir + create + YAML encode + flush,
// which is precisely when the last mock of a recording arrives.

import (
	"context"
	"encoding/gob"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func TestGetOutgoing_DeliversDecodedMocksWhenConsumerIsBusyAndContextCancelled(t *testing.T) {
	const want = 2

	// An agent that hands over both mocks at once and then holds the stream
	// open, like a real recording session mid-run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := gob.NewEncoder(w)
		for i := 0; i < want; i++ {
			if err := enc.Encode(models.Mock{Name: "mock-" + string(rune('0'+i)), Kind: models.HTTP}); err != nil {
				return
			}
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	client := httpclient.New(zap.NewNop(), nil, cfg)
	out, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatalf("GetOutgoing: %v", err)
	}

	// Cancel while the consumer below is still busy. Against the old
	// `select { case <-ctx.Done(): return nil; ... }` hand-off this discarded
	// whatever had been decoded but not yet taken — silently.
	time.AfterFunc(50*time.Millisecond, cancel)

	got := 0
	for range out {
		// Model a slow InsertMock: the consumer is not at the channel when the
		// cancel lands.
		time.Sleep(150 * time.Millisecond)
		got++
	}
	_ = grp.Wait()

	if got != want {
		t.Fatalf("consumer received %d mock(s), want %d — a decoded mock was discarded because the capture context was cancelled before the busy consumer could take it", got, want)
	}
}

// Note on what this pins. The fix is two redundant belts - a buffered hand-off
// and an offer that no longer races the context - and either alone is enough
// here, so mutating just one of them leaves this test green; only removing both
// (the original code) fails it. That redundancy is deliberate rather than
// accidental, but it is worth knowing the test does not isolate them.
//
// Isolating the offer would need the buffer full so the hand-off blocks, and
// that cannot be done through this seam: cancelling aborts the HTTP request, so
// a backlog large enough to fill the buffer is also a backlog still on the wire
// and legitimately unrecoverable. The contract is "never discard a mock already
// decoded", not "un-cancel the request" - and the reason there is no backlog at
// the real cancel is that Record now drains to quiescence first
// (pkg/service/mock/record.go).

// A consumer that takes nothing for a while (the recorder's append waiting on
// a mock file a whole-file rewrite holds) is back-pressure: the mocks the agent
// sent meanwhile all reach it, in order, once it takes them again, and the
// stream stays open for the mocks after them. Before, the reader dropped the
// mock it held once the consumer had taken nothing for its grace and ended the
// mock stream for good, while the test-case stream went on.
func TestGetOutgoing_KeepsTheStreamThroughAConsumerThatTakesNothingForAWhile(t *testing.T) {
	const sent = 1100 // past the client's hand-off buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := gob.NewEncoder(w)
		for i := 0; i < sent; i++ {
			if err := enc.Encode(models.Mock{Name: fmt.Sprintf("mock-%d", i), Kind: models.HTTP}); err != nil {
				return
			}
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	out, err := httpclient.New(zap.NewNop(), nil, cfg).GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatalf("GetOutgoing: %v", err)
	}
	// The consumer takes nothing until the reader holds a full buffer and a
	// mock it cannot hand on.
	deadline := time.Now().Add(time.Minute)
	for len(out) < cap(out) {
		if time.Now().After(deadline) {
			t.Fatalf("the hand-off buffer never filled: %d of %d", len(out), cap(out))
		}
		time.Sleep(time.Millisecond)
	}
	// Past any bound of wall time short of a long pass a live stream's reader
	// might give the consumer: a whole-file pass held it for 49 s.
	time.Sleep(2500 * time.Millisecond)

	for i := 0; i < sent; i++ {
		select {
		case m, ok := <-out:
			if !ok {
				t.Fatalf("the mock stream ended after %d of %d mocks: the reader gave the slow consumer up", i, sent)
			}
			if want := fmt.Sprintf("mock-%d", i); m.Name != want {
				t.Fatalf("mock %d is %q, want %q", i, m.Name, want)
			}
		case <-time.After(time.Minute):
			t.Fatalf("mock %d never came", i)
		}
	}
	select {
	case _, ok := <-out:
		t.Fatalf("the stream gave more than the agent sent, or ended (open=%v), while the agent held it open", ok)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("a mock came after the stream's client ended it")
		}
	case <-time.After(time.Minute):
		t.Fatal("the mock stream did not end once its client ended it")
	}
	_ = grp.Wait()
}
