package starts

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

type Proc interface {
	Parent(pid uint32) (uint32, bool)
	Birth(pid uint32) (time.Time, bool)
	Program(pid uint32) string
}

type Start struct {
	Ref     string
	Key     string
	Program string
	Place   string
	N       int
	Dir     string
	Set     string
	Root    uint32
	Born    time.Time
	Worker  uint32
	First   time.Time
	Ready   time.Time
	Port    uint16
	Bound   string
	App     bool
}

type frame struct {
	place  string
	opened time.Time
	counts map[string]int
	seq    uint64 // order of the Begin that opened it, unique per registry
}

type worker struct {
	pid    uint32
	dir    string
	frames []*frame
	suite  bool
	born   time.Time
}

type procKey struct {
	pid  uint32
	born int64
}

type Registry struct {
	mu       sync.Mutex
	proc     Proc
	slack    time.Duration
	workers  map[uint32]*worker
	all      []*Start
	byRoot   map[procKey]*Start
	byProc   map[procKey]*Start
	byPID    map[uint32]*Start
	conns    map[string]uint32
	child    map[string]bool
	root     string
	sets     map[string]models.SetTable
	universe map[string]struct{}
	marked   bool
	onMark   []func()
	begins   uint64 // Begin calls so far; numbers test frames
	// origin is, under the proven switch, the process each recorded
	// connection came from (origin.go).
	origin origins
	// dest is each connection's address, which every recorded mock names
	// (dest.go).
	dest destinations
	// instances are the live test workers' keploy instances
	// (instances.go).
	instances liveInstances
}

// ParentOf is the parent of process pid in this host's process table, read
// as Default reads it: /proc/<pid>/stat on Linux, the kernel's process table
// (sysctl kern.proc.pid) on macOS, which has no /proc, and nothing elsewhere.
// It is false when pid names no process it can read.
func ParentOf(pid uint32) (uint32, bool) { return sysProc{}.Parent(pid) }

func New(p Proc, slack time.Duration) *Registry {
	return &Registry{proc: p, slack: slack}
}

func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers = nil
	r.all = nil
	r.byRoot = nil
	r.byProc = nil
	r.byPID = nil
	r.conns = nil
	r.child = nil
	r.root = ""
	r.sets = nil
	r.universe = nil
	r.marked = false
	r.resetOrigins()
	r.resetDests()
}

func (r *Registry) Begin(pid uint32, name, dir string, suite bool, at time.Time) {
	if pid == 0 || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.worker(pid)
	if dir != "" && (suite || w.dir == "") {
		w.dir = dir
	}
	if suite {
		w.frames = []*frame{{place: "", opened: at, counts: map[string]int{}}}
		w.suite = true
		return
	}
	if len(w.frames) == 0 {
		w.frames = []*frame{{place: "", opened: at, counts: map[string]int{}}}
	}
	r.begins++
	w.frames = append(w.frames, &frame{place: name, opened: at, counts: map[string]int{}, seq: r.begins})
}

func (r *Registry) End(pid uint32, name string, suite bool, at time.Time) {
	if pid == 0 || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[pid]
	if !ok {
		return
	}
	if suite {
		w.frames = nil
		w.suite = false
		return
	}
	for i := len(w.frames) - 1; i >= 1; i-- {
		if w.frames[i].place == name {
			w.frames = w.frames[:i]
			if i == 1 {
				w.frames[0] = &frame{place: "after:" + name, opened: at, counts: map[string]int{}}
			}
			return
		}
	}
}

func (r *Registry) worker(pid uint32) *worker {
	if r.workers == nil {
		r.workers = map[uint32]*worker{}
	}
	w, ok := r.workers[pid]
	if !ok {
		w = &worker{pid: pid}
		if r.proc != nil {
			w.born, _ = r.proc.Birth(pid)
		}
		r.workers[pid] = w
	}
	return w
}

func (r *Registry) Note(conn string, pid uint32, at time.Time) {
	if pid == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if conn != "" {
		if r.conns == nil {
			r.conns = map[string]uint32{}
		}
		r.conns[conn] = pid
	}
	r.startOf(pid, at)
}

func (r *Registry) Child(conn string) {
	if conn == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.child == nil {
		r.child = map[string]bool{}
	}
	r.child[conn] = true
}

func (r *Registry) Ready(pid uint32, port uint16, at time.Time) bool {
	if pid == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.rootStart(pid)
	if s == nil {
		s = r.startOf(pid, at)
	}
	if s == nil {
		return false
	}
	s.Ready = at
	s.Port = port
	s.App = true
	r.firstMark()
	return true
}

