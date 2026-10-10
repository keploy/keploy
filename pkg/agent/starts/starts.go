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
	dests    map[string]string
	child    map[string]bool
	root     string
	sets     map[string]models.SetTable
	universe map[string]struct{}
	marked   bool
	onMark   []func()
	begins   uint64 // Begin calls so far; numbers test frames
}

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
	r.dests = nil
	r.child = nil
	r.root = ""
	r.sets = nil
	r.universe = nil
	r.marked = false
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

func (r *Registry) Dest(conn, addr string) {
	if conn == "" || addr == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dests == nil {
		r.dests = map[string]string{}
	}
	r.dests[conn] = addr
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
	r.mu.Lock()
	defer r.mu.Unlock()
	id := m.ConnectionID
	if id == "" && m.Spec.Metadata != nil {
		id = m.Spec.Metadata["connID"]
	}
	if addr := r.dests[id]; addr != "" && m.Spec.Metadata["destAddr"] == "" {
		if m.Spec.Metadata == nil {
			m.Spec.Metadata = map[string]string{}
		}
		m.Spec.Metadata["destAddr"] = addr
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
// the test whose mocks it sees: the test frame currentFrame picks (a re-run of
// the same test is a new frame, so a new scope), or, with no test open to it,
// the registered worker it descends from. "" when pid belongs to no worker.
//
// It follows View's attribution exactly, so a call's stateful cursors are
// those of the test whose recordings it is served — including currentFrame's
// fallback, under which a worker between its own tests is attributed to the
// one test open elsewhere. It does not tell app starts within one test apart.
func (r *Registry) Scope(pid uint32) string {
	if pid == 0 {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, w := r.chain(pid)
	if w == nil {
		return ""
	}
	if f := r.currentFrame(w); f != nil {
		return fmt.Sprintf("t%d", f.seq)
	}
	return fmt.Sprintf("w%d", w.pid)
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

func (r *Registry) currentTest(w *worker) string {
	if f := r.currentFrame(w); f != nil {
		return f.place
	}
	return ""
}

// currentFrame is the test frame a call from w's process tree runs in: w's own
// open test, or — for a worker with none (an app started once for the suite)
// — the one test open on any worker, when exactly one is. nil otherwise.
func (r *Registry) currentFrame(w *worker) *frame {
	if w != nil && len(w.frames) > 1 {
		return w.frames[len(w.frames)-1]
	}
	var open *frame
	count := 0
	for _, o := range r.workers {
		if len(o.frames) > 1 {
			open = o.frames[len(o.frames)-1]
			count++
		}
	}
	if count == 1 {
		return open
	}
	return nil
}

func (r *Registry) View(pid uint32, at time.Time) (map[string]int, map[string]struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets == nil || pid == 0 {
		return nil, nil, false
	}
	path, w := r.chain(pid)
	if w == nil {
		return nil, nil, false
	}
	var s *Start
	if len(path) > 0 {
		s = r.startOf(pid, at)
	}
	set := r.setOf(w.dir)
	st, ok := r.sets[set]
	if !ok {
		return nil, nil, false
	}
	test := r.currentTest(w)
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
		return rank, r.universe, true
	}
	for _, o := range st.Tests[test] {
		put(o.Name, 1)
	}
	for _, n := range st.Runner {
		put(n, 2)
	}
	return rank, r.universe, true
}
