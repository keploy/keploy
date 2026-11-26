package telemetry

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// InternalEnv set to 1 or true (any case, spaces ignored) marks a run as
// keploy's own: keploy's CI sets it in every pipeline, and a test machine or
// an e2e drive may set it too. Its events then carry meta.internal, and the
// daily analysis leaves them out of the users' numbers. Any other value is a
// user's run: a mark that is not clearly set never hides one. keploy's
// enterprise CLI reads the same variable the same way (its analytics
// Internal).
const InternalEnv = "KEPLOY_INTERNAL"

// InvokerEnv names what started this CLI, as the tool that started it says
// (the VS Code extension, a CI template): "ci" means a CI job, even where the
// job's container carries none of its runner's variables.
const InvokerEnv = "KEPLOY_INVOKER"

// Why an event is keploy's own, its meta.internal_why: the first of these
// that holds, in this order.
const (
	// InternalWhyEnv: InternalEnv marks the run.
	InternalWhyEnv = "env"
	// InternalWhyCIRepo: a CI run building one of keploy's repositories
	// (keploy/...), named by its CI provider's variables.
	InternalWhyCIRepo = "ci_repo"
	// InternalWhyDevBuild: a build of no release (IsDevBuild).
	InternalWhyDevBuild = "dev_build"
)

// commitVersion is a version that is a git commit and nothing else: CI
// builds stamped with the commit they built (-X main.version=<sha>).
var commitVersion = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// IsDevBuild reports whether version is that of a build of no release:
// utils.DevVersion (a build nobody gave a version: a developer's or a CI's
// source build), a version of 0.0.0 (CI's 0.0.0-pipeline-N), or a bare commit hash.
// A release's own suffixes (3.9.4-abc1234, 3.9.15-rc.1, 1.0.0-beta10) are
// releases, and an empty version says nothing: neither is a dev build. The
// daily analysis reads keploy_version by the same rule.
func IsDevBuild(version string) bool {
	v := strings.TrimSpace(version)
	switch {
	case v == utils.DevVersion:
		return true
	case strings.HasPrefix(strings.TrimPrefix(v, "v"), "0.0.0"):
		return true
	}
	return commitVersion.MatchString(v)
}

// internalByEnv reports whether InternalEnv marks this run as keploy's own.
func internalByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(InternalEnv))) {
	case "1", "true":
		return true
	}
	return false
}

// ownCIRepoVars are the variables in which keploy's own CI providers, GitHub
// Actions and Woodpecker, name the repository whose pipeline runs: the
// pipeline's own, never one a job only clones. Jenkins' GIT_URL, CircleCI's,
// Buildkite's and CodeBuild's URLs name the repository the job checks out,
// which in a prospect's job may be a keploy sample's.
var ownCIRepoVars = []string{"GITHUB_REPOSITORY", "CI_REPO"}

// keployCIRun reports whether this process is a CI run (detectCIOrInvoker) of
// one of keploy's repositories, as the first of ownCIRepoVars that names one
// says. The CI provider's variables only, never the checkout's remote, so a
// user who cloned a keploy sample app is still a user; and only in CI, since
// a Codespace of a keploy sample sets GITHUB_REPOSITORY with no runner's
// variable, and its user is a user.
func keployCIRun() bool {
	if ci, _ := detectCIOrInvoker(); !ci {
		return false
	}
	for _, name := range ownCIRepoVars {
		if s := normalizeRepo(os.Getenv(name), false); s != "" {
			return strings.HasPrefix(strings.ToLower(s), "keploy/")
		}
	}
	return false
}

// InternalVariables are the variables whose values make a run keploy's own
// for the agent it starts (PassesInternal): what a test of a user's run
// clears, also in keploy's own CI.
func InternalVariables() []string {
	return append([]string{InternalEnv}, ownCIRepoVars...)
}

// InternalWhy is why this process's events are keploy's own (an
// InternalWhy... constant), "" when they are a user's: InternalEnv, then a CI
// run of a keploy/ repository (keployCIRun), then a dev build of version
// (IsDevBuild).
func InternalWhy(version string) string {
	switch {
	case internalByEnv():
		return InternalWhyEnv
	case keployCIRun():
		return InternalWhyCIRepo
	case IsDevBuild(version):
		return InternalWhyDevBuild
	}
	return ""
}

// PassesInternal reports whether this process must hand its mark to a keploy
// it starts (its agent: AgentEnv, AgentArgs): the mark the agent cannot read
// for itself. A container or a sudo starts it without this environment, so
// neither InternalEnv nor the CI variables reach it; a dev build's agent is
// the same build, which marks itself.
func PassesInternal() bool {
	return internalByEnv() || keployCIRun()
}

// AgentFlag is the agent's hidden flag that marks its run as keploy's own,
// for a native agent: sudo drops InternalEnv on the way to it. The agent sets
// InternalEnv from it (MarkInternal), so its events, and those of a keploy it
// starts in turn, carry the mark.
const AgentFlag = "keploy-internal"

// AgentArgs is what a native agent's launcher appends to its argv: AgentFlag
// when this run is keploy's own (PassesInternal), nothing otherwise, so a
// user's agent's argv is what it was before the flag existed. Only a native
// launcher passes it, since the agent it starts is this very binary; a docker
// agent gets AgentEnv instead, which an agent image of another version
// ignores where it would refuse an unknown flag.
func AgentArgs() []string {
	if !PassesInternal() {
		return nil
	}
	return []string{"--" + AgentFlag}
}

// AgentEnv is the environment entry a launcher puts on an agent it starts in
// a container: InternalEnv=1 when this run is keploy's own (PassesInternal),
// "" otherwise.
func AgentEnv() string {
	if !PassesInternal() {
		return ""
	}
	return InternalEnv + "=1"
}

// MarkInternal makes this process's run keploy's own, as InternalEnv says,
// for the agent given AgentFlag. Its own events and those of any keploy it
// starts then carry the mark.
func MarkInternal() {
	_ = os.Setenv(InternalEnv, "1")
}

// invokedByCI reports whether InvokerEnv says a CI job started this CLI.
func invokedByCI() bool {
	return strings.TrimSpace(os.Getenv(InvokerEnv)) == "ci"
}

// defaultTeleHost is defaultTeleURL's host, which a test binary never sends
// to (sendsTo).
const defaultTeleHost = "telemetry.keploy.io"

// sendsTo reports whether an event may go to rawURL. A test binary sends only
// to a sink a test named (KEPLOY_TELEMETRY_URL, or teleURL set by the test):
// never to keploy's own host, however the URL spells it, where a test's run
// would count as a user's. A URL that does not parse is refused too.
func sendsTo(rawURL string) bool {
	if !testing.Testing() {
		return true
	}
	u, err := url.Parse(rawURL)
	return err == nil && !strings.EqualFold(u.Hostname(), defaultTeleHost)
}
