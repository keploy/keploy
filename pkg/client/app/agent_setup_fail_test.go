package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// setupFailApp is newRestartApp whose agent client fails to set up the agent
// compose started again (setUp's error), with what the retry did noted in
// order: the hold, the up, the setup, each resume of the hold with what it
// carried, and the run's end.
type setupFailApp struct {
	a      *App
	d      *restartingDocker
	mu     sync.Mutex
	got    []string
	resume []error
	ended  chan struct{}
}

var errRestartedAgentSetup = errors.New("the restarted agent refused its mocks (store-mocks): 500")

func newSetupFailApp(t *testing.T, setUp func(ctx context.Context) error) *setupFailApp {
	t.Helper()
	a, d, _ := newRestartApp(t)
	s := &setupFailApp{a: a, d: d, ended: make(chan struct{})}
	a.SetOnAgentRestart(func(ctx context.Context) error {
		s.note("setup")
		return setUp(ctx)
	})
	a.SetAgentReleaseHold(func(context.Context) func(error) {
		s.note("hold")
		return func(failed error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.resume = append(s.resume, failed)
			if failed != nil {
				s.got = append(s.got, "resume-failed")
			} else {
				s.got = append(s.got, "resume")
			}
		}
	})
	return s
}

func (s *setupFailApp) note(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, what)
}

func (s *setupFailApp) trail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.got, ",")
}

// end is the run's end on a halt: it notes itself, and lets the up return,
// as keploy's stop of the stack does.
func (s *setupFailApp) end() {
	s.note("end")
	close(s.ended)
}

// A FAILED SETUP OF THE RESTARTED AGENT ENDS THE RUN AT ONCE. keploy's stop
// of the stack took the agent, and the retry's `up` starts it again; the
// agent client cannot set it up again. The run ends on a halt that names the
// agent, the failed setup, its error and the next step, while the up still
// runs; first the releases the run asked for end with that error (the hold's
// resume carries it), none reaching an agent with none of the run's mocks or
// settings, and never held for good. Both KEPLOY_PROVEN modes.
func TestAFailedSetupOfTheRestartedAgentEndsTheRunWithNoReleaseInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			s := newSetupFailApp(t, func(context.Context) error { return errRestartedAgentSetup })
			s.d.set("keploy-v3-test", "agent-1", "t1", false)
			s.d.set("app", "app-1", "", false)
			s.a.retryComposeUp(context.Background(), s.end, func() utils.CmdError {
				s.note("up")
				s.d.set("keploy-v3-test", "agent-1", "t3", true)
				select {
				case <-s.ended:
				case <-time.After(30 * time.Second):
					t.Error("the run did not end within 30 s of the restarted agent's failed setup; the up ran on")
				}
				return utils.CmdError{}
			})
			if g := s.trail(); !strings.HasPrefix(g, "hold,up,setup,resume-failed,end") {
				t.Fatalf("the retry did %s; want the releases held before its up, ended with the setup's failure, then the run ended", g)
			}
			if len(s.resume) == 0 || !errors.Is(s.resume[0], errRestartedAgentSetup) {
				t.Fatalf("the hold was let go with %v; want it to end the releases with the failed setup", s.resume)
			}
			h := s.a.composeAppHalt()
			if h == nil || !errors.Is(h.agentSetup, errRestartedAgentSetup) {
				t.Fatalf("halt %+v; want the restarted agent's failed setup", h)
			}
			msg := h.Error()
			for _, want := range []string{`keploy's agent container "keploy-v3-test"`, "could not be set up again", errRestartedAgentSetup.Error(),
				"keploy does not release the application to it", "Next step: read keploy's log above for why the setup failed"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("the halt says %q; want it to say %q", msg, want)
				}
			}
			if e := h.appError(""); e.AppErrorType != models.ErrUnExpected || e.ExitCode != -1 || !errors.Is(e.Err, models.ErrAppExitedOnItsOwn) {
				t.Fatalf("the run ends %+v; want an unexpected end with no exit code of the app's", e)
			}
		})
	}
}

