//go:build linux

package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A call record's full round reads after keploy test began test-2's capture
// window and before its filter gave test-2's request waits for the request
// (learn.Tracker.AwaitRequest). keploy test's filter gives the request, then
// settles every call made before (the settle reads the proxy's connections,
// State with answers), and only then installs test-2's view. The call's
// connection holds work for that settle while the call waits for the
// request, and after the request woke it until it is answered, however late
// the scheduler runs it: its handler is not reading the application. So the
// install never lands before the woken call reads the round again, and the
// call is decided between tests, as at once (the store-level row that
// installs test-2 under the woken call shows what it would read then: a
// miss naming the move, nothing learned).
func TestACallWokenByItsTestsRequestHoldsTheSettleUntilAnswered(t *testing.T) {
	p, mm := gapRound(t)
	tr := &inflight{}
	app, prox := acceptedPair(t, tr)
	// The call reaches the wait, then the scheduler runs it again only once
	// the row says: the request is given meanwhile.
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.learnTracker.OnAwait(func() {
		once.Do(func() {
			close(parked)
			<-release
		})
	})
	t.Cleanup(func() { p.learnTracker.OnAwait(nil) })
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	ctx := learn.WithTracker(models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError), p.learnTracker)
	ctx = integrations.WithRequestHolder(ctx, prox)
	go func() {
		_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, prox, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{Learn: true, LearnPool: true})
	}()
	if _, err := io.WriteString(app, roundPost(`{"item":"apricot"}`)); err != nil {
		t.Fatal(err)
	}
	<-parked
	if n, unknown := tr.State(true); n != 1 || len(unknown) != 0 {
		t.Fatalf("a call waiting for its test case's request: state %d %v, want 1 pending and known (the settle would install the view under it)", n, unknown)
	}
	p.TakeTestRequest(&models.TestRequest{Test: "test-2", Parts: []models.TestRequestPart{
		{Name: "header.Content-Type", Recorded: "application/json", Sent: "application/json"},
		{Name: "body", Recorded: `{"q":"books"}`, Sent: `{"q":"books"}`},
	}})
	if n, unknown := tr.State(true); n != 1 || len(unknown) != 0 {
		t.Fatalf("a call its test case's request woke, not yet run: state %d %v, want 1 pending and known", n, unknown)
	}
	close(release)
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != `{"order":0}` {
		t.Fatalf("the woken call run late: %d %s, want the pool's answer, 200 {\"order\":0}, as at once", resp.StatusCode, b)
	}
	if len(r3States(mm)["mock-pool"].ReqBodyNoise) == 0 {
		t.Fatal("the woken call run late learned nothing onto the pool; at once it learns the drifted item")
	}
	// Answered, its handler reads the application again: the settle ends.
	for {
		changed := tr.Changed()
		if n, _ := tr.State(true); n == 0 {
			break
		}
		<-changed
	}
}
