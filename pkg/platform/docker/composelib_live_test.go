package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"errors"

	"github.com/docker/cli/cli"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// These exercise the compose library against a REAL docker daemon. They are the
// only place the in-process bring-up is proven end to end: everything above it
// is faked so it can run without a daemon, and the whole point of this code is
// that it replaces a `docker` binary that is no longer there to fall back on.
//
// Skipped when no daemon is reachable, and under -short.
func liveClient(t *testing.T) client.APIClient {
	t.Helper()
	if testing.Short() {
		t.Skip("live docker test skipped under -short")
	}
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx); err != nil {
		t.Skipf("no docker daemon reachable: %v", err)
	}
	return c
}

// runnerFor loads a project and guarantees it is torn down, however the test ends.
func runnerFor(t *testing.T, apiClient client.APIClient, project, yaml string) *ComposeRunner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := NewComposeRunner(ctx, apiClient, ComposeRunnerOptions{
		Content:     []byte(yaml),
		ProjectName: project,
		WorkingDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dcancel()
		_ = r.Down(dctx, time.Second)
	})
	return r
}

const busybox = "busybox:1.36"

// TestLiveUpBlocksUntilExitAndPropagatesTheExitCode is the central claim of the
// change: Up runs the stack in the foreground and reports the app's exit status.
//
// Both halves matter. If Up did not BLOCK, every caller would read the app as
// having exited immediately. If the exit code did not come back, a wrapped
// runner's failure would silently become a pass.
func TestLiveUpBlocksUntilExitAndPropagatesTheExitCode(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "echo hello-from-app; exit 7"]
`, busybox)

	r := runnerFor(t, apiClient, project, yaml)

	var out, errW bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	start := time.Now()
	err := r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeStop}, &out, &errW)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Up reported success for a container that exited 7; the exit status is lost.\nstdout:\n%s", out.String())
	}
	var status cli.StatusError
	if !errors.As(err, &status) {
		t.Fatalf("Up error = %T (%v), want cli.StatusError so exitCodeFromErr can read the code", err, err)
	}
	if status.StatusCode != 7 {
		t.Fatalf("exit code = %d, want 7", status.StatusCode)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Up returned in %s — it did not wait for the container, so callers would read the app as "+
			"having exited immediately", elapsed)
	}
	if !strings.Contains(out.String(), "hello-from-app") {
		t.Fatalf("the app's stdout never reached the log consumer; recentAppLogs and the CI scrapers read "+
			"this stream.\ngot:\n%s", out.String())
	}
}

// TestLiveCascadeIgnoreLetsAOneShotFinish pins the default that is NOT
// CascadeStop. A stack with a one-shot init/migration container must not be torn
// down the moment that container finishes.
func TestLiveCascadeIgnoreLetsAOneShotFinish(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	yaml := fmt.Sprintf(`
services:
  init:
    image: %s
    command: ["sh", "-c", "echo migrating; exit 0"]
  app:
    image: %s
    command: ["sh", "-c", "echo app-started; sleep 2; echo app-done"]
`, busybox, busybox)

	r := runnerFor(t, apiClient, project, yaml)

	var out, errW bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if err := r.Up(ctx, ComposeUpOptions{OnExit: api.CascadeIgnore}, &out, &errW); err != nil {
		t.Fatalf("Up failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "app-done") {
		t.Fatalf("the app was cut short when the one-shot init container exited; with CascadeIgnore it "+
			"must be allowed to finish.\ngot:\n%s", out.String())
	}
}

// TestLivePsCarriesTheIDAndOneOffLabel pins what the teardown sweep depends on.
// Without the id it has nothing to remove; without the oneoff label it cannot
// tell a `compose run` container (the user's) from a `compose up` one.
func TestLivePsCarriesTheIDAndOneOffLabel(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "exit 0"]
`, busybox)

	r := runnerFor(t, apiClient, project, yaml)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	var out, errW bytes.Buffer
	if err := r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeIgnore}, &out, &errW); err != nil {
		t.Fatalf("Up failed: %v\n%s", err, out.String())
	}

	states, err := r.Ps(ctx)
	if err != nil {
		t.Fatalf("Ps failed: %v", err)
	}
	if len(states) == 0 {
		t.Fatal("Ps returned nothing for a project whose container has exited; `-a` semantics are lost " +
			"and the dependency-failure classifier goes blind")
	}
	var found bool
	for _, s := range states {
		if s.Service != "app" {
			continue
		}
		found = true
		if s.ID == "" {
			t.Error("Ps row has no container ID; the teardown sweep has nothing to remove")
		}
		if s.Labels == nil {
			t.Error("Ps row has no labels; the sweep cannot tell a `compose run` one-off from a service container")
		}
		if _, ok := s.Labels["com.docker.compose.oneoff"]; !ok {
			t.Errorf("the com.docker.compose.oneoff label is missing from %v", s.Labels)
		}
	}
	if !found {
		t.Fatalf("no row for service app in %+v", states)
	}
}

