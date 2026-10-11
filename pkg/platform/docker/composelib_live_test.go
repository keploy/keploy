//go:build composelib && !darwin

// Tagged, because every test here drives the compose library itself —
// NewComposeRunner and the api.Compose backend behind it. Without the tag
// that backend is the refusal stub, so these would fail for the absence of
// the thing they exist to pin rather than for anything about its behaviour.

package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"errors"

	"github.com/docker/cli/cli"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
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
	c, err := NewComposeAPIClient()
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("no docker daemon reachable: %v", err)
	}
	return c
}

// runnerFor loads a project and guarantees it is torn down, however the test ends.
func runnerFor(t *testing.T, apiClient client.APIClient, project, yaml string) *ComposeRunner {
	t.Helper()
	return runnerWith(t, apiClient, ComposeRunnerOptions{
		Content:     []byte(yaml),
		ProjectName: project,
		WorkingDir:  t.TempDir(),
	})
}

// runnerWith is runnerFor with every option the caller's to set.
func runnerWith(t *testing.T, apiClient client.APIClient, opts ComposeRunnerOptions) *ComposeRunner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := NewComposeRunner(ctx, apiClient, opts)
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

	var out, errW syncBuffer
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

	var out, errW syncBuffer
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

// TestLiveUpSurvivesTeardownRemovingItsContainer pins the teardown that took
// keploy down after a compose replay whose tests had all passed. While Up is
// attached, keploy's teardown runs Down, which stops the app and removes it at
// once. compose's monitor, watching the project for Up, inspects the container
// whose death it was just told of, and on a loaded machine the removal comes
// first: compose v2.40.3 then read the state of a container the inspect did not
// find, a nil pointer (pkg/compose/monitor.go:150), and the panic killed the
// process running Up. Here lateInspect makes that order certain. compose v5
// reads a container it can no longer find as one that is not coming back, so
// Up ends the way the stack did: with the app's exit status.
func TestLiveUpSurvivesTeardownRemovingItsContainer(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	// Exits 3 on the SIGTERM Down stops it with: a status nothing but the app
	// could have reported.
	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "trap 'exit 3' TERM; sleep 300 & wait"]
