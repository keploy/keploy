package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// KEPLOY DOES WHAT THE USER'S COMPOSE ABORT FLAGS SAY, ON ITS OWN WATCH.
//
// compose runs without --abort-on-container-exit, --abort-on-container-failure
// and --exit-code-from (withoutComposeAbortFlags): compose would act on them at
// keploy's own stop of the app, and take keploy's agent down with the stack.
// Dropped and not acted on, they no longer ended the run: `keploy record -c
// "docker compose up --exit-code-from tests"`, a test driver's container that
// exits when it is through, ran on until it was stopped. So keploy reads them
// from the user's command (composeUserAbort), says so, and its watch of the
// app's container does what compose would have done (composeAbortDied),
// keploy's own stops excepted (its stop of the stack, and an exit while it
// holds the app stopped at a set's end, which its stop gave: a dependent that
// fails once the app is down is no failure of the app): an exit of a
// container compose attaches to, in the app's project, ends the run with --abort-on-container-exit or
// --exit-code-from, and a failure (a code other than 0) with
// --abort-on-container-failure. keploy stops the stack then, as compose's
// abort stopped it. The run's exit code is --exit-code-from's service's first
// exit's, whatever the other flags say, as compose's up captures it: read once
// the stack is stopped (composeHaltCodeFrom) when another container's exit
// ended the run; with no such service, it is that of the container whose exit
// ended the run.
//
// That holds before the app has started too, as compose's own up ended on it
// (`up --exit-code-from tests`, a test driver that fails at once): left to
// compose's up, the run never ended, since that up, the flags left out, waits
// for the app. The one exception is an exit compose's up waits on itself
// (composeUpWaitsOn): a service another service of the up depends on with
// condition service_completed_successfully or service_healthy, that other
// service not yet started. A 0 then is the dependency's completion (a one-shot
// migration the app waits on), and a failure fails the up, which keploy's
// retry of the up reads (isTransientComposeDependencyFailure); keploy says so.
// The stack's stop takes keploy's agent down, and the app runs in the agent's
// network and pid namespaces, so no app can start after it.
//
// The app's own exit ends the run with the app's code, as without the flags,
// and keploy's agent's exit names the agent (watchComposeApp).

// composeAgentServiceKey is the service key keploy gives its agent in the
// compose document it runs (docker.ModifyComposeForAgent): its exits are
// keploy's agent's, never the user's abort's.
const composeAgentServiceKey = "keploy-agent"

// composeAbortRule is what the user's `compose up` flags say compose does on
// a container's exit.
type composeAbortRule struct {
	// onExit: an exit of an attached container ends the run
	// (--abort-on-container-exit, or --exit-code-from, which implies it).
	onExit bool
	// onFailure: a failure of an attached container ends the run
	// (--abort-on-container-failure, which --exit-code-from does not
	// override).
	onFailure bool
	// exitCodeFrom is --exit-code-from's service, whose exit code is the
	// run's; it is attached whatever else the flags say.
	exitCodeFrom string
	// attached are the services compose attaches to; nil: every service.
	attached map[string]bool
	// detached are those it never attaches to (--no-attach, `attach:
	// false` with no --attach of it).
	detached map[string]bool
	// flags are the abort flags as the command gives them.
	flags []string
	// waitedOnBy are, by service, the services that depend on it with a
	// condition compose's up waits on (composeWaitingDependents).
	waitedOnBy map[string][]string
}

// active reports whether the rule ends a run on any exit.
func (r composeAbortRule) active() bool { return r.onExit || r.onFailure }

// attaches reports whether compose attaches to service's containers.
func (r composeAbortRule) attaches(service string) bool {
	if service == r.exitCodeFrom {
		return true
	}
	if r.detached[service] {
		return false
	}
	return r.attached == nil || r.attached[service]
}

// endsOn reports whether an exit of an attached container with code ends the
// run. A code docker does not give (-1) is no clean exit.
func (r composeAbortRule) endsOn(code int) bool {
	if r.onFailure {
		return code != 0
	}
	return r.onExit
}

