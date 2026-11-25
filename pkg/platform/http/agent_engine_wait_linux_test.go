//go:build linux

package http

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// lagsDaemon is the engine's API of a daemon that takes the question of the
// agent container's removal at once, answers it when said is sent, and
// removes each volume asked (removed).
type lagsDaemon struct {
	kdocker.Client
	said    chan container.WaitResponse
	removed chan string
}

func (d *lagsDaemon) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	return d.said, make(chan error)
}

func (d *lagsDaemon) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, nil
}

func (d *lagsDaemon) VolumeRemove(_ context.Context, name string, _ bool) error {
	d.removed <- name
	return nil
}

// silentDaemon is the engine's API of a daemon that answers nothing: each
// question returns only once its context has ended.
type silentDaemon struct{ kdocker.Client }

func (silentDaemon) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	<-ctx.Done()
	failed := make(chan error, 1)
	failed <- ctx.Err()
	return nil, failed
}

func (silentDaemon) ContainerInspect(ctx context.Context, _ string) (container.InspectResponse, error) {
	<-ctx.Done()
	return container.InspectResponse{}, ctx.Err()
}

func (silentDaemon) VolumeRemove(ctx context.Context, _ string, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

// engineWaitRun starts a docker-mode agent run whose engine is docker (a
// script in dir) and api, waits until its `docker run` runs (it touches
// running), stops it, and starts the replay drain of its group (30 s of
// keploy's runnable time on clk without progress). It returns the run's
// exit and the drain's verdict (timedOut).
func engineWaitRun(t *testing.T, dir, docker string, api kdocker.Client, logger *zap.Logger) (clk *stall.ManualClock, exited <-chan error, drained <-chan bool) {
	t.Helper()
	fakeEngineForStartInDocker(t, dir, docker)
	clk = &stall.ManualClock{}
	boundClock = clk
	t.Cleanup(func() { boundClock = nil })
	a := &AgentClient{logger: logger, conf: &config.Config{InstallationID: "install-id"}, dockerClient: api}
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, stop := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	t.Cleanup(stop)
	exited, err := a.startAgent(ctx, true, models.SetupOptions{
		KeployContainer: engineWaitContainer, Mode: models.MODE_TEST, AgentPort: 43904, ProxyPort: 16793, DnsPort: 26793,
	})
	if err != nil {
		t.Fatalf("startAgent: %v", err)
	}
	awaitFile(t, "the agent's docker run", filepath.Join(dir, "running"))
	stop()
	verdict := make(chan bool, 1)
	go func() {
		_, timedOut := utils.DrainErrGroupProgressOn(clk, zap.NewNop(), "replay", grp, 30*time.Second, func() uint64 { return 0 })
		verdict <- timedOut
	}()
	return clk, exited, verdict
}

func awaitFile(t *testing.T, what, path string) {
	t.Helper()
	within(t, what, func() {
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}

// pass moves keploy's runnable time on clk by d, a second a step, letting
// what waits on it look between steps.
func pass(clk *stall.ManualClock, d time.Duration) {
	for passed := time.Duration(0); passed < d; passed += time.Second {
		time.Sleep(20 * time.Millisecond)
		clk.Advance(time.Second)
	}
}

// The stop of a docker-mode agent whose container exits at its SIGTERM, on a
// daemon that says so (`docker wait`) only after 40 s of keploy's runnable
// time: more than the replay drain's 30 s, within keploy's bound on the
// daemon's answer (daemonAnswerStall). The drain waited for the stop only
// while the agent reported its teardown, which it no longer could (`cp`
// fails): it gave up at 30 s, keploy exited, and the run's volumes were never
// removed. The run's wait on the engine (engineWait) holds the drains now:
// the drain waits, says at INFO what it waits for, and the run's volumes are
// removed once the daemon answers.
func TestDockerAgentStopOnALaggingDaemonRemovesTheRunsVolumes(t *testing.T) {
	dir := t.TempDir()
	answer := filepath.Join(dir, "answer")
	waited := filepath.Join(dir, "waited")
	core, logs := observer.New(zapcore.InfoLevel)
	daemon := &lagsDaemon{said: make(chan container.WaitResponse, 1), removed: make(chan string, 8)}
	clk, exited, drained := engineWaitRun(t, dir, `#!/bin/sh
case "$1 $2" in
"container run")
  : > "`+dir+`/running"
  while [ -d "`+dir+`" ] && [ ! -e "`+answer+`" ] && kill -0 "$PPID" 2>/dev/null; do sleep 0.05; done
  exit 0 ;;
esac
case "$1" in
kill) exit 0 ;;
cp) echo "Error response from daemon: Could not find the file" >&2; exit 1 ;;
wait)
  : > "`+waited+`"
  while [ -d "`+dir+`" ] && [ ! -e "`+answer+`" ]; do sleep 0.05; done
  echo 0; exit 0 ;;
esac
exit 0
`, daemon, zap.New(core))
	awaitFile(t, "the stop's docker wait", waited)
	pass(clk, 40*time.Second)
	select {
	case timedOut := <-drained:
		t.Fatalf("the replay drain ended (timedOut=%v) while the stop waited on the daemon's answer, within its bound", timedOut)
	case name := <-daemon.removed:
		t.Fatalf("volume %s asked to go before the daemon said the container exited", name)
	default:
	}
	if said := logs.FilterMessageSnippet("to stop keploy's agent container").FilterLevelExact(zapcore.InfoLevel).Len(); said != 1 {
		t.Fatalf("keploy said %d times at INFO what it waits for, want once", said)
	}
	// The daemon answers, within its bound.
	if err := os.WriteFile(answer, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	daemon.said <- container.WaitResponse{}
	within(t, "the run's end once the daemon answered", func() { <-exited })
	within(t, "the replay drain", func() {
		if <-drained {
			t.Error("the replay drain gave up on the run's end")
		}
	})
	want := kdocker.RunVolumeFor(kdocker.KeployTLSVolumeName, engineWaitContainer)
	select {
	case name := <-daemon.removed:
		if name != want {
			t.Fatalf("the run's end asked to remove %s first, want %s", name, want)
		}
	default:
		t.Fatalf("the run ended without asking to remove its volume %s", want)
	}
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if _, left := e.ContextMap()["volume"]; left || strings.Contains(e.Message, "has not answered keploy's stop") {
			t.Fatalf("a stop the daemon answered within its bound said at WARN: %s", e.Message)
		}
	}
}

// The stop of a docker-mode agent on a daemon that never answers it: one
// that answers nothing, and one that takes the stop's signal but never says
// the container exited (`docker wait`). The run ends at keploy's bound on the
// daemon's answer (daemonAnswerStall) of runnable time, not before and not
// one bound a question later; the drain waits for it; and it says at WARN
// each of its volumes it could not remove, with the command that removes it.
func TestDockerAgentStopOnASilentDaemonEndsAtTheBound(t *testing.T) {
	for _, tc := range []struct{ name, kill string }{
		{name: "the daemon answers nothing", kill: `while [ -d "$D" ]; do sleep 0.05; done; exit 1`},
		{name: "the daemon takes the signal but never says the container exited", kill: `exit 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			core, logs := observer.New(zapcore.InfoLevel)
			clk, exited, drained := engineWaitRun(t, dir, `#!/bin/sh
D="`+dir+`"
case "$1 $2" in
"container run")
  : > "$D/running"
  while [ -d "$D" ] && kill -0 "$PPID" 2>/dev/null; do sleep 0.05; done
  exit 0 ;;
esac
case "$1" in
kill) `+tc.kill+` ;;
cp) while [ -d "$D" ]; do sleep 0.05; done; exit 1 ;;
wait) while [ -d "$D" ]; do sleep 0.05; done; exit 1 ;;
esac
exit 0
`, silentDaemon{}, zap.New(core))
			pass(clk, daemonAnswerStall-time.Second)
			// Wall time passes, keploy's runnable time does not.
			for i := 0; i < 25; i++ {
				time.Sleep(20 * time.Millisecond)
				clk.Pass()
			}
			select {
			case <-exited:
				t.Fatalf("the run ended at %v of keploy's runnable time, before the bound %v", clk.Runnable(), daemonAnswerStall)
			case timedOut := <-drained:
				t.Fatalf("the replay drain ended (timedOut=%v) before the run did", timedOut)
			default:
			}
			pass(clk, time.Second)
			got := make(chan struct{})
			go func() { <-exited; close(got) }()
			within(t, "the run's end at the bound", func() {
				for {
					select {
					case <-got:
						return
					case <-time.After(20 * time.Millisecond):
						clk.Pass()
					}
				}
			})
			if clk.Runnable() != daemonAnswerStall {
				t.Fatalf("the run ended at %v of keploy's runnable time, want the bound %v", clk.Runnable(), daemonAnswerStall)
			}
			within(t, "the replay drain", func() {
				if <-drained {
					t.Error("the replay drain gave up on the run's end")
				}
			})
			var left []string
			for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
				name, ok := e.ContextMap()["volume"].(string)
				if !ok {
					continue
				}
				if !strings.HasSuffix(e.Message, "volume rm "+name) {
					t.Errorf("WARN %q does not end with the command that removes %s", e.Message, name)
				}
				if cause, _ := e.ContextMap()["cause"].(string); cause != errDaemonSilent.Error() {
					t.Errorf("the volume %s was left with cause %q, want %q", name, cause, errDaemonSilent)
				}
				left = append(left, name)
			}
			if want := kdocker.RunVolumeFor(kdocker.KeployTLSVolumeName, engineWaitContainer); len(left) == 0 || left[0] != want {
				t.Fatalf("said at WARN the volumes %v left, want %s first", left, want)
			}
		})
	}
}
