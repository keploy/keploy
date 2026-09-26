// Package docker: in-process docker-compose bring-up.
//
// Everything here exists so keploy can start a compose stack it generated
// ITSELF without a `docker` binary on PATH. It deliberately does NOT replace
// the shell-out for a compose file the user supplied: that command is theirs,
// it may carry flags and shell syntax we do not model, and on their machine the
// CLI is present anyway. The seam is `App.composeContent` — set only when
// keploy built the stack in memory.
package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/client"
)

// composeProjectNameEnv is the variable compose itself consults when no
// -p/--project-name is given. Named here rather than imported from
// compose-go/internal/consts, which is not importable.
const composeProjectNameEnv = "COMPOSE_PROJECT_NAME"

// ComposeRunner drives one compose project through the compose library.
type ComposeRunner struct {
	svc     api.Compose
	project *composetypes.Project
}

// composeLogConsumer forwards container output to the same writers the
// shell-out path handed to the child process.
//
// The CLI prefixes each line with the service name; reproduce that, because
// both the sample-app CI scripts and recentAppLogs read this output, and a
// silent reformat would look like a behaviour change in the app itself.
type composeLogConsumer struct {
	out io.Writer
	err io.Writer
}

func (c composeLogConsumer) Log(containerName, message string) {
	fmt.Fprintf(c.out, "%s  | %s\n", containerName, message)
}

func (c composeLogConsumer) Err(containerName, message string) {
	fmt.Fprintf(c.err, "%s  | %s\n", containerName, message)
}

func (c composeLogConsumer) Status(container, msg string) {
	fmt.Fprintf(c.out, "%s  %s\n", container, msg)
}

// newComposeCLI builds the command.Cli the compose service needs, backed by the
// Docker client keploy already holds.
//
// Initialize must be called even though the API client is supplied: DockerCli's
// lazy init resolves the docker endpoint BEFORE it notices a client is already
// set, and with no options it resolves the current context name to "" — which
// is not the default context, so it goes looking for a context store that was
// never created and fails with "no context store initialized". Client() reacts
// to that by calling os.Exit(1), which would take keploy down with no error.
//
// The context is pinned to "default" rather than autodetected so this object
// can never resolve to a different daemon than keploy's own client, which is
// built with client.FromEnv and honours DOCKER_HOST alone (docker contexts are
// not consulted). Every compose operation runs through the supplied client
// regardless; the endpoint only supplies metadata.
func newComposeCLI(apiClient client.APIClient) (command.Cli, error) {
	dockerCli, err := command.NewDockerCli(command.WithAPIClient(apiClient))
	if err != nil {
		return nil, fmt.Errorf("build compose docker cli: %w", err)
	}

	opts := cliflags.NewClientOptions()
	opts.Context = command.DefaultContextName
	if err := dockerCli.Initialize(opts); err != nil {
		return nil, fmt.Errorf("initialise compose docker cli: %w", err)
	}

	// Force the lazy init now, while a failure is still an error we can return.
	// DockerEndpoint deliberately does not exit on failure, unlike Client().
	if endpoint := dockerCli.DockerEndpoint(); endpoint.Host == "" {
		return nil, fmt.Errorf("could not resolve a docker endpoint for the compose stack")
	}
	return dockerCli, nil
}

// ComposeRunnerOptions describes the project exactly as the equivalent
// `docker compose` invocation would have resolved it.
type ComposeRunnerOptions struct {
	// Content is the in-memory compose YAML (what `-f -` piped on stdin).
	Content []byte
	// ProjectName is the value of -p/--project-name, empty when not given.
	ProjectName string
	// WorkingDir is --project-directory, or the process cwd when not given.
	WorkingDir string
}

// NewComposeRunner builds a runner for an in-memory compose document.
//
// apiClient is the Docker Engine client keploy already holds — the same one
// docker.Client embeds — so this adds no second connection and no second
// configuration source.
//
// The command.Cli it builds is an in-process object, not a subprocess: no
// `docker` binary is executed. It does read $DOCKER_CONFIG/config.json to
// resolve auths / credHelpers exactly as the CLI does — including exec'ing
// docker-credential-<name>, which is a separate binary and must still be on
// PATH for ECR/GCR/ACR.
func NewComposeRunner(ctx context.Context, apiClient client.APIClient, opts ComposeRunnerOptions) (*ComposeRunner, error) {
	if len(opts.Content) == 0 {
		return nil, fmt.Errorf("compose content is empty")
	}

	workingDir := strings.TrimSpace(opts.WorkingDir)
	if workingDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve compose working directory: %w", err)
		}
		workingDir = wd
	}
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve compose working directory: %w", err)
	}

	dockerCli, err := newComposeCLI(apiClient)
	if err != nil {
		return nil, err
	}

	details := composetypes.ConfigDetails{
		WorkingDir: absWorkingDir,
		// Filename is required for error messages and for relative-path
		// resolution; Content being set is what stops the loader reading disk.
		ConfigFiles: []composetypes.ConfigFile{{Filename: syntheticComposeFilename, Content: opts.Content}},
		Environment: environmentMap(),
	}

	project, err := loader.LoadWithContext(ctx, details, projectNameOption(opts.ProjectName, absWorkingDir))
	if err != nil {
		return nil, fmt.Errorf("load compose project: %w", err)
	}

	// The CLI records which files a project came from; the config-files label is
	// built from this, so set it for the synthetic document too.
	project.ComposeFiles = []string{syntheticComposeFilename}

	project, err = asComposeCLIWouldShapeIt(project)
	if err != nil {
		return nil, fmt.Errorf("prepare compose project: %w", err)
	}

	return &ComposeRunner{svc: compose.NewComposeService(dockerCli), project: project}, nil
}