// A setup cut by the up's end (the watch's) is no failed setup: the up's end
// decides, as before. No halt is kept, the run is not ended, and the hold is
// let go with no failure. Both KEPLOY_PROVEN modes.
func TestASetupCutByTheUpsEndKeepsNoHaltInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			began := make(chan struct{})
			s := newSetupFailApp(t, func(ctx context.Context) error {
				close(began)
				<-ctx.Done()
				return ctx.Err()
			})
			s.d.set("keploy-v3-test", "agent-1", "t1", false)
			s.d.set("app", "app-1", "", false)
			s.a.retryComposeUp(context.Background(), s.end, func() utils.CmdError {
				s.note("up")
				s.d.set("keploy-v3-test", "agent-1", "t3", true)
				select {
				case <-began:
				case <-time.After(30 * time.Second):
					t.Error("the agent compose started again was never set up")
				}
				return utils.CmdError{}
			})
			if g := s.trail(); g != "hold,up,setup,resume" {
				t.Fatalf("the retry did %s; want the hold let go with no failure once its up ended", g)
			}
			if h := s.a.composeAppHalt(); h != nil {
				t.Fatalf("halt %+v kept for a setup the up's end cut; want none", h)
			}
		})
	}
}

// A halt kept before the restarted agent's setup failed stands, and its end
// is under way: the failed setup does not replace it, nor end the run again.
// The releases still end with the failure. Both KEPLOY_PROVEN modes.
func TestAFailedSetupLeavesAHaltKeptBeforeStandingInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			s := newSetupFailApp(t, func(context.Context) error { return errRestartedAgentSetup })
			s.d.set("keploy-v3-test", "agent-1", "t1", false)
			s.d.set("app", "app-1", "", false)
			before := &composeHalt{exitCode: 3}
			s.a.composeApp.halted.Store(before)
			retried := up(func() { s.d.set("keploy-v3-test", "agent-1", "t3", true) }, func() {}, func() {})
			s.a.retryComposeUp(context.Background(), s.end, func() utils.CmdError { s.note("up"); return retried() })
			if g := s.trail(); g != "hold,up,setup,resume-failed,resume" {
				t.Fatalf("the retry did %s; want the releases ended with the failure, and no second end of the run", g)
			}
			if h := s.a.composeAppHalt(); h != before {
				t.Fatalf("halt %+v; want the one kept before the failed setup", h)
			}
		})
	}
}

