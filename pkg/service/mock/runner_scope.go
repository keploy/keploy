package mock

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"golang.org/x/term"
)

const maxRunnerLine = 1 << 20

// boundarySlack is how close to a window edge a mock has to be to count as at risk of landing in the wrong test.
const boundarySlack = 20 * time.Millisecond

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
	// status and elapsed come with a result line: pass, fail or skip, and how long the test took.
	status  string
	elapsed time.Duration
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
	var ev struct {
		Action, Package, Test, Time string
		Elapsed                     float64
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Test == "" {
		return scopeEvent{}
	}
	kind := kindOfAction(ev.Action)
	if kind == scopeNone {
		return scopeEvent{}
	}
	at, _ := time.Parse(time.RFC3339Nano, ev.Time)
	out := scopeEvent{kind: kind, pkg: ev.Package, test: ev.Test, at: at}
	if kind == scopeEnd {
		out.status = ev.Action
		out.elapsed = time.Duration(math.Round(ev.Elapsed * float64(time.Second)))
	}
	return out
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
		return scopeEvent{kind: scopeEnd, test: fields[2], status: strings.ToLower(strings.TrimSuffix(fields[1], ":")), elapsed: plainElapsed(fields)}
	}
	return scopeEvent{}
}

// plainElapsed reads the "(0.01s)" a result line ends with; zero when it is missing or unreadable.
func plainElapsed(fields []string) time.Duration {
	if len(fields) < 4 {
		return 0
	}
	d, _ := time.ParseDuration(strings.Trim(fields[3], "()"))
	return d
}

// runnerScope turns the wrapped test runner's output into per-test windows and results, the same way for record and replay.
type runnerScope struct {
	mu        sync.Mutex
	partial   []byte
	open      []scopeEvent
	paused    map[string]bool
	overlaps  []string
	starts    map[string]time.Time
	closed    []models.ScopeWindow
	results   []TestOutcome
	readTimed int
	runs      map[string]int
	tops      []scopeEvent
	steps     map[string]string
	parents   map[string]string
	overlong  bool
}

func newRunnerScope() *runnerScope {
	return &runnerScope{paused: map[string]bool{}, starts: map[string]time.Time{}, runs: map[string]int{}, steps: map[string]string{}, parents: map[string]string{}}
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
			if len(r.partial) > maxRunnerLine {
				r.partial, r.overlong = r.partial[:0], true
			}
			return len(p), nil
		}
		if !r.overlong {
			r.handle(parseRunnerLine(string(r.partial[:i])))
		}
		r.overlong = false
		r.partial = r.partial[i+1:]
	}
}

func (r *runnerScope) handle(ev scopeEvent) {
	switch ev.kind {
	case scopeBegin:
		r.begin(ev, r.stamp(ev))
	case scopePause:
		r.paused[ev.name()] = true
	case scopeEnd:
		r.end(ev, r.stamp(ev))
	}
}

// stamp is the runner's own time for ev, or the time keploy read the line when the output carries none.
func (r *runnerScope) stamp(ev scopeEvent) time.Time {
	if !ev.at.IsZero() {
		return ev.at
	}
	r.readTimed++
	return time.Now()
}

// begin opens ev's window; a sibling still listed has only not printed its result yet, so its window closes here, unless it paused for t.Parallel().
func (r *runnerScope) begin(ev scopeEvent, at time.Time) {
	kept := r.open[:0]
	for _, running := range r.open {
		switch {
		case ev.inside(running):
			kept = append(kept, running)
		case ev.parent() == running.parent() && !r.paused[running.name()]:
			r.finish(running.name(), at)
		default:
			r.overlaps = append(r.overlaps, running.name()+" and "+ev.name())
			kept = append(kept, running)
		}
	}
	r.open = append(kept, ev)
	r.starts[ev.name()] = at
	r.parents[ev.name()] = ev.pkg + "."
	if i := strings.LastIndexByte(ev.test, '/'); i >= 0 {
		r.parents[ev.name()] = scopeEvent{pkg: ev.pkg, test: ev.test[:i]}.name()
	}
	if _, sub, ok := strings.Cut(ev.test, "/"); ok {
		r.steps[ev.name()], _, _ = strings.Cut(sub, "/")
		return
	}
	if r.runs[ev.name()] == 0 {
		r.tops = append(r.tops, ev)
	}
	r.runs[ev.name()]++
}