// syntheticComposeFilename names the in-memory document. It is never read from
// disk; it appears in loader error messages and in the config-files label.
const syntheticComposeFilename = "docker-compose.yaml"

// asComposeCLIWouldShapeIt finishes the project the way `docker compose` does
// between loading and running it.
//
// The loader alone is NOT enough, and the gap is silent rather than loud: the
// per-service CustomLabels below are what stamp com.docker.compose.project /
// .service / .oneoff onto every container compose creates. Without them Create
// reports success and then NOTHING can find the containers again — `up` fails
// with `service "x" has no container to start`, `ps` returns nothing, and `down`
// has nothing to remove, so the containers leak. Verified against a live daemon.
func asComposeCLIWouldShapeIt(project *composetypes.Project) (*composetypes.Project, error) {
	project, err := project.WithServicesEnabled()
	if err != nil {
		return nil, err
	}

	for name, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  project.WorkingDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			// "False" here, as in the CLI: only `compose run` overrides it, and
			// the teardown sweep reads this label to leave a user's one-off alone.
			api.OneoffLabel: "False",
		}
		project.Services[name] = s
	}

	// Select every service (nil = all), then drop networks/volumes/configs and
	// secrets no selected service references — the same two steps `up` takes.
	project, err = project.WithSelectedServices(nil)
	if err != nil {
		return nil, err
	}
	return project.WithoutUnnecessaryResources(), nil
}

// projectNameOption reproduces compose's own project-name precedence:
// -p/--project-name, then $COMPOSE_PROJECT_NAME, then a `name:` in the YAML,
// then the normalized base name of the working directory.
//
// The first two are set imperatively; the directory-derived fallback is not,
// which is precisely what lets a `name:` in the document win over it (see
// loader.projectName). Getting this wrong does not fail loudly — it silently
// addresses a DIFFERENT project, so `down` would leave this stack running and
// `ps` would report nothing.
func projectNameOption(projectName, absWorkingDir string) func(*loader.Options) {
	return func(o *loader.Options) {
		if name := strings.TrimSpace(projectName); name != "" {
			o.SetProjectName(name, true)
			return
		}
		if name := strings.TrimSpace(os.Getenv(composeProjectNameEnv)); name != "" {
			o.SetProjectName(name, true)
			return
		}
		o.SetProjectName(loader.NormalizeProjectName(filepath.Base(absWorkingDir)), false)
	}
}