// A RUN WHOSE RESTARTED AGENT'S SETUP FAILED ENDS ON IT AT ONCE. A dependency
// the app depends on crashes before the app started, and keploy stops the
// stack, its agent with it, and retries the bring-up. The retried up starts
// the agent again, and the agent client cannot set it up again: run() ends
// on a halt naming the failed setup, its error and the next step, while the
// retried up still ran; keploy stops the stack (the restarted agent with it)
// before that up returns, the releases the run asked for end with the failure
// instead of being let go, and the bring-up is not retried again. Record and
// test, both KEPLOY_PROVEN modes.
func TestARunWhoseRestartedAgentsSetupFailedEndsOnItInEveryMode(t *testing.T) {
	doc := `
services:
  app:
    image: app
    depends_on: [flaky]
  runner:
    image: runner
  flaky:
    image: flaky
`
	for _, proven := range []string{"", "1"} {
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			t.Run("switch="+proven+"/"+string(mode), func(t *testing.T) {
				t.Setenv("KEPLOY_STUB_UP_CODE", "1")
				retryEnds := filepath.Join(t.TempDir(), "retry-ends")
				t.Setenv("KEPLOY_STUB_UP_RETRY_WAIT_FILE", retryEnds)
				var mu sync.Mutex
				var resumed []error
				r := startStackRun(t, proven, func(d *stackDaemon) bool {
					if d.running(runAgentName) {
						return false
					}
					// The retried up returns once keploy has stopped the
					// agent it started again.
					if strings.Contains(recordedLog(t, d.log), "UP-RETRIED") {
						_ = os.WriteFile(retryEnds, nil, 0o600)
					}
					return true
				}, func(a *App) {
					a.Mode, a.opts.Mode = mode, mode
					a.setComposeUserAbort("docker compose up --exit-code-from runner", composeDoc(t, doc))
					a.SetOnAgentRestart(func(context.Context) error { return errRestartedAgentSetup })
					a.SetAgentReleaseHold(func(context.Context) func(error) {
						return func(failed error) {
							mu.Lock()
							defer mu.Unlock()
							resumed = append(resumed, failed)
						}
					})
				})
				// Before the run's own cleanup, which waits for run() to return.
				t.Cleanup(func() { _ = os.WriteFile(retryEnds, nil, 0o600) })
				r.d.mu.Lock()
				r.d.cs[runAppName].status = "created"
				r.d.mu.Unlock()
				r.d.add(&abortContainer{name: "flaky-1", service: "flaky", status: "running"},
					&abortContainer{name: "runner-1", service: "runner", status: "running"})
				stream := r.watchStream(t)
				if err := r.d.exit("flaky-1", 1, "EXIT"); err != nil {
					t.Fatal(err)
				}
				stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().UnixNano(),
					Actor: events.Actor{ID: "flaky-1", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "flaky", "exitCode": "1"}}}
				eventually(t, "the bring-up is retried", func() bool { return trailAt(r.trail(t), "UP-RETRIED") >= 0 })
				// compose's retried up starts keploy's agent again.
				r.d.mu.Lock()
				r.d.cs[runAgentName].status = "running"
				r.d.mu.Unlock()
				e := r.result(t, "the restarted agent's failed setup")
				var h composeHalt
				if !errors.As(e.Err, &h) || !errors.Is(h.agentSetup, errRestartedAgentSetup) {
					t.Fatalf("run() ended %+v; want the halt of the restarted agent's failed setup", e)
				}
				if e.AppErrorType != models.ErrUnExpected || !strings.Contains(e.Err.Error(), "could not be set up again") || !strings.Contains(e.Err.Error(), "Next step:") {
					t.Fatalf("run() ended %+v; want an unexpected end naming the failed setup and the next step", e)
				}
				mu.Lock()
				first := append([]error(nil), resumed...)
				mu.Unlock()
				if len(first) == 0 || !errors.Is(first[0], errRestartedAgentSetup) {
					t.Fatalf("the hold was let go with %v; want the releases ended with the failed setup", first)
				}
				trail := r.trail(t)
				retried, stop, done := trailAt(trail, "UP-RETRIED"), -1, trailAt(trail, "UP-RETRY-DONE")
				for i := retried + 1; i < len(trail); i++ {
					if trail[i] == "STOP "+runAgentName {
						stop = i
						break
					}
				}
				if stop < 0 || done < 0 || stop > done {
					t.Fatalf("want the restarted agent stopped by keploy before the retried up returned:\n%s", strings.Join(trail, "\n"))
				}
				n := 0
				for _, l := range trail {
					if l == "UP-RETRIED" {
						n++
					}
				}
				if n != 1 {
					t.Fatalf("the bring-up was retried %d times; want once, the run ending on the failed setup:\n%s", n, strings.Join(trail, "\n"))
				}
			})
		}
	}
}