// composeUpValueFlags are compose up's flags whose value is the next word
// when it is not given with "=".
var composeUpValueFlags = map[string]bool{
	"--attach": true, "--no-attach": true, "--exit-code-from": true, "--pull": true,
	"--scale": true, "-t": true, "--timeout": true, "--wait-timeout": true,
}

// composeShellWords are the words that end a command in a shell line: what
// follows them is not up's.
var composeShellWords = map[string]bool{"&&": true, "||": true, ";": true, "|": true, "&": true}

// unquoteWord is a command word without the quotes around it, as the shell
// passes it on: `"app"` and `'app'` are app.
func unquoteWord(w string) string {
	return strings.Trim(w, `"'`)
}

// composeUserAbort is the abort rule the `up` of cmd, the user's command as
// written, gives, its services read from compose (attach: false, depends_on
// for --attach-dependencies).
func composeUserAbort(cmd string, compose *docker.Compose) composeAbortRule {
	words := strings.Fields(cmd)
	up := composeUpField(words)
	if up < 0 {
		return composeAbortRule{}
	}
	var r composeAbortRule
	var services []string
	attach := map[string]bool{}
	noAttach := map[string]bool{}
	attachDeps := false
	for i := up + 1; i < len(words); i++ {
		w := unquoteWord(words[i])
		if composeShellWords[w] {
			break
		}
		if !strings.HasPrefix(w, "-") {
			services = append(services, w)
			continue
		}
		name, value, given := strings.Cut(w, "=")
		// --exit-code-from="runner": the shell's quotes are the value's.
		value = unquoteWord(value)
		if !given && composeUpValueFlags[name] && i+1 < len(words) {
			i++
			value = unquoteWord(words[i])
		}
		switch name {
		case "--abort-on-container-exit":
			r.onExit = true
			r.flags = append(r.flags, name)
		case "--abort-on-container-failure":
			r.onFailure = true
			r.flags = append(r.flags, name)
		case "--exit-code-from":
			r.exitCodeFrom = value
			r.flags = append(r.flags, name+" "+value)
		case "--attach":
			attach[value] = true
		case "--no-attach":
			noAttach[value] = true
		case "--attach-dependencies":
			attachDeps = true
		}
	}
	if r.exitCodeFrom != "" && !r.onFailure {
		r.onExit = true
	}
	switch {
	case len(attach) > 0:
		r.attached = attach
	case len(services) > 0:
		r.attached = map[string]bool{}
		for _, s := range services {
			r.attached[s] = true
		}
		if attachDeps {
			for _, s := range composeDependencies(compose, services) {
				r.attached[s] = true
			}
		}
	}
	r.detached = noAttach
	for _, s := range composeAttachOff(compose) {
		if !attach[s] {
			r.detached[s] = true
		}
	}
	r.waitedOnBy = composeWaitingDependents(compose)
	return r
}

// composeWaitConditions are the depends_on conditions on which compose's up
// waits for the dependency before it starts the service that names it:
// its completion, or its health. service_started (the list form's) waits on
// nothing past the dependency's start.
var composeWaitConditions = map[string]bool{"service_completed_successfully": true, "service_healthy": true}

// composeWaitingDependents are, by service, the services of compose that
// depend on it with a condition compose's up waits on (composeWaitConditions).
//
// Every read here is compose's (docker.MappingEntries): through aliases and
// `<<` merge keys, so a depends_on a service inherits from an anchor, or
// whose entries or conditions are anchored, is the one compose's up waits on.
func composeWaitingDependents(compose *docker.Compose) map[string][]string {
	by := map[string][]string{}
	for _, svc := range composeServiceEntries(compose) {
		for _, dep := range docker.MappingEntries(composeServiceField(svc.Value, "depends_on")) {
			if cond := composeServiceField(dep.Value, "condition"); cond != nil && composeWaitConditions[cond.Value] {
				by[dep.Key.Value] = append(by[dep.Key.Value], svc.Key.Value)
			}
		}
	}
	return by
}

