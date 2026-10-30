package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// silentAgent is an agent that takes every read of its capture losses and
// never answers it (a wedged agent): asked counts the reads it took, and
// left is closed when a read's connection ended (the read was given up).
func silentAgent(t *testing.T) (a *AgentClient, asked, left chan struct{}) {
	t.Helper()
	asked, left = make(chan struct{}, 8), make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capturelosses" && r.URL.Path != "/capturelosses/uncharged" {
			http.NotFound(w, r)
			return
		}
		asked <- struct{}{}
		<-r.Context().Done()
		left <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(srv.CloseClientConnections) // first: a read still out ends, so Close does not wait on it
	return &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}, asked, left
}

// onCaptureLossClock bounds the client's capture-loss reads on a clock the
// test drives, for the test.
func onCaptureLossClock(t *testing.T, grace time.Duration) *stall.ManualClock {
	t.Helper()
	clk := &stall.ManualClock{}
	oldClk, oldStall := captureLossClock, captureLossStall
	captureLossClock, captureLossStall = clk, grace
	t.Cleanup(func() { captureLossClock, captureLossStall = oldClk, oldStall })
	return clk
}

// captureLossReads are the client's two reads of the agent's capture losses.
func captureLossReads(a *AgentClient) map[string]func(context.Context) (models.CaptureLosses, error) {
	return map[string]func(context.Context) (models.CaptureLosses, error){
		"GetCaptureLosses":          a.GetCaptureLosses,
		"GetUnchargedCaptureLosses": a.GetUnchargedCaptureLosses,
	}
}

type lossRead struct {
	losses models.CaptureLosses
	err    error
}

// A wedged agent that takes a read of its capture losses and never answers
// it held the run for good: the client has no timeout, and record's per-test
// read is made on a context the stop does not end, so a CI job hung until
// its runner killed it. The read is given up once keploy had the bound of
// its own runnable time with no answer, an error naming it (never a count),
// and its connection ends. Wall time keploy could not run (a starved host)
// never counts.
func TestACaptureLossReadAWedgedAgentNeverAnswersIsGivenUp(t *testing.T) {
	const grace = 50 * time.Millisecond
	clk := onCaptureLossClock(t, grace)
	a, asked, left := silentAgent(t)
	for name, read := range captureLossReads(a) {
		t.Run(name, func(t *testing.T) {
			done := make(chan lossRead, 1)
			go func() {
				l, err := read(context.WithoutCancel(context.Background()))
				done <- lossRead{l, err}
			}()
			<-asked
			// Four graces of wall time, no CPU: keploy could not run.
			for end := time.Now().Add(4 * grace); time.Now().Before(end); {
				waitLook(t, clk)
				clk.Pass()
			}
			waitLook(t, clk)
			select {
			case r := <-done:
				t.Fatalf("given up for wall time keploy could not run: %+v", r)
			default:
			}
			clk.Advance(grace)
			select {
			case r := <-done:
				if !errors.Is(r.err, errAgentSilent) || r.losses != nil || !strings.Contains(r.err.Error(), "capture losses") || !strings.Contains(r.err.Error(), grace.String()+" of its own time to run") {
					t.Fatalf("a wedged agent's read: %+v; want errAgentSilent naming the read and its bound, no count", r)
				}
			case <-time.After(time.Minute): // only turns a hang into a failure
				t.Fatal("HANG: a read the agent never answered was not given up once keploy had its bound of runnable time")
			}
			select {
			case <-left:
			case <-time.After(time.Minute): // only turns a hang into a failure
				t.Fatal("the given-up read's connection to the agent never ended")
			}
		})
	}
}

// The Ctrl+C escape: a read made on a context the stop does not end (record's
// per-test read) ends at the interrupt that gives a stop's waits up
// (utils.StopNow), before its bound, an error and no count; the run's own
// context ending ends it too.
func TestACaptureLossReadEndsAtTheInterruptThatGivesTheStopUp(t *testing.T) {
	clk := onCaptureLossClock(t, time.Hour)
	a, asked, left := silentAgent(t)
	for name, read := range captureLossReads(a) {
		t.Run(name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
			done := make(chan lossRead, 1)
			go func() {
				l, err := read(context.WithoutCancel(context.Background()))
				done <- lossRead{l, err}
			}()
			<-asked
			waitLook(t, clk)
			utils.MarkInterrupted() // the stop
			utils.OfferStopNow()    // "press Ctrl+C again to stop now"
			if !utils.MarkInterruptedAgain() {
				t.Fatal("precondition: the second interrupt did not stop now")
			}
			select {
			case r := <-done:
				if r.err == nil || r.losses != nil || errors.Is(r.err, errAgentSilent) || !strings.Contains(r.err.Error(), "at the interrupt") {
					t.Fatalf("a read at the interrupt: %+v; want an error naming the interrupt, no count", r)
				}
			case <-time.After(time.Minute): // only turns a hang into a failure
				t.Fatal("HANG: the interrupt that gives the stop up did not end the read")
			}
			<-left
		})
	}
	utils.ResetInterruptedAgain()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan lossRead, 1)
	go func() {
		l, err := a.GetCaptureLosses(ctx)
		done <- lossRead{l, err}
	}()
	<-asked
	cancel()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) || r.losses != nil {
			t.Fatalf("a read whose context ended: %+v; want context.Canceled, no count", r)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("HANG: a read whose context ended went on")
	}
	<-left
}

// An agent that answers is read as before, however late: the bound is on no
// answer, and an answer within it is the count.
func TestACaptureLossReadAnAgentAnswersIsItsCount(t *testing.T) {
	clk := onCaptureLossClock(t, time.Hour)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set(models.AgentInstanceHeader, "agent-1")
		_, _ = w.Write([]byte(`[{"kind":"unix_unseen","count":2}]`))
	}))
	t.Cleanup(srv.Close)
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	type got struct {
		losses models.CaptureLosses
		agent  string
		err    error
	}
	done := make(chan got, 1)
	go func() {
		l, ag, err := a.GetCaptureLossesAndAgent(context.Background())
		done <- got{l, ag, err}
	}()
	waitLook(t, clk)
	clk.Advance(59 * time.Minute)
	close(release)
	r := <-done
	if r.err != nil || r.agent != "agent-1" || len(r.losses) != 1 || r.losses[0].Count != 2 {
		t.Fatalf("an agent that answered within the bound: %+v; want its count", r)
	}
}
