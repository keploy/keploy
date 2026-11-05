package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ComposeStack runs one compose project in process, through a compose library
// rather than the `docker compose` CLI plugin.
//
// keploy itself links no compose library: with no builder registered
// (RegisterComposeStack), a compose document keploy holds in memory runs
// through `docker compose -f -`. A build that links a library registers a
// builder, and keploy then drives that document through it instead, which
// needs no compose plugin on the host.
type ComposeStack interface {
	// ProjectName is the compose project the stack runs as.
	ProjectName() string
	// Up starts the stack and blocks until it ends or ctx is cancelled, as a
	// foreground `docker compose up` does. out and errW receive each attached
	// container's output, one line at a time, each line prefixed with the
	// container's name as the CLI prints it. A non-zero exit of the
	// ExitCodeFrom service is a *ComposeExitError.
	Up(ctx context.Context, opts ComposeUpOptions, out, errW io.Writer) error
	// Down stops and removes the project's containers and networks, giving
	// each container timeout to stop, and keeps its named volumes, as a bare
	// `docker compose down --timeout N` does.
	Down(ctx context.Context, timeout time.Duration) error
	// Ps reports every container in the project, running or not, as
	// `docker compose ps -a` does.
	Ps(ctx context.Context) ([]ServiceState, error)
	// ContainerIDsForService returns the ids of the project's containers,
	// running or not, of one service.
	ContainerIDsForService(ctx context.Context, service string) ([]string, error)
}

// ComposeStackSpec is the compose document a ComposeStack runs, and the scope
// the equivalent `docker compose -f -` invocation would resolve it in.
type ComposeStackSpec struct {
	// Content is the compose document.
	Content []byte
	// ProjectName is the project name the command gives (-p or
	// --project-name), or "" when it gives none.
	ProjectName string
	// WorkingDir is the project directory the command gives
	// (--project-directory), or "" for the current directory.
	WorkingDir string
	// EnvFiles are the --env-file paths the command gives, in order.
	EnvFiles []string
}

// ComposeOnExit is what `up` does when an attached container exits.
type ComposeOnExit int

const (
	// ComposeOnExitIgnore keeps the stack running: the CLI's behaviour when
	// the command gives neither abort flag. It is the zero value, so options
	// built without it never stop the stack at a one-shot container's exit.
	ComposeOnExitIgnore ComposeOnExit = iota
	// ComposeOnExitStop stops the stack when any container exits
	// (--abort-on-container-exit, or --exit-code-from alone).
	ComposeOnExitStop
	// ComposeOnExitFail stops the stack when a container exits non-zero
	// (--abort-on-container-failure).
	ComposeOnExitFail
)

// String names the behaviour for logs.
func (c ComposeOnExit) String() string {
	switch c {
	case ComposeOnExitIgnore:
		return "ignore"
	case ComposeOnExitStop:
		return "stop"
	case ComposeOnExitFail:
		return "fail"
	}
	return "unknown"
}

// ComposeUpOptions carries the semantics of the `up` flags keploy injects.
//
// They are read from the resolved command string rather than re-derived, so
// the in-process path and the shell-out can never disagree about them.
type ComposeUpOptions struct {
	// ExitCodeFrom is --exit-code-from: the service whose exit status becomes
	// the run's exit status. Empty when the flag is absent.
	ExitCodeFrom string
	// OnExit mirrors --abort-on-container-exit / --abort-on-container-failure.
	OnExit ComposeOnExit
}

// ServiceState is one row of `docker compose ps -a --format json`.
//
// ID and Labels are carried as well as the classifier's three fields: the
// teardown sweep needs an id to remove, and it must skip `compose run`
// one-offs, which are only distinguishable by the com.docker.compose.oneoff
// label: a one-off carries the same service label as a service container.
type ServiceState struct {
	Service  string
	State    string
	ExitCode int
	ID       string
	Labels   map[string]string
}

// ComposeExitError is the error ComposeStack.Up returns when the stack ran and
// its exit status, the one the CLI would have exited with, is not zero.
type ComposeExitError struct {
	Code int
	Err  error
}

func (e *ComposeExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("compose up exited with status %d", e.Code)
}

func (e *ComposeExitError) Unwrap() error { return e.Err }

// ComposeStackBuilder builds the in-process runner for one compose document.
// An error means the stack could never be started (the document does not load,
// the engine cannot be reached).
type ComposeStackBuilder func(ctx context.Context, spec ComposeStackSpec) (ComposeStack, error)

var (
	composeStackMu      sync.RWMutex
	composeStackBuilder ComposeStackBuilder
)

// RegisterComposeStack makes keploy run the compose documents it holds in
// memory through b. A build registers at most one builder, at init; nil
// unregisters it.
func RegisterComposeStack(b ComposeStackBuilder) {
	composeStackMu.Lock()
	defer composeStackMu.Unlock()
	composeStackBuilder = b
}

// ComposeStackRegistered reports whether a builder is registered, so keploy
// runs its in-memory compose documents in process.
func ComposeStackRegistered() bool {
	composeStackMu.RLock()
	defer composeStackMu.RUnlock()
	return composeStackBuilder != nil
}

// errNoComposeStack is NewComposeStack's answer when no builder is registered.
var errNoComposeStack = errors.New("this keploy build runs compose through the docker compose plugin: no in-process compose runner is registered")

// NewComposeStack builds the in-process runner for spec with the registered
// builder.
func NewComposeStack(ctx context.Context, spec ComposeStackSpec) (ComposeStack, error) {
	composeStackMu.RLock()
	b := composeStackBuilder
	composeStackMu.RUnlock()
	if b == nil {
		return nil, errNoComposeStack
	}
	return b(ctx, spec)
}

// StartAppOutputRun starts a new run of project's app-output tails, for one
// in-process Up, and returns its id: it drops the tails the project holds, so
// what they hold from here on is that Up's output alone. Record the Up's lines
// with RecordAppOutputOfRun.
func StartAppOutputRun(project string) uint64 {
	return appOutput.startRun(project)
}

// RecordAppOutputOfRun cuts one line that project's container compose names
// name printed (without its line end) of the values the log redactor holds,
// adds the cut line to the container's tail when run is the project's current
// run (StartAppOutputRun), and returns the cut line, for the caller to print.
// service is the container's compose service, or "" when not known.
func RecordAppOutputOfRun(project string, run uint64, name, service, line string) string {
	return appOutput.record(project, run, name, service, line, 0)
}