func (r *Registry) Mark(pid uint32, at time.Time) bool {
	if pid == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.rootStart(pid)
	if s == nil {
		s = r.startOf(pid, at)
	}
	if s == nil {
		return false
	}
	s.App = true
	r.firstMark()
	return true
}

func (r *Registry) OnMark(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMark = append(r.onMark, f)
}

func (r *Registry) firstMark() {
	if r.marked {
		return
	}
	r.marked = true
	for _, f := range r.onMark {
		go f()
	}
}

func (r *Registry) Dependency(pid uint32) bool {
	if pid == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.marked {
		return false
	}
	s := r.rootStart(pid)
	return s == nil || !s.App
}

func (r *Registry) Stamp(m *models.Mock) {
	if m == nil {
		return
	}
	// The process its call came from (proven switch): the
	// sync manager stamped it as its parser handed it over, else here.
	if provenswitch.On() {
		r.StampOrigin(m)
		r.StampInstance(m)
	}
	// The address its connection reached, for a mock no parser handed the
	// sync manager, which stamps it as it is handed over (StampDest).
	r.StampDest(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	id := m.ConnectionID
	if id == "" && m.Spec.Metadata != nil {
		id = m.Spec.Metadata["connID"]
	}
	if r.child[id] {
		if m.Spec.Metadata == nil {
			m.Spec.Metadata = map[string]string{}
		}
		m.Spec.Metadata["startedByTests"] = "true"
	}
	pid := m.SourcePID
	if pid == 0 {
		pid = r.conns[id]
	}
	if pid == 0 {
		return
	}
	m.SourcePID = pid
	if m.Start != "" {
		return
	}
	if s := r.byPID[pid]; s != nil {
		m.Start = s.Key
		m.StartRef = s.Ref
	}
}

func (r *Registry) List() []Start {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Start, 0, len(r.all))
	for _, s := range r.all {
		out = append(out, *s)
	}
	return out
}

func (r *Registry) chain(pid uint32) ([]uint32, *worker) {
	var path []uint32
	cur := pid
	for i := 0; i < 64 && cur > 1; i++ {
		if w, ok := r.workers[cur]; ok {
			return path, w
		}
		path = append(path, cur)
		p, ok := r.proc.Parent(cur)
		if !ok {
			return path, nil
		}
		cur = p
	}
	return path, nil
}

// Scope names the scope a call from pid runs in, resolved as View resolves
// the test whose mocks it sees (attribute): the test frame open to it (a
// re-run of the same test is a new frame, so a new scope), or, with no test
// open to it, the registered worker it descends from. "" when it has neither.
//
// It follows View's attribution exactly, so a call's stateful cursors are
// those of the test whose recordings it is served — including currentFrame's
// fallback, under which a worker between its own tests is attributed to the
// one test open elsewhere, and a call of no worker to the one test open. It
// does not tell app starts within one test apart.
func (r *Registry) Scope(pid uint32) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := r.attribute(pid)
	switch {
	case err != nil || a.w == nil:
		return ""
	case a.f != nil:
		return fmt.Sprintf("t%d", a.f.seq)
	}
	return fmt.Sprintf("w%d", a.w.pid)
}

// Unattributed is why a call is served no recording: its process descends
// from no process that began a test or the suite (an app server the tests did
// not start, a call from a test process whose pid was sent from another pid
// namespace, or one whose pid the proxy could not read), while more than one
// test is open, so keploy cannot tell which test it belongs to.
type Unattributed struct {
	PID  uint32 // 0 when the proxy could not read it
	Open int    // the tests open
}

func (e *Unattributed) Error() string {
	who := fmt.Sprintf("the call from pid %d", e.PID)
	if e.PID == 0 {
		who = "a call whose pid the proxy could not read"
	}
	return fmt.Sprintf("keploy cannot tell which test %s belongs to: %d tests are open, and it comes from no process that began a test or the suite, nor from one they started, so it is served no recording. "+
		"Start the app from the test process that begins its tests, or run the tests one at a time", who, e.Open)
}

// attribution is a call's place in the run: the worker whose set it reads,
// the test open to it, and, for a call of the worker's own process tree, the
// processes below the worker it came through.
type attribution struct {
	path []uint32 // pid and its ancestors below w, nearest first; nil when w is not its own
	w    *worker  // nil when no test is open to a call of no worker
	f    *frame   // the test open to the call; nil for none
}

