//go:build composelib && !darwin

package docker

import (
	"fmt"
	"os"
	"strconv"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/cmd/display"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// ComposeLibrarySupported reports whether this build can drive compose
// in-process. Requires the `composelib` build tag; see
// compose_backend_unsupported.go for why it is opt-in and why darwin is out.
const ComposeLibrarySupported = true

// newComposeBackend returns the compose library's own implementation of the
// service interface, for one operation (see ComposeRunner.service).
// progressGiven says the caller set the CLI's stderr to its own progress
// writer (ComposeRunnerOptions.Progress), which composeProgress then keeps.
//
// It lives in its own file for ONE reason: this is the only place that imports
// github.com/docker/compose/v5/pkg/compose and cmd/display, the bulk of what
// the composelib tag opts into (see compose_backend_unsupported.go). Keeping
// the imports here means composelib.go itself — and everything that only needs
// pkg/api — compiles in every build, tagged or not.
func newComposeBackend(dockerCli command.Cli, progressGiven bool) (api.Compose, error) {
	svc, err := compose.NewComposeService(dockerCli, compose.WithEventProcessor(composeProgress(dockerCli, progressGiven)))
	if err != nil {
		return nil, fmt.Errorf("build the compose service: %w", err)
	}
	return svc, nil
}

// composeProgress reports what compose does to the project's containers,
// networks and images ("Container app-1  Started"), as `docker compose` does by
// default (--progress auto): redrawn in place when the stream is a terminal, a
// line per event otherwise. compose v2 chose this itself; compose v5 reports
// nothing at all without an event processor.
//
// The stream is the CLI's stderr: the caller's progress writer when it gave
// one, and otherwise os.Stderr, or os.Stdout under COMPOSE_STATUS_STDOUT,
// which v2's library read itself and v5's leaves to its CLI. COMPOSE_ANSI and
// NO_COLOR stay the CLI's alone, as they were under v2.
func composeProgress(dockerCli command.Cli, progressGiven bool) api.EventProcessor {
	out := dockerCli.Err()
	if toStdout, _ := strconv.ParseBool(os.Getenv("COMPOSE_STATUS_STDOUT")); toStdout && !progressGiven {
		out = dockerCli.Out()
	}
	if out.IsTerminal() {
		return display.Full(out, out, false)
	}
	return display.Plain(out)
}
