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
// for the app. Whether an exit came before or after the app started is the
// daemon's word: the exit's FinishedAt against the app's first StartedAt in
// this run (composeExitBeforeApp), never what runs when keploy reads.
//
// Two kinds of exit differ from compose's own up, and from keploy before:
//   - An exit compose's up waits on (composeUpWaitsOn): a service another
//     service of the up depends on, with a condition, that other service not
//     yet started at the exit. A 0 of a service_completed_successfully
//     dependency (a one-shot migration) is its completion, and ends nothing,
//     before or after the app started; compose's own up, these flags given,
//     ended the run on it, and so did keploy, so a stack that ran a migration
//     never recorded. Before the app started, any other exit of a required
//     dependency fails compose's up as it starts the dependent (a
//     service_healthy one's 0 too), which keploy's retry of the up reads
//     when it crashed and the dependent is the app, and which ends the run
//     with the up's error otherwise. After the app started, a failure ends
//     the run as the flags say.
//   - A crash, before the app started, of a service the app depends on (any
//     depends_on form, to any depth): keploy stops the stack, as compose's
//     abort stopped it, keeps no halt, and the run retries the bring-up
//     (depCrash), as it did compose's abort, a bounded number of times
//     (composeDepFailureRetries), then ends on the crash.
//
// The stack's stop takes keploy's agent down, and the app runs in the agent's
// network and pid namespaces, so no app can start after it; what compose's up
// starts after the stop's listing is stopped as it starts
// (stopComposeLateStarters), so the up returns.
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
	waitedOnBy map[string][]composeWaiter
	// dependsOn are, by service, the services its depends_on names, in any
	// form (composeDependsOnNames).
	dependsOn map[string][]string
}

// composeWaiter is a service that depends on another with a condition
// compose's up waits on before it starts it.
type composeWaiter struct {
	service, condition string
	// required: compose's up fails when the dependency fails (depends_on's
	// `required`, true unless set false); an optional one's failure is
	// skipped, and the up starts the service all the same.
	required bool
}

// waits reports whether compose's up waits on an exit with code of the
// dependency before it starts w: a 0 of a service_completed_successfully
// dependency is its completion, which the up waits for; any other exit fails
// the up as it starts w, unless the dependency is optional, which the up
// skips.
func (w composeWaiter) waits(code int) bool {
	return w.required || (code == 0 && w.condition == "service_completed_successfully")
}