// composeServiceEntries are compose's services, read as compose reads them
// (docker.MappingEntries); nil for no compose.
func composeServiceEntries(compose *docker.Compose) []docker.YAMLEntry {
	if compose == nil {
		return nil
	}
	return docker.MappingEntries(&compose.Services)
}

// composeServiceNode is the mapping of service name in compose; nil when it
// has none.
func composeServiceNode(compose *docker.Compose, name string) *yaml.Node {
	if compose == nil {
		return nil
	}
	return composeServiceField(&compose.Services, name)
}

// composeServiceField is the value of key in a mapping (a service's, or one
// of its fields), read as compose reads it (docker.MappingEntries); nil when
// it has none.
func composeServiceField(m *yaml.Node, key string) *yaml.Node {
	for _, e := range docker.MappingEntries(m) {
		if e.Key.Value == key {
			return e.Value
		}
	}
	return nil
}

// composeAttachOff are compose's services set `attach: false`.
func composeAttachOff(compose *docker.Compose) []string {
	var off []string
	for _, svc := range composeServiceEntries(compose) {
		if v := composeServiceField(svc.Value, "attach"); v != nil && v.Value == "false" {
			off = append(off, svc.Key.Value)
		}
	}
	return off
}

// composeDependencies are the services those depend on, through depends_on
// (a list of names or a mapping keyed by them), to any depth.
func composeDependencies(compose *docker.Compose, services []string) []string {
	seen := map[string]bool{}
	for _, s := range services {
		seen[s] = true
	}
	var deps []string
	queue := append([]string{}, services...)
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		on := composeServiceField(composeServiceNode(compose, s), "depends_on")
		if on == nil {
			continue
		}
		var names []string
		switch on.Kind {
		case yaml.SequenceNode:
			for _, n := range on.Content {
				names = append(names, docker.AliasTarget(n).Value)
			}
		case yaml.MappingNode:
			for _, e := range docker.MappingEntries(on) {
				names = append(names, e.Key.Value)
			}
		}
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				deps = append(deps, n)
				queue = append(queue, n)
			}
		}
	}
	return deps
}

// setComposeUserAbort keeps the abort rule of cmd, the user's command before
// keploy leaves its abort flags out, and says what keploy does with them.
func (a *App) setComposeUserAbort(cmd string, compose *docker.Compose) {
	a.composeAbort = composeUserAbort(cmd, compose)
	if !a.composeAbort.active() {
		return
	}
	a.logger.Info("keploy runs docker compose up without the command's abort flags, which would take keploy's agent down at keploy's own stop of the application; keploy does what they say itself: an exit of a container compose attaches to ends the run, keploy's own stops and an exit compose's up waits on before it starts a service excepted",
		zap.Strings("flags", a.composeAbort.flags))
}