`, busybox)

	late := &lateInspect{APIClient: apiClient}
	r := runnerFor(t, late, project, yaml)
	// Teardown's own runner, on the plain client, so that its Down never waits
	// on an inspect lateInspect is holding.
	teardown := runnerFor(t, apiClient, project, yaml)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var out, errW syncBuffer
	upErr := make(chan error, 1)
	go func() {
		upErr <- r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeStop}, &out, &errW)
	}()

	id := waitRunning(ctx, t, teardown, "app", upErr, &out)

	late.hold(id)
	if err := teardown.Down(ctx, time.Second); err != nil {
		t.Fatalf("Down: %v", err)
	}

	select {
	case err := <-upErr:
		var status cli.StatusError
		if !errors.As(err, &status) {
			t.Fatalf("Up = %T (%v), want the app's exit status as a cli.StatusError\n%s", err, err, out.String())
		}
		if status.StatusCode != 3 {
			t.Fatalf("exit code = %d, want 3: the app exited 3 on the teardown's SIGTERM", status.StatusCode)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Up was still running a minute after the teardown removed its only container")
	}
	// Up ends only once compose has handled the app's death, which it
	// inspects; so by now the inspect has run, after the removal.
	if late.held.Load() == 0 {
		t.Fatal("compose never inspected the app after the teardown, so this did not test the order it exists for")
	}
}

// TestLiveTeardownDownWhileUpIsAttached runs keploy's teardown as keploy runs
// it: Down on the same runner whose Up is still attached (app.go's teardown
// reuses the cached runner). The test above needs a second runner to hold
// compose's inspect; this one is the production shape, without the hold.
func TestLiveTeardownDownWhileUpIsAttached(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "trap 'exit 3' TERM; sleep 300 & wait"]
`, busybox)
	r := runnerFor(t, apiClient, project, yaml)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var out, errW syncBuffer
	upErr := make(chan error, 1)
	go func() {
		upErr <- r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeStop}, &out, &errW)
	}()
	waitRunning(ctx, t, r, "app", upErr, &out)

	if err := r.Down(ctx, time.Second); err != nil {
		t.Fatalf("Down: %v", err)
	}
	select {
	case err := <-upErr:
		var status cli.StatusError
		if !errors.As(err, &status) || status.StatusCode != 3 {
			t.Fatalf("Up = %T (%v), want cli.StatusError 3: the app exited 3 on the teardown's SIGTERM\n%s",
				err, err, out.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Up was still running a minute after the teardown")
	}
}

// waitRunning waits until service's container is running and returns its id,
// failing if Up (reporting on upErr) returns first.
func waitRunning(ctx context.Context, t *testing.T, r *ComposeRunner, service string, upErr <-chan error, out *syncBuffer) string {
	t.Helper()
	for {
		select {
		case err := <-upErr:
			t.Fatalf("Up returned before the teardown: %v\n%s", err, out.String())
		case <-ctx.Done():
			t.Fatalf("%s never started", service)
		case <-time.After(200 * time.Millisecond):
		}
		states, err := r.Ps(ctx)
		if err != nil {
			t.Fatalf("Ps: %v", err)
		}
		for _, st := range states {
			if st.Service == service && st.State == "running" {
				return st.ID
			}
		}
	}
}

// lateInspect is a client whose inspects of one container, once held, wait
// until that container has been removed: the order that crashed compose v2,
// made certain.
type lateInspect struct {
	client.APIClient
	id   atomic.Value // string
	held atomic.Int32
}

func (c *lateInspect) hold(id string) { c.id.Store(id) }

func (c *lateInspect) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if held, _ := c.id.Load().(string); held != "" && id == held {
		c.held.Add(1)
		for {
			list, err := c.APIClient.ContainerList(ctx, client.ContainerListOptions{
				All: true, Filters: make(client.Filters).Add("id", id),
			})
			if err != nil {
				return client.ContainerInspectResult{}, err
			}
			if len(list.Items) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return client.ContainerInspectResult{}, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return c.APIClient.ContainerInspect(ctx, id, opts)
}

// TestLiveProgressReportsWhatComposeDoes pins the lines `docker compose`
// prints about the project's containers and images, which the shell-out
// printed and the CI logs are read by ("Container ... Started", "... Removed").
// compose v5 prints none of them unless it is given somewhere to: its
// default is to report nothing, which is what it did here before
// composeProgress.
func TestLiveProgressReportsWhatComposeDoes(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "exit 0"]
`, busybox)

	// The writer given wins over the variable that would otherwise send the
	// progress to stdout.
	t.Setenv("COMPOSE_STATUS_STDOUT", "true")

	var progress syncBuffer
	r := runnerWith(t, apiClient, ComposeRunnerOptions{
		Content:     []byte(yaml),
		ProjectName: project,
		WorkingDir:  t.TempDir(),
		Progress:    &progress,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if err := r.Pull(ctx, false); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !progressHas(progress.String(), busybox, "Pulled") {
		t.Fatalf("Pull reported nothing about %s:\n%s", busybox, progress.String())
	}

	var out, errW syncBuffer
	if err := r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeStop}, &out, &errW); err != nil {
		t.Fatalf("Up: %v\n%s", err, out.String())
	}
	if err := r.Down(ctx, time.Second); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, want := range []string{"Started", "Removed"} {
		if !progressHas(progress.String(), "Container", want) {
			t.Errorf("no %q container line in the progress:\n%s", want, progress.String())
		}
	}
}

// progressHas reports whether one line of progress names both words.
func progressHas(progress, a, b string) bool {
	for _, line := range strings.Split(progress, "\n") {
		if strings.Contains(line, a) && strings.Contains(line, b) {
			return true
		}
	}
	return false
}

// syncBuffer is a bytes.Buffer compose's goroutines can write while the test
// reads it: Up writes the app's output from its log goroutines and its
// monitor's, so a plain bytes.Buffer is a data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLiveUpReturnsWhenADependencyFails pins what keploy's retry of a crashed
// dependency (isTransientComposeDependencyFailure) is built on: when a service
// the app depends on exits non-zero, Up returns an error at once, leaving the
// app created and never started and the dependency exited non-zero. compose
// v5.6.0 broke the first half: its Up drops start's dependency error once the
// dependency's exit has cascaded, then waits for an app container that will
// never start, so Up hangs until its context ends.
func TestLiveUpReturnsWhenADependencyFails(t *testing.T) {
	apiClient := liveClient(t)
	for _, tc := range []struct{ name, condition, healthcheck string }{
		{"completed successfully", "service_completed_successfully", ""},
		{"healthy", "service_healthy", `
    healthcheck:
      test: ["CMD-SHELL", "exit 1"]
      interval: 1s`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)
			yaml := fmt.Sprintf(`
services:
  dep:
    image: %[1]s
    command: ["sh", "-c", "sleep 1; exit 7"]%[2]s
  app:
    image: %[1]s
    command: ["sh", "-c", "exit 0"]
    depends_on:
      dep:
        condition: %[3]s
`, busybox, tc.healthcheck, tc.condition)
			r := runnerFor(t, apiClient, project, yaml)

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			var out, errW syncBuffer
			start := time.Now()
			// keploy's own flags (ensureComposeExitOnAppFailure).
			err := r.Up(ctx, ComposeUpOptions{ExitCodeFrom: "app", OnExit: api.CascadeStop}, &out, &errW)
			if took := time.Since(start); took > 60*time.Second {
				t.Fatalf("Up took %s to report a dependency that exited in a second (err %v)", took, err)
			}
			if err == nil {
				t.Fatalf("Up reported success though app's dependency exited 7\n%s", out.String())
			}

			states, err := r.Ps(ctx)
			if err != nil {
				t.Fatalf("Ps: %v", err)
			}
			got := map[string]ServiceState{}
			for _, st := range states {
				got[st.Service] = st
			}
			if got["app"].State != "created" {
				t.Errorf("app is %q, want created: it must never have started", got["app"].State)
			}
			if got["dep"].State != "exited" || got["dep"].ExitCode != 7 {
				t.Errorf("dep is %q (exit %d), want exited 7", got["dep"].State, got["dep"].ExitCode)
			}
		})
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
	var out, errW syncBuffer
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
	var out, errW syncBuffer
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

	vols, err := apiClient.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", "com.docker.compose.project="+project),
	})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	if len(vols.Items) == 0 {
		t.Fatal("Down removed the project's named volume; the shell-out it replaces did not, and a user's " +
			"database would be discarded between test-sets")
	}
	// Clean up the volume this test deliberately left behind.
	for _, v := range vols.Items {
		_, _ = apiClient.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
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
