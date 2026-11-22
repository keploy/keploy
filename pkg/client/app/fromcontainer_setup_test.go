package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// setupDocker answers the three calls SetupFromContainer makes and records what
// it was asked to create. The embedded interface is nil, so anything else
// panics rather than silently returning a zero value.
type setupDocker struct {
	docker.Client

	source   container.InspectResponse
	existing *container.InspectResponse // what a name lookup of the replacement finds
	removed  string

	createdName string
	createdCfg  *container.Config
	createdHost *container.HostConfig
	createErr   error
}

func (f *setupDocker) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	if strings.HasSuffix(ref, replacementSuffix) {
		if f.existing == nil {
			return container.InspectResponse{}, errors.New("No such container")
		}
		return *f.existing, nil
	}
	return f.source, nil
}

func (f *setupDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.removed = id
	return nil
}

func (f *setupDocker) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.createdName, f.createdCfg, f.createdHost = name, cfg, host
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	return container.CreateResponse{ID: "new-id"}, nil
}

func sourceInspect(tty bool) container.InspectResponse {
	return container.InspectResponse{
		Config: &container.Config{Image: "app:latest", Tty: tty},
		ContainerJSONBase: &container.ContainerJSONBase{
			ID: "src-id", Name: "/api", Image: "sha256:resolved",
			HostConfig: &container.HostConfig{},
		},
	}
}

func setupApp(d *setupDocker) *App {
	return &App{
		logger:          zap.NewNop(),
		docker:          d,
		kind:            utils.FromContainer,
		keployContainer: "keploy-v3-abcd",
		opts:            models.SetupOptions{FromContainer: "api", FromContainerWasRunning: true},
	}
}

// The TTY flag decides how the replacement's log stream is read, and the two
// framings are not interchangeable: reading a raw stream as multiplexed loses
// every line the app prints.
func TestSetupFromContainerCarriesTheTTYFlag(t *testing.T) {
	for _, tty := range []bool{true, false} {
		d := &setupDocker{source: sourceInspect(tty)}
		app := setupApp(d)
		if err := app.SetupFromContainer(context.Background()); err != nil {
			t.Fatalf("SetupFromContainer: %v", err)
		}
		if app.sourceTTY != tty {
			t.Errorf("sourceTTY = %v for a source with Tty=%v", app.sourceTTY, tty)
		}
	}
}

// The replacement takes a DERIVED name rather than reusing the source's, which
// is what lets the user's container be stopped instead of destroyed.
func TestSetupFromContainerNamesTheCopyAndKeepsTheOriginal(t *testing.T) {
	d := &setupDocker{source: sourceInspect(false)}
	app := setupApp(d)
	if err := app.SetupFromContainer(context.Background()); err != nil {
		t.Fatalf("SetupFromContainer: %v", err)
	}
	if d.createdName != "api"+replacementSuffix {
		t.Errorf("created %q, want the derived name", d.createdName)
	}
	if d.removed != "" {
		t.Errorf("removed %q; the user's container must only be stopped, never removed", d.removed)
	}
	if app.sourceContainer != "api" {
		t.Errorf("sourceContainer = %q, want the name without docker's leading slash", app.sourceContainer)
	}
	if app.replacementID != "new-id" {
		t.Errorf("replacementID = %q", app.replacementID)
	}
}

// The replacement's name is derived from the user's own container name, so a
// container they already own can hold it. Removing that to make room would be
// exactly the destructive behaviour this path exists to avoid.
func TestSetupFromContainerRefusesAnUnlabelledNameSquatter(t *testing.T) {
	d := &setupDocker{
		source: sourceInspect(false),
		existing: &container.InspectResponse{
			Config:            &container.Config{Labels: map[string]string{"com.example.mine": "yes"}},
			ContainerJSONBase: &container.ContainerJSONBase{ID: "theirs"},
		},
	}
	err := setupApp(d).SetupFromContainer(context.Background())
	if err == nil {
		t.Fatal("created over a container keploy did not make")
	}
	if !strings.Contains(err.Error(), "not created by keploy") {
		t.Errorf("error does not explain why: %v", err)
	}
	if d.removed != "" {
		t.Errorf("removed %q, which keploy does not own", d.removed)
	}
}

// One keploy left behind is keploy's to clear.
func TestSetupFromContainerClearsItsOwnLeftovers(t *testing.T) {
	d := &setupDocker{
		source: sourceInspect(false),
		existing: &container.InspectResponse{
			Config:            &container.Config{Labels: map[string]string{replacementLabel: "true"}},
			ContainerJSONBase: &container.ContainerJSONBase{ID: "stale"},
		},
	}
	if err := setupApp(d).SetupFromContainer(context.Background()); err != nil {
		t.Fatalf("SetupFromContainer: %v", err)
	}
	if d.removed != "stale" {
		t.Errorf("removed %q, want the stale replacement", d.removed)
	}
}

// Without a live agent there is no namespace to attach to, and the failure
// would otherwise surface from the daemon as an opaque create error.
func TestSetupFromContainerNeedsTheAgentAndAContainer(t *testing.T) {
	d := &setupDocker{source: sourceInspect(false)}

	noAgent := setupApp(d)
	noAgent.keployContainer = ""
	if err := noAgent.SetupFromContainer(context.Background()); err == nil {
		t.Error("accepted a run with no agent container to attach to")
	}

	noContainer := setupApp(d)
	noContainer.opts.FromContainer = ""
	if err := noContainer.SetupFromContainer(context.Background()); err == nil {
		t.Error("accepted a run with no container named")
	}
}