func (r *runnerScope) end(ev scopeEvent, at time.Time) {
	delete(r.paused, ev.name())
	r.finish(ev.name(), at)
	if ev.status != "" {
		r.results = append(r.results, TestOutcome{Name: ev.name(), Status: ev.status, Duration: ev.elapsed})
	}
	for i, running := range r.open {
		if running.name() == ev.name() {
			r.open = append(r.open[:i], r.open[i+1:]...)
			return
		}
	}
}

// finish turns an open test into a window; a test already closed at a sibling's start is left alone.
func (r *runnerScope) finish(name string, at time.Time) {
	start, ok := r.starts[name]
	if !ok {
		return
	}
	delete(r.starts, name)
	r.closed = append(r.closed, models.ScopeWindow{Name: name, Start: start, End: at})
}

// windows lists every test that began and ended, stamped by the runner's clock where the output had one.
func (r *runnerScope) windows() []models.ScopeWindow {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return sealed(append([]models.ScopeWindow(nil), r.closed...), r.parents)
}

// sealed closes the gap between one test and its next sibling: the runner prints a test's start a moment after
// the test began, so a call made in that gap belongs to the test that follows, never to nobody. A subtest never
// starts before its parent, and windows that overlap, from tests running at the same time, are left as they are.
func sealed(windows []models.ScopeWindow, parents map[string]string) []models.ScopeWindow {
	starts := make(map[string]time.Time, len(windows))
	for _, w := range windows {
		starts[w.Name] = w.Start
	}
	order := make([]int, len(windows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		wa, wb := windows[order[a]], windows[order[b]]
		if pa, pb := parents[wa.Name], parents[wb.Name]; pa != pb {
			return pa < pb
		}
		return wa.Start.Before(wb.Start)
	})
	var latest time.Time
	for k, i := range order {
		p := parents[windows[i].Name]
		if k > 0 && parents[windows[order[k-1]].Name] != p {
			latest = time.Time{}
		}
		end := windows[i].End
		if !latest.IsZero() && !latest.After(windows[i].Start) {
			if ps, ok := starts[p]; ok && latest.Before(ps) {
				latest = ps
			}
			windows[i].Start = latest
		}
		if end.After(latest) {
			latest = end
		}
	}
	return windows
}

func (r *runnerScope) stepWindows() []models.ScopeWindow {
	if r == nil {
		return nil
	}
	var out []models.ScopeWindow
	for _, w := range r.windows() {
		r.mu.Lock()
		step, ok := r.steps[w.Name]
		r.mu.Unlock()
		if ok {
			w.Name = step
			out = append(out, w)
		}
	}
	return out
}

func (r *runnerScope) repeated(existed bool) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.tops {
		n := r.runs[ev.name()]
		if n < 2 || ev.pkg == "" {
			continue
		}
		return refusedRun(ev.test, n, ev.pkg, existed)
	}
	return nil
}

// tests lists every result the runner printed, in the order it printed them.
func (r *runnerScope) tests() []TestOutcome {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TestOutcome(nil), r.results...)
}

func teeable(command string) bool {
	if !term.IsTerminal(int(os.Stdout.Fd())) || strings.Contains(command, "go test") {
		return true
	}
	for _, f := range strings.Fields(command) {
		if strings.HasSuffix(f, ".test") || strings.HasSuffix(f, "test2json") || strings.HasSuffix(f, "gotestsum") {
			return true
		}
	}
	return false
}

// mergeWindows keeps the adapter's windows and adds the agent's only for tests the adapter never saw.
func mergeWindows(adapter, agent []models.ScopeWindow) []models.ScopeWindow {
	// A test that marked its own start and end is exact; a window read from the runner's output is late by
	// however long the runner took to print, so it only fills in for tests that did not mark themselves.
	seen := make(map[string]struct{}, len(agent))
	for _, w := range agent {
		seen[w.Name] = struct{}{}
	}
	out := append([]models.ScopeWindow(nil), agent...)
	for _, w := range adapter {
		if _, ok := seen[w.Name]; !ok {
			out = append(out, w)
		}
	}
	return out
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

// nearBoundary counts the mocks whose request fell within slack of any window's start or end.
func nearBoundary(windows []models.ScopeWindow, mocks []capturedMock, slack time.Duration) int {
	n := 0
	for _, mk := range mocks {
		for _, w := range windows {
			if mk.ts.Sub(w.Start).Abs() <= slack || mk.ts.Sub(w.End).Abs() <= slack {
				n++
				break
			}
		}
	}
	return n
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
