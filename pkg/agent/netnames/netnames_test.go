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

// heldAsk starts the CLI's held ask and returns once the store counts it.
func heldAsk(t *testing.T, s *Store, ctx context.Context, after uint64) <-chan Ask {
	t.Helper()
	out := make(chan Ask, 1)
	go func() {
		if a, ok := s.AwaitAsk(ctx, after); ok {
			out <- a
		}
		close(out)
	}()
	waitFor(s, func() bool { return s.present })
	return out
}

// With no CLI holding an ask nothing would answer one, so Refresh does not
// wait: the miss is answered by keploy at once.
func TestRefreshWithNoCLIReturns(t *testing.T) {
	s := New()
	s.Refresh(context.Background())
	if s.asked != 0 {
		t.Fatalf("asked = %d with no CLI to answer, want 0", s.asked)
	}
}

// A miss asks once and waits for the list that answers it; concurrent misses
// (glibc's A and AAAA) join the one ask.
func TestRefreshWaitsForTheAnsweringList(t *testing.T) {
	s := New()
	s.Apply(List{Networks: []string{"net"}, Names: []string{"db"}})
	asks := heldAsk(t, s, context.Background(), 0)

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
	// A miss while the CLI lists the names (it holds no ask then) joins the
	// ask it lists them for.
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
}

// A CLI that goes away answers nothing: a waiting Refresh returns, and the
// next one does not wait.
func TestRefreshEndsWhenTheCLIGoes(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	asks := heldAsk(t, s, ctx, 0)
	cancel()
	if _, ok := <-asks; ok {
		t.Fatal("a held ask whose CLI went away answered")
	}
	s.Refresh(context.Background())
}

// A CLI that counted the asks of an agent process this one replaced is
// handed this one's next ask.
func TestAwaitAskAfterAReplacedAgent(t *testing.T) {
	s := New()
	asks := heldAsk(t, s, context.Background(), 7)
	go s.Refresh(context.Background())
	if a := <-asks; a.Asked != 1 {
		t.Fatalf("handed ask %d, want 1", a.Asked)
	}
	s.Apply(List{Answers: 1})
}