// appDependsOn reports whether app depends on service, through depends_on
// in any form, to any depth.
func (r composeAbortRule) appDependsOn(app, service string) bool {
	seen := map[string]bool{app: true}
	queue := []string{app}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, dep := range r.dependsOn[s] {
			if dep == service {
				return true
			}
			if !seen[dep] {
				seen[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return false
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
	r.dependsOn = map[string][]string{}
	for _, svc := range composeServiceEntries(compose) {
		r.dependsOn[svc.Key.Value] = composeDependsOnNames(composeServiceField(svc.Value, "depends_on"))
	}
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
func composeWaitingDependents(compose *docker.Compose) map[string][]composeWaiter {
	by := map[string][]composeWaiter{}
	for _, svc := range composeServiceEntries(compose) {
		for _, dep := range docker.MappingEntries(composeServiceField(svc.Value, "depends_on")) {
			if cond := composeServiceField(dep.Value, "condition"); cond != nil && composeWaitConditions[cond.Value] {
				req := composeServiceField(dep.Value, "required")
				by[dep.Key.Value] = append(by[dep.Key.Value], composeWaiter{service: svc.Key.Value, condition: cond.Value,
					required: req == nil || req.Value != "false"})
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
		for _, n := range composeDependsOnNames(composeServiceField(composeServiceNode(compose, s), "depends_on")) {
			if !seen[n] {
				seen[n] = true
				deps = append(deps, n)
				queue = append(queue, n)
			}
		}
	}
	return deps
}

// composeDependsOnNames are the services a depends_on value names: a list of
// names, or a mapping keyed by them; nil for none.
func composeDependsOnNames(on *yaml.Node) []string {
	if on == nil {
		return nil
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
	return names
}

// setComposeUserAbort keeps the abort rule of cmd, the user's command before
// keploy leaves its abort flags out, and says what keploy does with them.
func (a *App) setComposeUserAbort(cmd string, compose *docker.Compose) {
	a.composeAbort = composeUserAbort(cmd, compose)
	if !a.composeAbort.active() {
		return
	}
	a.logger.Info("keploy runs docker compose up without the command's abort flags, which would take keploy's agent down at keploy's own stop of the application; keploy does what they say itself: an exit of a container compose attaches to ends the run, except keploy's own stops, a one-shot dependency's completion (a change from compose's own up, which ended the run on it), and, before the application starts, an exit compose's up waits on and a crash of a service the application depends on, after which keploy retries the bring-up",
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
	// stackStopped is closed once keploy's stop of the stack for a crash
	// this exit is has ended (stackStop); crash is that crash.
	var stackStopped chan struct{}
	var crash *composeHalt
	verdict := func() composeAbortVerdict {
		a.composeApp.stopMu.Lock()
		defer a.composeApp.stopMu.Unlock()
		info, read := a.readComposeContainer(ctx, m.Actor.ID, "keploy decides the exit on the event")
		if ctx.Err() != nil || a.composeApp.agentStopping.Load() || (read && info.State.Running) {
			return composeAbortNone
		}
		if read && composeDieOfAnEarlierRun(m, info.State) {
			return composeAbortNone
		}
		code := -1
		if read {
			code = info.State.ExitCode
		} else if c, err := strconv.Atoi(attrs["exitCode"]); err == nil {
			code = c
		}
		// When the exit was: the daemon's FinishedAt, or its die event's time.
		exitedAt := m.TimeNano
		if read {
			if at := daemonNanos(info.State.FinishedAt); at != 0 {
				exitedAt = at
			}
		}
		// Read after the inspect, under stopMu, as composeAppDied reads it:
		// an exit while keploy holds the app stopped is one keploy's stop
		// gave (a service that fails once the app is down), never the app's
		// failure, so it keeps no halt and ends nothing, whatever the flags
		// say.
		if a.composeApp.stopping.Load() {
			if a.composeAbort.endsOn(code) {
				a.logger.Info("a container compose attaches to exited while keploy held the application stopped (at a set's end, or at the recording's or the run's end): keploy takes the exit for one its own stop gave, and it ends nothing",
					zap.String("service", service), zap.Int("exitCode", code), zap.Strings("flags", a.composeAbort.flags))
			}
			return composeAbortNone
		}
		// A run keploy's stop of the stack took down, read after the run's
		// retry of the bring-up began, is keploy's own stop too.
		if read && a.composeStoppedRun(info.ID, info.State) {
			a.logger.Debug("a container compose attaches to exited from keploy's stop of the stack for the retry of the bring-up: keploy's own stop, which ends nothing",
				zap.String("service", service), zap.Int("exitCode", code))
			return composeAbortNone
		}
		v := a.decideComposeAbortExit(ctx, service, code, exitedAt, true)
		if v == composeAbortRetry {
			// keploy's stop of the stack, as compose's abort stopped it:
			// none of its exits is a halt, and the retry begins anew.
			a.composeApp.stopping.Store(true)
			a.composeApp.agentStopping.Store(true)
			crash = &composeHalt{exitCode: code, service: service, serviceCode: code,
				flags: strings.Join(a.composeAbort.flags, " "), beforeApp: true}
			a.composeApp.depCrash.CompareAndSwap(nil, crash)
			stackStopped = make(chan struct{})
			a.composeApp.stackStop.Store(&stackStopped)
		}
		return v
	}()
	switch verdict {
	case composeAbortHalt:
		return true
	case composeAbortRetry:
		// Outside stopMu: the stop waits for the stack's containers to go,
		// and the run's retry of the bring-up waits for the stop, the stops
		// of its late starters among it: those end once the up has returned,
		// so the watch does not wait for them. A late starter still in its
		// stop's grace (a runner that does not handle SIGTERM) was reused by
		// the retried up, then killed mid-run.
		// The halt of a stop that failed is kept before the stop is said
		// ended: the run's retry, which waits for that, then reads it, and
		// begins no bring-up.
		stopped, lateStopped := a.stopComposeStack(context.WithoutCancel(ctx))
		ends := false
		if !stopped {
			ends = a.composeKeepsAnUnstoppedDepCrash(crash)
		}
		go func() {
			<-lateStopped
			close(stackStopped)
		}()
		return ends
	}
	return false
}

// A CRASH KEPLOY COULD NOT STOP THE STACK ON ENDS THE RUN.
//
// keploy holds every exit for one its own stop gave (stopping,
// agentStopping) from the moment it begins to stop the stack for a crash
// before the app started. When that stop could not list the project, the
// stack ran on with keploy still holding it: compose's up started the app,
// and under `--exit-code-from runner` the runner's exit was taken for
// keploy's own and ended nothing, so the run went on until the user stopped
// it. The crash is now the run's halt, as the halt of an exit the flags end
// the run on is, and the run's end takes the stack down (composeHaltEnd: a
// stop, else compose down).

// composeKeepsAnUnstoppedDepCrash keeps crash, a crash before the app started
// of a service the app depends on, whose stop of the stack keploy could not
// make, as the run's halt (stackNotStopped), and reports that the run ends on
// it: on a halt kept already, too, which ends it.
func (a *App) composeKeepsAnUnstoppedDepCrash(crash *composeHalt) bool {
	h := *crash
	h.stackNotStopped = true
	a.composeApp.halted.CompareAndSwap(nil, &h)
	a.logger.Warn("keploy could not stop the application's compose stack to retry its bring-up after a dependency's crash; it ends the run on that crash and takes the stack down",
		zap.String("service", crash.service))
	return true
}

// composeAbortVerdict is what an exit the user's flags end the run on does.
type composeAbortVerdict int

const (
	// composeAbortNone: the exit ends nothing.
	composeAbortNone composeAbortVerdict = iota
	// composeAbortHalt: the exit's halt is kept, and ends the run.
	composeAbortHalt
	// composeAbortRetry: a crash, before the app started, of a service the
	// app depends on: keploy stops the stack, as compose's abort stopped it,
	// and the run retries the bring-up (depCrash).
	composeAbortRetry
)

// decideComposeAbortExit is what service's exit with code, at exitedAt on
// the daemon's clock (0: not said), does under the user's flags, keeping
// its halt when it ends the run (no halt kept yet, the app's own or the
// agent's, stands): nothing when the flags do not end the run on it, nor
// when compose's up waits on it (composeUpWaitsOn) and it is a completion, or
// came before the app started; a retry of the bring-up when it is a crash,
// before the app started, of a service the app depends on, and the run can
// still retry it (canRetry); the halt otherwise. Before or after the app is
// the daemon's instants' (composeExitBeforeApp).
func (a *App) decideComposeAbortExit(ctx context.Context, service string, code int, exitedAt int64, canRetry bool) composeAbortVerdict {
	if !a.composeAbort.endsOn(code) {
		return composeAbortNone
	}
	beforeApp := a.composeExitBeforeApp(ctx, exitedAt)
	if code == 0 || beforeApp {
		if waiting := a.composeUpWaitsOn(ctx, service, code, exitedAt); waiting != "" {
			a.logger.Info("a container compose attaches to exited, and docker compose up waits on that exit before it starts another service (a depends_on condition): it ends nothing (a change from compose's own up, which these flags ended on it); a one-shot dependency's 0 is its completion, and any other exit of it before the application started fails the up as it starts that service, which keploy's retry of the up reads when the dependency crashed and that service is the application",
				zap.String("service", service), zap.Int("exitCode", code), zap.String("waitingService", waiting), zap.Bool("beforeApplication", beforeApp))
			return composeAbortNone
		}
	}
	if canRetry && beforeApp && code != 0 && a.composeAbort.appDependsOn(a.composeService, service) {
		a.logger.Info("a container of a service the application depends on crashed before the application started: keploy stops the stack, as compose's abort stopped it, and retries the bring-up",
			zap.String("service", service), zap.Int("exitCode", code), zap.Strings("flags", a.composeAbort.flags))
		return composeAbortRetry
	}
	h := &composeHalt{exitCode: code, service: service, serviceCode: code, flags: strings.Join(a.composeAbort.flags, " "), beforeApp: beforeApp}
	if !a.composeApp.halted.CompareAndSwap(nil, h) {
		return composeAbortNone
	}
	a.logger.Info("a container compose attaches to exited, and the command's abort flags end the run on it: ending the run",
		zap.String("service", service), zap.Int("exitCode", code), zap.Strings("flags", a.composeAbort.flags))
	return composeAbortHalt
}

// composeUpWaitsOn is a service compose's up waits on service's exit with
// code for before it starts it, "" when there is none: one that depends on
// service with a condition compose waits on, and waits on that code
// (composeWaiter.waits), whose container in the app's project, one-offs
// aside, had not started at the exit (exitedAt, on the daemon's clock; with
// none said, has not started yet): its StartedAt after the exit is a start
// compose's up gave it on that exit. compose's up makes the containers of
// all its services before it starts any, so a service with no container is
// not the up's, and waits on nothing. A daemon that gives no answer (ctx's
// end) leaves the exit to compose's up.
func (a *App) composeUpWaitsOn(ctx context.Context, service string, code int, exitedAt int64) string {
	project := a.composeAppProject(ctx)
	for _, w := range a.composeAbort.waitedOnBy[service] {
		if !w.waits(code) {
			continue
		}
		dependent := w.service
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
			if !read {
				return dependent
			}
			if started := daemonNanos(info.State.StartedAt); started == 0 || (exitedAt != 0 && started > exitedAt) {
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
	first, firstAt, firstCode, firstNanos := "", time.Time{}, 0, int64(0)
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
			first, firstAt, firstCode, firstNanos = service, at, info.State.ExitCode, daemonNanos(info.State.FinishedAt)
		}
	}
	if first == "" {
		return false
	}
	// The up has returned, and the run's retry of it is behind: a crash of
	// a service the app depends on ends the run here.
	return a.decideComposeAbortExit(ctx, first, firstCode, firstNanos, false) == composeAbortHalt
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