// composeAbortDied reports whether m, a die (the watch's kills are
// composeDied's) of a container of the app's
// project that is neither the app's nor keploy's agent's, ends the run under
// the user's abort flags (composeUserAbort), keeping its halt if so: keploy
// has not begun to stop the stack, nor holds the app stopped (stopping, from
// its stop at a set's end until it starts the app again), the container is
// down, and its exit is one the flags end the run on (keepComposeAbortHalt).
func (a *App) composeAbortDied(ctx context.Context, m events.Message) bool {
	r := a.composeAbort
	attrs := m.Actor.Attributes
	service := attrs[composeServiceLabelKey]
	if !r.active() || a.composeService == "" || service == "" ||
		service == a.composeService || service == composeAgentServiceKey || isComposeOneOffLabel(attrs) || !r.attaches(service) {
		return false
	}
	if project := a.composeAppProject(ctx); project == "" || attrs[composeProjectLabelKey] != project {
		return false
	}
	a.composeApp.stopMu.Lock()
	defer a.composeApp.stopMu.Unlock()
	info, read := a.readComposeContainer(ctx, m.Actor.ID, "keploy decides the exit on the event")
	if ctx.Err() != nil || a.composeApp.agentStopping.Load() || (read && info.State.Running) {
		return false
	}
	if read && composeDieOfAnEarlierRun(m, info.State) {
		return false
	}
	code := -1
	if read {
		code = info.State.ExitCode
	} else if c, err := strconv.Atoi(attrs["exitCode"]); err == nil {
		code = c
	}
	// Read after the inspect, under stopMu, as composeAppDied reads it: an
	// exit while keploy holds the app stopped is one keploy's stop gave (a
	// service that fails once the app is down), never the app's failure, so
	// it keeps no halt and ends nothing, whatever the flags say.
	if a.composeApp.stopping.Load() {
		if a.composeAbort.endsOn(code) {
			a.logger.Info("a container compose attaches to exited while keploy held the application stopped at a set's end: keploy takes the exit for one its own stop gave, and it ends nothing",
				zap.String("service", service), zap.Int("exitCode", code), zap.Strings("flags", a.composeAbort.flags))
		}
		return false
	}
	return a.keepComposeAbortHalt(ctx, service, code)
}

// keepComposeAbortHalt keeps the halt of service's exit with code when the
// user's flags end the run on it, unless the app has not started in this run
// and compose's up waits on that exit (composeUpWaitsOn); no halt kept yet
// (the app's own, the agent's) stands.
func (a *App) keepComposeAbortHalt(ctx context.Context, service string, code int) bool {
	if !a.composeAbort.endsOn(code) {
		return false
	}
	beforeApp := !a.composeAppStarted(ctx)
	if beforeApp {
		if waiting := a.composeUpWaitsOn(ctx, service); waiting != "" {
			a.logger.Info("a container compose attaches to exited before the application started, and docker compose up waits on that exit before it starts another service (a depends_on condition): it ends nothing; a 0 is the dependency's completion, and a failure fails the up, which keploy's retry of the up reads",
				zap.String("service", service), zap.Int("exitCode", code), zap.String("waitingService", waiting))
			return false
		}
	}
	h := &composeHalt{exitCode: code, service: service, serviceCode: code, flags: strings.Join(a.composeAbort.flags, " "), beforeApp: beforeApp}
	if !a.composeApp.halted.CompareAndSwap(nil, h) {
		return false
	}
	a.logger.Info("a container compose attaches to exited, and the command's abort flags end the run on it: ending the run",
		zap.String("service", service), zap.Int("exitCode", code), zap.Strings("flags", a.composeAbort.flags))
	return true
}

// composeUpWaitsOn is a service compose's up waits on service's exit for
// before it starts it, "" when there is none: one that depends on service with
// a condition compose waits on (composeWaitingDependents) whose container in
// the app's project, one-offs aside, has not started in this run. compose's up
// makes the containers of all its services before it starts any, so a
// service with no container is not the up's, and waits on nothing. A daemon
// that gives no answer (ctx's end) leaves the exit to compose's up.
func (a *App) composeUpWaitsOn(ctx context.Context, service string) string {
	project := a.composeAppProject(ctx)
	for _, dependent := range a.composeAbort.waitedOnBy[service] {
		list, err := askDaemon(ctx, a.logger, fmt.Sprintf("the containers of compose service %q (with no answer, docker compose up decides the exit it may wait on)", dependent),
			func(ctx context.Context) ([]container.Summary, error) {
				return a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
					filters.Arg("label", composeProjectLabelKey+"="+project),
					filters.Arg("label", composeServiceLabelKey+"="+dependent),
				)})
			})
		if err != nil {
			return dependent
		}
		for _, c := range list {
			if isComposeOneOffLabel(c.Labels) {
				continue
			}
			info, read := a.readComposeContainer(ctx, c.ID, "keploy takes it for one that has not started")
			if !read || daemonNanos(info.State.StartedAt) == 0 {
				return dependent
			}
		}
	}
	return ""
}