// environmentMap is the process environment in the shape the loader wants, so
// ${VAR} interpolation resolves the same way it did for the child process.
func environmentMap() composetypes.Mapping {
	env := composetypes.Mapping{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// ProjectName is the name the stack's containers, network and volumes are
// prefixed with.
func (r *ComposeRunner) ProjectName() string {
	if r == nil || r.project == nil {
		return ""
	}
	return r.project.Name
}

// ServiceImages lists the image reference of every service in the project, in
// declaration order and de-duplicated. It is the in-process equivalent of
// `docker compose config --images`.
func (r *ComposeRunner) ServiceImages() []string {
	if r == nil || r.project == nil {
		return nil
	}
	seen := map[string]struct{}{}
	images := make([]string, 0, len(r.project.Services))
	for _, name := range r.project.ServiceNames() {
		svc, err := r.project.GetService(name)
		if err != nil {
			continue
		}
		image := strings.TrimSpace(svc.Image)
		if image == "" {
			continue
		}
		if _, dup := seen[image]; dup {
			continue
		}
		seen[image] = struct{}{}
		images = append(images, image)
	}
	return images
}

// ComposeUpOptions carries the semantics of the `up` flags keploy injects.
//
// They are read from the resolved command string rather than re-derived, so
// this path and the shell-out it replaces can never disagree about them.
type ComposeUpOptions struct {
	// ExitCodeFrom is --exit-code-from: the service whose exit status becomes
	// the run's exit status. Empty when the flag is absent.
	ExitCodeFrom string
	// OnExit mirrors --abort-on-container-exit / --abort-on-container-failure.
	// The CLI's default when NEITHER flag is present is CascadeIgnore, not
	// CascadeStop: getting this wrong would tear the stack down the first time
	// any container exits, including a one-shot init or migration container.
	OnExit api.Cascade
}

// attachTo lists the services the CLI would stream logs from: every service
// that has not opted out with `attach: false` in the compose document.
// Dependencies are excluded, matching the CLI's IgnoreDependencies default.
func (r *ComposeRunner) attachTo() []string {
	names := r.project.ServiceNames()
	attach := make([]string, 0, len(names))
	for _, name := range names {
		svc, err := r.project.GetService(name)
		if err != nil {
			continue
		}
		if svc.Attach == nil || *svc.Attach {
			attach = append(attach, name)
		}
	}
	return attach
}

// Up starts the stack and BLOCKS until a container exits or ctx is cancelled.
//
// Blocking is the whole point, and it is not the library's default: with a nil
// Attach, Up returns as soon as the containers are created. The shell-out this
// replaces ran `docker compose up` in the foreground, and every caller above
// treats the call returning as "the application exited". A non-blocking Up
// would make the app look like it had exited immediately.
//
// Every other field is left at the value `docker compose up` itself passes
// with no extra flags (verified against cmd/compose/up.go): Recreate and
// RecreateDependencies are "diverged" — NOT force, which would needlessly
// recreate containers compose would otherwise reuse — Inherit is true,
// RemoveOrphans is false, and Timeout is nil.
func (r *ComposeRunner) Up(ctx context.Context, opts ComposeUpOptions, out, errW io.Writer) error {
	return r.svc.Up(ctx, r.project, api.UpOptions{
		Create: api.CreateOptions{
			Recreate:             api.RecreateDiverged,
			RecreateDependencies: api.RecreateDiverged,
			Inherit:              true,
		},
		Start: api.StartOptions{
			Project:      r.project,
			Attach:       composeLogConsumer{out: out, err: errW},
			AttachTo:     r.attachTo(),
			OnExit:       opts.OnExit,
			ExitCodeFrom: strings.TrimSpace(opts.ExitCodeFrom),
		},
	})
}

// Pull fetches every image the project references, resolving registry
// credentials through the same config.json the CLI would read.
func (r *ComposeRunner) Pull(ctx context.Context, quiet bool) error {
	return r.svc.Pull(ctx, r.project, api.PullOptions{Quiet: quiet})
}

// Down tears the project down. It is the direct analogue of the
// `docker compose down --timeout N` shell-out.
//
// Volumes are deliberately NOT removed: the shell-out it replaces ran a bare
// `docker compose down --timeout 1`, which leaves named volumes in place.
// Removing them here would silently discard a user's database between
// test-sets.
func (r *ComposeRunner) Down(ctx context.Context, timeout time.Duration) error {
	return r.svc.Down(ctx, r.project.Name, api.DownOptions{
		Project: r.project,
		Timeout: &timeout,
	})
}

// ServiceState is one row of `docker compose ps -a --format json`.
//
// ID and Labels are carried as well as the classifier's three fields: the
// teardown sweep needs an id to remove, and it must skip `compose run`
// one-offs, which are only distinguishable by the com.docker.compose.oneoff
// label — a one-off carries the same service label as a service container.
type ServiceState struct {
	Service  string
	State    string
	ExitCode int
	ID       string
	Labels   map[string]string
}

// Ps reports every container in the project, running or not.
//
// `-a` matters: the classifier has to see a service that already exited, which
// is exactly the row a running-only listing would drop.
func (r *ComposeRunner) Ps(ctx context.Context) ([]ServiceState, error) {
	containers, err := r.svc.Ps(ctx, r.project.Name, api.PsOptions{Project: r.project, All: true})
	if err != nil {
		return nil, err
	}
	states := make([]ServiceState, 0, len(containers))
	for _, c := range containers {
		states = append(states, ServiceState{
			Service:  c.Service,
			State:    c.State,
			ExitCode: c.ExitCode,
			ID:       c.ID,
			Labels:   c.Labels,
		})
	}
	return states, nil
}

// ContainerIDsForService returns the ids compose currently tracks for one
// service key. The agent's container_name is random per process, so it can only
// be found by service, never by name.
func (r *ComposeRunner) ContainerIDsForService(ctx context.Context, service string) ([]string, error) {
	containers, err := r.svc.Ps(ctx, r.project.Name, api.PsOptions{
		Project:  r.project,
		All:      true,
		Services: []string{service},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, c := range containers {
		if strings.TrimSpace(c.ID) != "" {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}