// attribute places a call from pid. A call of a worker's process tree is that
// worker's, with the test currentFrame gives it. A call of no worker (an app
// server the tests did not start, a call from a test process whose pid was
// sent from another pid namespace, or pid 0, one the proxy could not read) is
// the one test open in the run, when exactly one is, as a begin without a pid
// narrows every connection to its test; with none open it is no test's (w
// nil: View shows it every recording, as before the first test); with more
// than one it is refused (*Unattributed), never served the whole pool.
func (r *Registry) attribute(pid uint32) (attribution, error) {
	path, w := r.chain(pid)
	if w != nil {
		return attribution{path: path, w: w, f: r.currentFrame(w)}, nil
	}
	ow, f, n := r.openTest()
	if n > 1 {
		return attribution{}, &Unattributed{PID: pid, Open: n}
	}
	return attribution{w: ow, f: f}, nil
}

func (r *Registry) rootStart(pid uint32) *Start {
	path, w := r.chain(pid)
	if w == nil {
		return nil
	}
	for _, p := range path {
		b, ok := r.proc.Birth(p)
		if !ok {
			continue
		}
		if s := r.byRoot[procKey{p, b.UnixNano()}]; s != nil {
			return s
		}
	}
	return nil
}

func (r *Registry) startOf(pid uint32, at time.Time) *Start {
	born, _ := r.proc.Birth(pid)
	self := procKey{pid, born.UnixNano()}
	if s := r.byProc[self]; s != nil {
		return s
	}
	path, w := r.chain(pid)
	if w == nil || len(path) == 0 {
		return nil
	}
	if len(w.frames) == 0 {
		w.frames = []*frame{{place: "", opened: at, counts: map[string]int{}}}
	}
	top := w.frames[len(w.frames)-1]
	root := path[len(path)-1]
	for i := len(path) - 1; i >= 0; i-- {
		b, ok := r.proc.Birth(path[i])
		if ok && !b.Before(top.opened.Add(-r.slack)) {
			root = path[i]
			break
		}
	}
	rb, _ := r.proc.Birth(root)
	k := procKey{root, rb.UnixNano()}
	if r.byRoot == nil {
		r.byRoot = map[procKey]*Start{}
		r.byProc = map[procKey]*Start{}
		r.byPID = map[uint32]*Start{}
	}
	s := r.byRoot[k]
	if s == nil {
		program := Name(r.proc.Program(pid))
		top.counts[program]++
		n := top.counts[program]
		s = &Start{
			Ref:     fmt.Sprintf("s%d", len(r.all)+1),
			Key:     KeyOf(program, n, top.place),
			Program: program,
			Place:   top.place,
			N:       n,
			Dir:     w.dir,
			Root:    root,
			Born:    rb,
			Worker:  w.pid,
			First:   at,
		}
		r.bind(s)
		r.all = append(r.all, s)
		r.byRoot[k] = s
	}
	r.byProc[self] = s
	r.byPID[pid] = s
	return s
}

func KeyOf(program string, n int, place string) string {
	key := fmt.Sprintf("%s#%d", program, n)
	if place != "" {
		key += "@" + place
	}
	return key
}

func Name(path string) string {
	base := filepath.Base(path)
	if base == "." || base == "/" || base == "" {
		return "app"
	}
	out := strings.TrimRight(numbers.ReplaceAllString(base, ""), "-_.")
	if out == "" || strings.Trim(out, "0123456789") == "" {
		return "app"
	}
	return out
}

var numbers = regexp.MustCompile(`[-_.]\d+`)

func (r *Registry) SetTable(root string, sets map[string]models.SetTable) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.root = root
	r.sets = sets
	r.universe = map[string]struct{}{}
	for _, st := range sets {
		for _, names := range st.Boots {
			for _, n := range names {
				r.universe[n] = struct{}{}
			}
		}
		for _, owned := range st.Tests {
			for _, o := range owned {
				r.universe[o.Name] = struct{}{}
			}
		}
		for _, n := range st.Runner {
			r.universe[n] = struct{}{}
		}
	}
	for _, s := range r.all {
		r.bind(s)
	}
}

func (r *Registry) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sets != nil
}

func (r *Registry) Start(pid uint32, dir string) (string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[pid]; ok && dir == "" {
		dir = w.dir
	}
	set := r.setOf(dir)
	return set, r.sets[set].Start
}