// TestLiveDownRemovesTheProjectButNotNamedVolumes pins both halves of teardown.
// The shell-out it replaces ran a bare `docker compose down --timeout 1`, which
// does NOT take volumes; removing them would silently discard a user's database
// between test-sets.
func TestLiveDownRemovesTheProjectButNotNamedVolumes(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "exit 0"]
    volumes:
      - data:/data
volumes:
  data: {}
`, busybox)

	r := runnerFor(t, apiClient, project, yaml)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	var out, errW bytes.Buffer
	if err := r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeIgnore}, &out, &errW); err != nil {
		t.Fatalf("Up failed: %v\n%s", err, out.String())
	}

	if err := r.Down(ctx, time.Second); err != nil {
		t.Fatalf("Down failed: %v", err)
	}

	states, err := r.Ps(ctx)
	if err != nil {
		t.Fatalf("Ps after down: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("Down left %d container(s) behind: %+v", len(states), states)
	}

	vols, err := apiClient.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project)),
	})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	if len(vols.Volumes) == 0 {
		t.Fatal("Down removed the project's named volume; the shell-out it replaces did not, and a user's " +
			"database would be discarded between test-sets")
	}
	// Clean up the volume this test deliberately left behind.
	for _, v := range vols.Volumes {
		_ = apiClient.VolumeRemove(ctx, v.Name, true)
	}
}

// TestLiveServiceImagesListsEveryImage covers the replacement for
// `docker compose config --images`, which the enterprise runner uses to decide
// whether a failed pull actually left anything missing.
func TestLiveServiceImagesListsEveryImage(t *testing.T) {
	apiClient := liveClient(t)
	yaml := fmt.Sprintf(`
services:
  a:
    image: %s
  b:
    image: %s
  c:
    image: alpine:3.20
`, busybox, busybox)

	r := runnerFor(t, apiClient, "keployimages", yaml)

	got := r.ServiceImages()
	want := map[string]bool{busybox: true, "alpine:3.20": true}
	if len(got) != len(want) {
		t.Fatalf("ServiceImages() = %v, want the %d distinct images de-duplicated", got, len(want))
	}
	for _, img := range got {
		if !want[img] {
			t.Fatalf("ServiceImages() = %v, unexpected %q", got, img)
		}
	}
}

// TestLiveProjectNameFollowsComposePrecedence pins that the library addresses
// the SAME project the equivalent `docker compose` invocation would. Getting it
// wrong is silent: `down` leaves the stack running and `ps` reports nothing.
func TestLiveProjectNameFollowsComposePrecedence(t *testing.T) {
	apiClient := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	yamlNoName := fmt.Sprintf("services:\n  app:\n    image: %s\n", busybox)
	yamlNamed := fmt.Sprintf("name: from-yaml\nservices:\n  app:\n    image: %s\n", busybox)

	dir := t.TempDir() // basename is random, so assert against it directly
	base := strings.ToLower(strings.ReplaceAll(dirBase(dir), "_", "-"))

	cases := []struct {
		name, yaml, explicit, env, want string
	}{
		{"explicit -p wins", yamlNamed, "from-flag", "", "from-flag"},
		{"env beats the document", yamlNamed, "", "from-env", "from-env"},
		{"document beats the directory", yamlNamed, "", "", "from-yaml"},
		{"directory is the fallback", yamlNoName, "", "", base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(composeProjectNameEnv, tc.env)
			} else {
				t.Setenv(composeProjectNameEnv, "")
			}
			r, err := NewComposeRunner(ctx, apiClient, ComposeRunnerOptions{
				Content:     []byte(tc.yaml),
				ProjectName: tc.explicit,
				WorkingDir:  dir,
			})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if r.ProjectName() != tc.want {
				t.Fatalf("ProjectName() = %q, want %q", r.ProjectName(), tc.want)
			}
		})
	}
}

func dirBase(p string) string {
	parts := strings.Split(strings.TrimRight(p, string(os.PathSeparator)), string(os.PathSeparator))
	return parts[len(parts)-1]
}
