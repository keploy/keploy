package app

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// stuckHooks stands in for a before-setup hook whose work never ends on its
// own, like an enterprise hook waiting on a docker daemon that does not
// answer: it tells the test it has started, then returns only when its ctx is
// done.
type stuckHooks struct {
	entered chan struct{}
}

// runKey marks the run's ctx, so a hook handed any other ctx fails the test at
// once instead of hanging it: one without the mark, or one nothing can cancel
// (a context.WithoutCancel of the run's keeps the mark).
type runKey struct{}

var errNotTheRunsCtx = errors.New("the hook was not given the run's ctx")

func (h stuckHooks) wait(ctx context.Context) error {
	close(h.entered)
	if ctx.Value(runKey{}) == nil || ctx.Done() == nil {
		return errNotTheRunsCtx
	}
	<-ctx.Done()
	return ctx.Err()
}

func (h stuckHooks) BeforeDockerSetup(ctx context.Context, cmd string) (string, error) {
	return cmd, h.wait(ctx)
}

func (h stuckHooks) BeforeDockerComposeSetup(ctx context.Context, _ *docker.Compose, _ string) (bool, error) {
	return false, h.wait(ctx)
}

// composeOnly answers the compose calls SetupCompose makes before its hook,
// without a daemon; any other call panics on the nil Client.
type composeOnly struct {
	docker.Client
}

func (composeOnly) FindContainerInComposeFiles(paths []string, _ string) (*docker.ComposeServiceInfo, error) {
	return &docker.ComposeServiceInfo{AppServiceName: "app", ComposePath: paths[0], Compose: &docker.Compose{}}, nil
}

func (composeOnly) FindContainerInCompose(c *docker.Compose, _ string) (*docker.ComposeServiceInfo, error) {
	return &docker.ComposeServiceInfo{AppServiceName: "app", Compose: c}, nil
}

func (composeOnly) ModifyComposeForAgent(*docker.Compose, models.SetupOptions, string) error {
	return nil
}

// The before-setup hooks run under the run's ctx, so cancelling the run (the
// user's Ctrl-C cancels it) ends a hook stuck on the docker daemon, for every
// way keploy readies a docker app. They used to get a ctx nothing cancels.
func TestSetupHooksEndWhenTheRunIsCancelled(t *testing.T) {
	prev := HookImpl
	t.Cleanup(func() { HookImpl = prev })

	composeFile := t.TempDir() + "/docker-compose.yaml"
	cases := []struct {
		name  string
		setup func(ctx context.Context) error
	}{
		{"docker run", func(ctx context.Context) error {
			a := NewApp(zap.NewNop(), "docker run --rm orders", nil, models.SetupOptions{KeployContainer: "keploy-v3"})
			return a.Setup(ctx)
		}},
		{"docker compose", func(ctx context.Context) error {
			a := NewApp(zap.NewNop(), "docker compose -f "+composeFile+" up", composeOnly{}, models.SetupOptions{Container: "orders"})
			return a.Setup(ctx)
		}},
		{"in-memory compose", func(ctx context.Context) error {
			a := NewApp(zap.NewNop(), "docker compose up", composeOnly{}, models.SetupOptions{Container: "orders", InMemoryCompose: []byte("services:\n  app:\n    image: busybox\n")})
			return a.Setup(ctx)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hooks := stuckHooks{entered: make(chan struct{})}
			HookImpl = hooks
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), runKey{}, true))
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.setup(ctx) }()

			select {
			case <-hooks.entered:
			case err := <-done:
				t.Fatalf("setup returned %v before it reached the hook", err)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("setup returned %v, want the run's cancellation", err)
			}
		})
	}
}