// A FAILED SETUP WITH NO HOLD ON STILL REFUSES EVERY RELEASE OF THE RUN. No
// release was held while the retry's `up` ran: compose recreated an agent that
// ran before the retry (a new container), or docker could not say which
// agent ran before and the agent the watch saw started after the retry
// began. The agent client cannot set that agent up again: the run ends on the
// halt as it does under a hold, and first every release of the run, in
// flight or later, ends with the failed setup (refuseAgentReleases), none
// reaching an agent with none of the run's mocks or settings. A setup that
// succeeds refuses nothing, and an App no agent client gave a refusal still
// ends the run. Both KEPLOY_PROVEN modes.
func TestAFailedSetupWithNoHoldStillRefusesEveryReleaseInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		for _, c := range []struct {
			name      string
			setUp     error
			noRefusal bool
			before    func(d *restartingDocker)
			restart   func(d *restartingDocker)
		}{
			{name: "recreated", setUp: errRestartedAgentSetup,
				before:  func(d *restartingDocker) { d.set("keploy-v3-test", "agent-1", "t1", true) },
				restart: func(d *restartingDocker) { d.set("keploy-v3-test", "agent-2", "t3", true) }},
			{name: "unknown", setUp: errRestartedAgentSetup,
				before: func(d *restartingDocker) {
					d.failing = agentBaselineTries
					d.set("keploy-v3-test", "agent-1", "t1", false)
				},
				restart: func(d *restartingDocker) {
					d.set("keploy-v3-test", "agent-1", time.Now().Add(time.Second).Format(time.RFC3339Nano), true)
				}},
			{name: "set up", setUp: nil,
				before:  func(d *restartingDocker) { d.set("keploy-v3-test", "agent-1", "t1", true) },
				restart: func(d *restartingDocker) { d.set("keploy-v3-test", "agent-2", "t3", true) }},
			{name: "no refusal set", setUp: errRestartedAgentSetup, noRefusal: true,
				before:  func(d *restartingDocker) { d.set("keploy-v3-test", "agent-1", "t1", true) },
				restart: func(d *restartingDocker) { d.set("keploy-v3-test", "agent-2", "t3", true) }},
		} {
			t.Run("switch="+proven+"/"+c.name, func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				setUp, tried := c.setUp, make(chan struct{})
				s := newSetupFailApp(t, func(context.Context) error { close(tried); return setUp })
				var refused []error
				if !c.noRefusal {
					s.a.SetAgentReleaseRefusal(func(failed error) {
						s.mu.Lock()
						defer s.mu.Unlock()
						refused = append(refused, failed)
						s.got = append(s.got, "refuse")
					})
				}
				c.before(s.d)
				s.d.set("app", "app-1", "", false)
				s.a.retryComposeUp(context.Background(), s.end, func() utils.CmdError {
					s.note("up")
					c.restart(s.d)
					// A failed setup ends the run while the up runs; one that
					// succeeds lets the up go on to its end.
					over := s.ended
					if setUp == nil {
						over = tried
					}
					select {
					case <-over:
					case <-time.After(30 * time.Second):
						t.Error("the agent compose started again was never set up, or its failed setup did not end the run")
					}
					return utils.CmdError{}
				})
				s.mu.Lock()
				got := append([]error(nil), refused...)
				s.mu.Unlock()
				if c.setUp == nil {
					if g := s.trail(); g != "up,setup" {
						t.Fatalf("the retry did %s; want the agent set up again, nothing held, nothing refused and the run not ended", g)
					}
					if h := s.a.composeAppHalt(); h != nil {
						t.Fatalf("halt %+v kept for a setup that succeeded; want none", h)
					}
					return
				}
				if c.noRefusal {
					if g := s.trail(); g != "up,setup,end" {
						t.Fatalf("the retry did %s with no refusal set; want the run ended on the failed setup", g)
					}
				} else if g := s.trail(); g != "up,setup,refuse,end" {
					t.Fatalf("the retry did %s; want no hold, the releases refused with the setup's failure, then the run ended", g)
				} else if len(got) != 1 || !errors.Is(got[0], errRestartedAgentSetup) {
					t.Fatalf("the releases were refused with %v; want the failed setup, once", got)
				}
				if h := s.a.composeAppHalt(); h == nil || !errors.Is(h.agentSetup, errRestartedAgentSetup) {
					t.Fatalf("halt %+v; want the restarted agent's failed setup", h)
				}
			})
		}
	}
}
