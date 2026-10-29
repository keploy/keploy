package netnames

import (
	"context"
	"sync"
	"testing"
)

func TestOwnsAndMayOwn(t *testing.T) {
	s := New()
	s.Apply(List{Networks: []string{"Keploy-Network"}, Names: []string{"DB", "db.keploy-network."}})
	for _, n := range []string{"db", "DB.", "db.Keploy-Network."} {
		if !s.Owns(n) {
			t.Errorf("Owns(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"riso-unrecorded", "db.corp.example", "", "."} {
		if s.Owns(n) {
			t.Errorf("Owns(%q) = true, want false", n)
		}
	}
	for n, want := range map[string]bool{
		"late-svc.": true, "late-svc.keploy-network.": true,
		"late-svc.other-network.": false, "api.example.com.": false, "": false,
	} {
		if got := s.MayOwn(n); got != want {
			t.Errorf("MayOwn(%q) = %v, want %v", n, got, want)
		}
	}
}

// waitFor returns once cond, read under s.mu, holds.
func waitFor(s *Store, cond func() bool) {
	for {
		s.mu.Lock()
		ok, ch := cond(), s.changed
		s.mu.Unlock()
		if ok {
			return
		}
		<-ch
	}
}

// stream holds a CLI's stream of asks after after open until ctx ends,
// handing each ask to the returned channel, and returns once the store
// counts it. The channel is closed when the stream ends.
func stream(t *testing.T, s *Store, ctx context.Context, after uint64) <-chan Ask {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	out := make(chan Ask, 16)
	s.mu.Lock()
	before := s.streams
	s.mu.Unlock()
	go func() {
		defer close(out)
		s.Serve(ctx, after, func(a Ask) error {
			out <- a
			return nil
		})
	}()
	waitFor(s, func() bool { return s.streams > before })
	return out
}

// With no CLI holding a stream of asks nothing would answer one, so Refresh
// does not wait: the miss is answered by keploy at once.
func TestRefreshWithNoCLIReturns(t *testing.T) {
	s := New()
	s.Refresh(context.Background())
	if s.asked != 0 {
		t.Fatalf("asked = %d with no CLI to answer, want 0", s.asked)
	}
}

// A miss asks once and waits for the list that answers it; concurrent misses
// (glibc's A and AAAA) join the one ask, as does a miss while the CLI lists
// the names for it.
func TestRefreshWaitsForTheAnsweringList(t *testing.T) {
	s := New()
	s.Apply(List{Networks: []string{"net"}, Names: []string{"db"}})
	asks := stream(t, s, context.Background(), 0)

	var wg sync.WaitGroup
	refresh := func() {
		wg.Add(1)
		go func() { defer wg.Done(); s.Refresh(context.Background()) }()
	}
	refresh()
	refresh()
	a := <-asks
	if a.Asked != 1 {
		t.Fatalf("the CLI was handed ask %d, want 1", a.Asked)
	}
	waitFor(s, func() bool { return s.waiting == 2 })
	refresh()
	waitFor(s, func() bool { return s.waiting == 3 })
	s.Apply(List{Answers: 1, Networks: []string{"net"}, Names: []string{"db", "late-svc"}})
	wg.Wait()
	if s.asked != 1 {
		t.Fatalf("asked = %d, want the 1 ask every concurrent miss joined", s.asked)
	}
	if !s.Owns("late-svc") {
		t.Fatal("the answering list's late-svc is not owned")
	}
	// The stream hands the next miss's ask on the same stream.
	refresh()
	if a := <-asks; a.Asked != 2 {
		t.Fatalf("the next miss handed ask %d, want 2", a.Asked)
	}
	s.Apply(List{Answers: 2, Networks: []string{"net"}, Names: []string{"db"}})
	wg.Wait()
}

// A CLI that goes away after it took an ask answers nothing: the Refresh
// waiting for that ask returns, and the next one does not wait.
func TestRefreshEndsWhenTheCLIGoesWithAnAsk(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	asks := stream(t, s, ctx, 0)
	done := make(chan struct{})
	go func() { defer close(done); s.Refresh(context.Background()) }()
	if a := <-asks; a.Asked != 1 {
		t.Fatalf("handed ask %d, want 1", a.Asked)
	}
	cancel()
	<-done
	for range asks {
	}
	s.Refresh(context.Background())
	if s.asked != 1 {
		t.Fatalf("asked = %d after the CLI went, want 1", s.asked)
	}
}

// A CLI that could not list the names for an ask opens its next stream after
// that ask: the Refresh waiting for it returns, though a stream is still open.
func TestAListTheCLICouldNotGiveSettlesItsAsk(t *testing.T) {
	s := New()
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := stream(t, s, ctx1, 0)
	done := make(chan struct{})
	go func() { defer close(done); s.Refresh(context.Background()) }()
	a := <-first
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	next := stream(t, s, ctx2, a.Asked)
	<-done
	if s.Owns("late-svc") {
		t.Fatal("a name no list gave is owned")
	}
	// The next miss asks afresh, on the open stream.
	go s.Refresh(context.Background())
	if b := <-next; b.Asked != a.Asked+1 {
		t.Fatalf("the next miss handed ask %d, want %d", b.Asked, a.Asked+1)
	}
	s.Apply(List{Answers: a.Asked + 1})
}

// A CLI that counted the asks of an agent process this one replaced is
// handed this one's next ask.
func TestServeAfterAReplacedAgent(t *testing.T) {
	s := New()
	asks := stream(t, s, context.Background(), 7)
	go s.Refresh(context.Background())
	if a := <-asks; a.Asked != 1 {
		t.Fatalf("handed ask %d, want 1", a.Asked)
	}
	s.Apply(List{Answers: 1})
}