func (r *Registry) Live() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, w := range r.workers {
		if !w.suite && len(w.frames) < 2 {
			continue
		}
		if !w.born.IsZero() {
			if b, ok := r.proc.Birth(w.pid); !ok || !b.Equal(w.born) {
				continue
			}
		}
		set := r.setOf(w.dir)
		if _, ok := r.sets[set]; ok && !slices.Contains(out, set) {
			out = append(out, set)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Registry) setOf(dir string) string {
	if dir == "" {
		return ""
	}
	if r.root != "" {
		if rel, err := filepath.Rel(r.root, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rel = filepath.ToSlash(rel)
			if _, ok := r.sets[rel]; ok {
				return rel
			}
		}
	}
	slash := filepath.ToSlash(dir)
	best := ""
	for name := range r.sets {
		if (slash == name || strings.HasSuffix(slash, "/"+name)) && len(name) > len(best) {
			best = name
		}
	}
	return best
}

func (r *Registry) bind(s *Start) {
	if r.sets == nil {
		return
	}
	s.Set = r.setOf(s.Dir)
	st, ok := r.sets[s.Set]
	if !ok {
		return
	}
	if _, ok := st.Boots[s.Key]; ok {
		s.Bound = s.Key
		return
	}
	fallback := KeyOf(s.Program, 1, "")
	if _, ok := st.Boots[fallback]; ok {
		s.Bound = fallback
	}
}

// currentFrame is the test frame a call from w's process tree runs in: w's own
// open test, or — for a worker with none (an app started once for the suite)
// — the one test open on any worker, when exactly one is. nil otherwise.
func (r *Registry) currentFrame(w *worker) *frame {
	if w != nil && len(w.frames) > 1 {
		return w.frames[len(w.frames)-1]
	}
	_, f, _ := r.openTest()
	return f
}

// openTest is the one test open in the run and the worker it is open on, when
// exactly one is (nil, nil otherwise), and how many are open.
func (r *Registry) openTest() (*worker, *frame, int) {
	var (
		ow   *worker
		open *frame
	)
	count := 0
	for _, o := range r.workers {
		if len(o.frames) > 1 {
			ow, open = o, o.frames[len(o.frames)-1]
			count++
		}
	}
	if count != 1 {
		return nil, nil, count
	}
	return ow, open, count
}

// Owns reports whether the named mock is a recording of the test open to a
// call from pid, resolved as View resolves it (attribute): whether that
// test's mapping names it. A boot, runner or unmapped mock, which View also
// shows the call, is no test's own, and with no test open to pid nothing is.
// known is false when pid has no view (View's ok, or its refusal), and owned
// is then false.
func (r *Registry) Owns(pid uint32, name string) (owned, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets == nil {
		return false, false
	}
	a, err := r.attribute(pid)
	if err != nil || a.w == nil {
		return false, false
	}
	st, ok := r.sets[r.setOf(a.w.dir)]
	if !ok {
		return false, false
	}
	if a.f == nil {
		return false, true
	}
	for _, o := range st.Tests[a.f.place] {
		if o.Name == name {
			return true, true
		}
	}
	return false, true
}

// View is what a call from pid may be served now, in the run's replay table:
// each recording it may be served, ranked (lower first), and the universe of
// every name the table maps, so a recording mapped to none (absent from it)
// is shown too. ok is false when the call has no view and is shown every
// recording: no replay table, a worker's set the table lacks, or a call of no
// worker with no test open. err refuses the call every recording: it is of no
// worker while more than one test is open (*Unattributed, attribute).
func (r *Registry) View(pid uint32, at time.Time) (map[string]int, map[string]struct{}, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets == nil {
		return nil, nil, false, nil
	}
	a, err := r.attribute(pid)
	if err != nil {
		return nil, nil, false, err
	}
	if a.w == nil {
		return nil, nil, false, nil
	}
	var s *Start
	if len(a.path) > 0 {
		s = r.startOf(pid, at)
	}
	set := r.setOf(a.w.dir)
	st, ok := r.sets[set]
	if !ok {
		return nil, nil, false, nil
	}
	test := ""
	if a.f != nil {
		test = a.f.place
	}
	rank := map[string]int{}
	put := func(name string, g int) {
		if old, ok := rank[name]; !ok || g < old {
			rank[name] = g
		}
	}
	if s != nil && s.Bound != "" {
		prefix := s.Program + "#"
		for _, o := range st.Tests[test] {
			switch {
			case o.Start == s.Bound:
				put(o.Name, 1)
			case o.Start != "" && strings.HasPrefix(o.Start, prefix):
				put(o.Name, 3)
			default:
				put(o.Name, 4)
			}
		}
		for _, n := range st.Boots[s.Bound] {
			put(n, 2)
		}
		for _, n := range st.Runner {
			put(n, 4)
		}
		return rank, r.universe, true, nil
	}
	for _, o := range st.Tests[test] {
		put(o.Name, 1)
	}
	for _, n := range st.Runner {
		put(n, 2)
	}
	return rank, r.universe, true, nil
}