// composeAbortExited is the halt the user's flags give an `up` that returned
// with the app still up: compose's up, its abort flags left out, returns once
// every container it attaches to is down, which an `up` of the user's
// services alone (`up tests`, the app their dependency) is as soon as they
// exit, before the watch may have taken their exit. The exit read is the one
// that came first.
func (a *App) composeAbortExited(ctx context.Context) bool {
	if !a.composeAbort.active() || a.composeService == "" {
		return false
	}
	project := a.composeAppProject(ctx)
	if project == "" {
		return false
	}
	list, err := askDaemon(ctx, a.logger, fmt.Sprintf("the containers of compose project %q (with no answer, keploy takes the up's end for a clean one)", project),
		func(ctx context.Context) ([]container.Summary, error) {
			return a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", composeProjectLabelKey+"="+project))})
		})
	if err != nil {
		return false
	}
	first, firstAt, firstCode := "", time.Time{}, 0
	for _, c := range list {
		service := c.Labels[composeServiceLabelKey]
		if service == "" || service == a.composeService || service == composeAgentServiceKey || isComposeOneOffLabel(c.Labels) || !a.composeAbort.attaches(service) {
			continue
		}
		info, read := a.readComposeContainer(ctx, c.ID, "keploy takes it for one still running")
		if !read || (info.State.Status != "exited" && info.State.Status != "dead") || !a.composeAbort.endsOn(info.State.ExitCode) {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, info.State.FinishedAt)
		if first == "" || at.Before(firstAt) {
			first, firstAt, firstCode = service, at, info.State.ExitCode
		}
	}
	return first != "" && a.keepComposeAbortHalt(ctx, first, firstCode)
}

// composeHaltCodeFrom makes the run's exit code, on a halt the user's flags
// gave, that of --exit-code-from's service, as compose's run ended with it,
// once keploy has stopped the stack: when another container's exit ended
// the run, that service's code is its first exit's, the one its stop gave it
// when it ran until then. compose's up captures it whatever the on-exit policy
// (--abort-on-container-failure too): its captureExitCodeFrom listens apart
// from the abort's. The app's own halt keeps the app's code.
func (a *App) composeHaltCodeFrom(ctx context.Context) {
	h := a.composeApp.halted.Load()
	from := a.composeAbort.exitCodeFrom
	if h == nil || h.service == "" || from == "" || from == h.service {
		return
	}
	code, ok := a.composeServiceExitCode(ctx, from)
	if !ok {
		a.logger.Debug("keploy could not read the exit code of the --exit-code-from service; the run ends with that of the container whose exit ended it",
			zap.String("service", from))
		return
	}
	next := *h
	next.exitCode, next.codeFrom = code, from
	a.composeApp.halted.Store(&next)
}

// composeServiceExitCode is the exit code of service's container in the app's
// project, when it ran and is down: one compose only made (`created`, never
// started) has no exit, nor does a one-off (`compose run`) of service.
func (a *App) composeServiceExitCode(ctx context.Context, service string) (int, bool) {
	project := a.composeAppProject(ctx)
	if project == "" {
		return 0, false
	}
	list, err := askDaemon(ctx, a.logger, fmt.Sprintf("the containers of compose service %q (with no answer, the run ends with the code of the container whose exit ended it)", service),
		func(ctx context.Context) ([]container.Summary, error) {
			return a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
				filters.Arg("label", composeProjectLabelKey+"="+project),
				filters.Arg("label", composeServiceLabelKey+"="+service),
			)})
		})
	if err != nil {
		return 0, false
	}
	for _, c := range list {
		if isComposeOneOffLabel(c.Labels) {
			continue
		}
		info, read := a.readComposeContainer(ctx, c.ID, "the run ends with the code of the container whose exit ended it")
		if read && (info.State.Status == "exited" || info.State.Status == "dead") {
			return info.State.ExitCode, true
		}
	}
	return 0, false
}
