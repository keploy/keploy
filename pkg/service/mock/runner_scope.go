package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// scopeCallTimeout bounds one scope call so a stuck agent cannot stall the runner's stdout.
const scopeCallTimeout = 2 * time.Second

type scopeEventKind int

const (
	scopeNone scopeEventKind = iota
	scopeBegin
	scopeEnd
	scopePause
)

// scopeEvent is what one line of test-runner output says about a test.
type scopeEvent struct {
	kind scopeEventKind
	pkg  string
	test string
	// at is the runner's own clock for this boundary; zero when the output carries none.
	at time.Time
}

// name is the scope name: the test, qualified by its package when the runner reports one.
func (e scopeEvent) name() string {
	if e.pkg == "" {
		return e.test
	}
	return e.pkg + "." + e.test
}

// parent is the enclosing test, or just the package for a top-level test.
func (e scopeEvent) parent() string {
	if i := strings.LastIndexByte(e.test, '/'); i >= 0 {
		return e.pkg + "." + e.test[:i]
	}
	return e.pkg + "."
}

// inside reports whether e is a subtest of t.
func (e scopeEvent) inside(t scopeEvent) bool {
	return e.pkg == t.pkg && strings.HasPrefix(e.test, t.test+"/")
}

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// parseRunnerLine reads one line of `go test -json` or `go test -v` output.
func parseRunnerLine(line string) scopeEvent {
	line = cleanRunnerLine(line)
	if strings.HasPrefix(line, "{") {
		return parseJSONLine(line)
	}
	return parsePlainLine(line)
}

// cleanRunnerLine drops colour codes and a `docker compose up` service prefix ("app-1  | ").
func cleanRunnerLine(line string) string {
	line = strings.TrimSpace(ansiEscape.ReplaceAllString(line, ""))
	service, rest, ok := strings.Cut(line, "| ")
	if ok && service != "" && !strings.ContainsAny(strings.TrimSpace(service), " \t\"{") {
		return strings.TrimSpace(rest)
	}
	return line
}

func parseJSONLine(line string) scopeEvent {
	var ev struct{ Action, Package, Test, Time string }
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Test == "" {
		return scopeEvent{}
	}
	kind := kindOfAction(ev.Action)
	if kind == scopeNone {
		return scopeEvent{}
	}
	at, _ := time.Parse(time.RFC3339Nano, ev.Time)
	return scopeEvent{kind: kind, pkg: ev.Package, test: ev.Test, at: at}
}

func kindOfAction(action string) scopeEventKind {
	switch action {
	case "run":
		return scopeBegin
	case "pass", "fail", "skip":
		return scopeEnd
	case "pause":
		return scopePause
	}
	return scopeNone
}

func parsePlainLine(line string) scopeEvent {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return scopeEvent{}
	}
	switch {
	case fields[0] == "===" && fields[1] == "RUN":
		return scopeEvent{kind: scopeBegin, test: fields[2]}
	case fields[0] == "===" && fields[1] == "PAUSE":
		return scopeEvent{kind: scopePause, test: fields[2]}
	case fields[0] == "---" && (fields[1] == "PASS:" || fields[1] == "FAIL:" || fields[1] == "SKIP:"):
		return scopeEvent{kind: scopeEnd, test: fields[2]}
	}
	return scopeEvent{}
}

// runnerScope turns the wrapped test runner's output into scope begin/end calls on the agent.
type runnerScope struct {
	ctx    context.Context
	logger *zap.Logger
	marker ScopeMarker

	mu        sync.Mutex
	partial   []byte
	open      []scopeEvent
	paused    map[string]bool
	overlaps  []string
	readTimed int
}

func newRunnerScope(ctx context.Context, logger *zap.Logger, marker ScopeMarker) *runnerScope {
	return &runnerScope{ctx: ctx, logger: logger, marker: marker, paused: map[string]bool{}}
}

// writer is the stdout observer to hand the app runner; nil when the adapter is off.
func (r *runnerScope) writer() io.Writer {
	if r == nil {
		return nil
	}
	return r
}

// Write never fails: an error here would break the runner's own stdout.
func (r *runnerScope) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, p...)
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		r.handle(parseRunnerLine(string(r.partial[:i])))
		r.partial = r.partial[i+1:]
	}
}

func (r *runnerScope) handle(ev scopeEvent) {
	switch ev.kind {
	case scopeBegin:
		r.begin(ev)
		r.mark(ev, r.marker.BeginScope)
	case scopePause:
		r.paused[ev.name()] = true
	case scopeEnd:
		r.end(ev)
		r.mark(ev, r.marker.EndScope)
	}
}

// begin lists ev as running; a sibling still listed has only not printed its result yet, unless it paused for t.Parallel().
func (r *runnerScope) begin(ev scopeEvent) {
	kept := r.open[:0]
	for _, running := range r.open {
		switch {
		case ev.inside(running):
			kept = append(kept, running)
		case ev.parent() == running.parent() && !r.paused[running.name()]:
		default:
			r.overlaps = append(r.overlaps, running.name()+" and "+ev.name())
			kept = append(kept, running)
		}
	}
	r.open = append(kept, ev)
}

func (r *runnerScope) end(ev scopeEvent) {
	delete(r.paused, ev.name())
	for i, running := range r.open {
		if running.name() == ev.name() {
			r.open = append(r.open[:i], r.open[i+1:]...)
			return
		}
	}
}

// mark posts one test boundary to the agent; a failure is logged, never fatal.
func (r *runnerScope) mark(ev scopeEvent, post func(context.Context, string, int, time.Time) error) {
	if ev.at.IsZero() {
		r.readTimed++
	}
	ctx, cancel := context.WithTimeout(r.ctx, scopeCallTimeout)
	defer cancel()
	if err := post(ctx, ev.name(), 0, ev.at); err != nil {
		r.logger.Debug("failed to report a test boundary to the agent", zap.String("test", ev.name()), zap.Error(err))
	}
}

// usedReadTime reports whether any boundary had to be stamped when keploy read it, for want of a runner time.
func (r *runnerScope) usedReadTime() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readTimed > 0
}

// overlapping lists the pairs of tests that ran at the same time, in the order they were seen.
func (r *runnerScope) overlapping() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.overlaps...)
}
