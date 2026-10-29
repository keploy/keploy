package docker

import (
	"bytes"
	"sort"
	"sync"
	"time"

	keployLog "go.keploy.io/server/v3/utils/log"
)

// The app-output tail: the last of what each container of keploy's own
// in-process compose stack printed, as keploy took it from the attach stream.
//
// It exists so nothing on that path reads `docker logs`. `docker logs` reads
// what the daemon's log driver stored, and the default driver (json-file)
// stores every line a container prints in a file on the docker host's disk:
// an app that prints a value it was handed as a Secret puts that value on
// disk there, cut of nothing. Reading the attach stream instead lets the
// containers run with `--log-driver none`, and keeps one copy of the output,
// cut, in memory.
//
// What it holds is already cut: recording a line cuts it of the values the
// log redactor holds (log.RedactChildOutput) and stores only the cut line,
// whoever records it. A read cuts it again, so a value registered after its
// line was recorded is cut too.
//
// Each ComposeRunner.Up of a project starts a new run of that project's
// tails (keploy runs one Up per test set): the tails it held are dropped, and
// a line that an earlier Up's attach stream still delivers is not recorded.
// So a tail holds one run's output, as `docker logs` of that run's container
// did: the app of one test set never shows the previous set's lines as its
// own, and a set whose app printed nothing reads nothing. A tail stays after
// compose removed its container, until the project's next Up.
//
// It is bounded by bytes, never by time, so a chatty app cannot grow it
// without limit over a long run. The bounds are on the memory the tails'
// buffers take, not only on the text they hold:
//
//   - at most appTailMaxContainers containers are held; past that, the one
//     that printed least recently goes, whole;
//   - each of the n held takes at most appTailShare(n) bytes of buffer:
//     appTailMaxContainerBytes while few are held, an equal part of
//     appTailBudget once more are (so never less than appTailBudget /
//     appTailMaxContainers, 64 KiB);
//   - so all buffers together take at most appTailBudget bytes. A buffer is
//     sized from the text it keeps (room to grow to twice it, never past its
//     share), and one whose share shrank is reallocated to fit the new share.
//     While one line is recorded, its container may briefly take one more
//     buffer of at most twice its share.
//
// A container that crashed stops printing and so stays the longest, until
// that many newer containers print: the crashed one is the one whose output
// is wanted.
//
// Lines are kept whole: a container's oldest lines go first, a whole line at
// a time, and the newest line always stays. A line longer than its
// container's share keeps its end, whether it is the first line the tail
// holds or comes after others, and when a new container shrinks the share.
const (
	appTailBudget            = 16 << 20
	appTailMaxContainerBytes = 4 << 20
	appTailMaxContainers     = 256
	// appTailMaxRuns bounds the runs remembered, one per compose project
	// this process brought up. Past it, the run that started least recently
	// among the projects that hold no tail is forgotten (see forgetRunLocked).
	appTailMaxRuns = 4096
)

// AppOutput is one container's tail: Project the compose project it ran in,
// Name as compose names the
// container in its output (the container_name its service declares, else
// <service>-<number>), Service its compose service when known, Text its last
// lines, each ended by "\n", and Dropped the bytes of older output it no
// longer holds.
type AppOutput struct {
	Project string
	Name    string
	Service string
	Text    []byte
	Dropped int64
}

// tailKey is one container of one compose project.
type tailKey struct{ project, name string }

type appTail struct {
	service string
	buf     []byte
	dropped int64
	// last orders containers for eviction: the one that printed least
	// recently goes first. A sequence, not a clock, so two lines in the same
	// clock tick still order.
	last uint64
	// first is when the container first printed, for AppOutputTails' order.
	first time.Time
}

// tailRun is a compose project's current run: the Up its tails are of.
type tailRun struct {
	id uint64
	// gone is how many of this run's tails were dropped whole to stay in
	// budget.
	gone int
}

type appTails struct {
	mu   sync.Mutex
	m    map[tailKey]*appTail
	runs map[string]*tailRun
	seq  uint64
}

var appOutput = newAppTails()

func newAppTails() *appTails {
	return &appTails{m: map[tailKey]*appTail{}, runs: map[string]*tailRun{}}
}

// appTailShare is the most buffer one container takes while n are held. With
// n at most appTailMaxContainers, n shares together are at most
// appTailBudget.
func appTailShare(n int) int {
	if n < 1 {
		n = 1
	}
	share := appTailBudget / n
	if share > appTailMaxContainerBytes {
		share = appTailMaxContainerBytes
	}
	return share
}

// startRun starts a new run of project's tails, for one Up: it drops the
// tails the project holds, and returns the run's id, which the Up's consumer
// records with. A line recorded with an earlier id is not kept.
func (s *appTails) startRun(project string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if k.project == project {
			delete(s.m, k)
		}
	}
	s.seq++
	s.runs[project] = &tailRun{id: s.seq}
	if len(s.runs) > appTailMaxRuns {
		s.forgetRunLocked()
	}
	return s.seq
}

// forgetRunLocked forgets the run that started least recently among the
// projects that hold no tail. One always exists: there are more runs than the
// appTailMaxContainers tails can belong to. A line that the forgotten run's Up
// still delivers is then not kept, which loses output, never mixes it.
func (s *appTails) forgetRunLocked() {
	holding := make(map[string]bool, len(s.m))
	for k := range s.m {
		holding[k.project] = true
	}
	oldest := ""
	var oldestRun *tailRun
	for project, r := range s.runs {
		if !holding[project] && (oldestRun == nil || r.id < oldestRun.id) {
			oldest, oldestRun = project, r
		}
	}
	if oldestRun != nil {
		delete(s.runs, oldest)
	}
}

// RecordAppOutput adds one line that project's container compose names name
// printed, without its line end, to that container's tail in the project's
// current run, cut first of the values the log redactor holds: only the cut
// line is held, whoever records it. service is its compose service, or ""
// when not known. ComposeRunner.Up's consumer records what the stack prints;
// this is for output that reaches keploy another way, and for tests.
func RecordAppOutput(project, name, service, line string) {
	cut := keployLog.RedactChildOutput(line)
	s := appOutput
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[project] == nil {
		s.seq++
		s.runs[project] = &tailRun{id: s.seq}
		if len(s.runs) > appTailMaxRuns {
			s.forgetRunLocked()
		}
	}
	s.addLocked(tailKey{project: project, name: name}, service, cut, 0)
}

// record cuts one line a container of project printed (without its line end)
// of the values the log redactor holds, adds the cut line to the container's
// tail when run is the project's current run, and returns the cut line, for
// the consumer to print. lost is how many bytes of the line's start the
// caller did not keep (a line longer than it holds); they count as dropped.
func (s *appTails) record(project string, run uint64, name, service, line string, lost int64) string {
	cut := keployLog.RedactChildOutput(line)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[project]; r != nil && r.id == run {
		s.addLocked(tailKey{project: project, name: name}, service, cut, lost)
	}
	return cut
}

func (s *appTails) addLocked(k tailKey, service, line string, lost int64) {
	t := s.m[k]
	if t == nil {
		if len(s.m) >= appTailMaxContainers {
			s.evictOldestLocked()
		}
		t = &appTail{first: time.Now()}
		s.m[k] = t
		// One more container: every tail gets a smaller share.
		share := appTailShare(len(s.m))
		for _, other := range s.m {
			other.trim(share)
		}
	}
	if service != "" {
		t.service = service
	}
	s.seq++
	t.last = s.seq
	t.dropped += lost
	share := appTailShare(len(s.m))
	if len(line)+1 > share {
		cut := len(line) + 1 - share
		t.dropped += int64(cut)
		line = line[cut:]
	}
	t.add(line, share)
	t.trim(share)
}

func (s *appTails) evictOldestLocked() {
	var oldest tailKey
	var oldestTail *appTail
	for k, t := range s.m {
		if oldestTail == nil || t.last < oldestTail.last {
			oldest, oldestTail = k, t
		}
	}
	delete(s.m, oldest)
	if r := s.runs[oldest.project]; r != nil {
		r.gone++
	}
}

// add appends line and its "\n" to the tail. A buffer it has to grow grows to
// twice its size, but not past limit unless the line needs it; trim then
// brings it back within limit.
//
// line is weighed against the buffer's free room, and then both lengths
// against limit, before any sum is made, so every size below is plainly made
// of values limit bounds (CodeQL's allocation-size check needs to see that;
// the caller keeping them within limit is not enough for it).
func (t *appTail) add(line string, limit int) {
	buf := t.buf
	switch {
	case len(line) < cap(buf)-len(buf):
		// line and its "\n" fit in the free room.
	case len(buf) <= limit && len(line) < limit:
		// The buffer is grown by hand, to twice its size held to limit, or
		// to need when that is more, so append's own growth never takes
		// more. need is at most twice limit.
		need := len(buf) + len(line) + 1
		c := twiceHeldTo(cap(buf), limit)
		if c < need {
			c = need
		}
		grown := make([]byte, len(buf), c)
		copy(grown, buf)
		buf = grown
	default:
		// Not reached: addLocked cuts the line to under limit, and trim
		// keeps the buffer within limit. Were it reached, append would
		// grow the buffer itself, and trim would bring it back.
	}
	buf = append(buf, line...)
	t.buf = append(buf, '\n')
}

// twiceHeldTo is twice x, or limit when that is less, for x and limit >= 0.
// It is worked out from the room x leaves under limit, with no product or
// sum, so it cannot overflow for any such x (2*x can).
func twiceHeldTo(x, limit int) int {
	if room := limit - x; x < room {
		// 2x is under limit: limit-(room-x) is x+x.
		return limit - (room - x)
	}
	return limit
}

// trim drops the tail's oldest whole lines until it holds at most limit
// bytes, and leaves it a buffer of at most limit bytes. It drops down to
// three quarters of limit, so the copy it makes is paid once per quarter of
// limit appended, not once per line. A tail that already holds no more than
// limit keeps its text, and is moved to a buffer that fits when its buffer is
// larger than limit (its share shrank).
//
// The last line is never dropped for being long: the cut never moves past
// its start. When it alone is longer than three quarters of limit it is kept
// whole if it fits in limit, else its last limit bytes are kept (the line's
// end, where a stack trace ends), and the rest of it counts as dropped.
func (t *appTail) trim(limit int) {
	if len(t.buf) <= limit {
		if cap(t.buf) > limit {
			t.keepFrom(0, limit)
		}
		return
	}
	keep := limit * 3 / 4
	cut := len(t.buf) - keep
	// buf ends with "\n", so the last line starts after the "\n" before it.
	last := bytes.LastIndexByte(t.buf[:len(t.buf)-1], '\n') + 1
	switch {
	case cut <= last:
		// The cut is in an older line: move it to that line's end, so the
		// tail starts at a line. buf[last-1] is a "\n", so this stops at
		// last at the latest.
		cut += bytes.IndexByte(t.buf[cut-1:last], '\n')
	case len(t.buf)-last <= limit:
		cut = last
	default:
		cut = len(t.buf) - limit
	}
	t.dropped += int64(cut)
	t.keepFrom(cut, limit)
}

// keepFrom moves buf[from:] to a buffer of its own, sized from what it keeps:
// room to grow to twice its length, never past limit (but always room for
// the text).
func (t *appTail) keepFrom(from, limit int) {
	n := len(t.buf) - from
	c := twiceHeldTo(n, limit)
	if c < n {
		c = n
	}
	kept := make([]byte, n, c)
	copy(kept, t.buf[from:])
	t.buf = kept
}

// AppOutputTail is the tail of project's container compose names name, cut
// again of the values the log redactor holds now, or nil when it holds none
// for it in the project's current run.
func AppOutputTail(project, name string) []byte {
	return appOutput.newest(project, func(k tailKey, _ *appTail) bool { return k.name == name })
}

// ServiceOutputTail is the tail of the container of project's compose
// service that printed last, cut again as AppOutputTail cuts it, or nil when
// it holds none for service in the project's current run. It needs no
// daemon, so it answers after the container is gone.
func ServiceOutputTail(project, service string) []byte {
	if service == "" {
		return nil
	}
	return appOutput.newest(project, func(_ tailKey, t *appTail) bool { return t.service == service })
}

// newest is the text of project's tail that printed last of those match
// accepts, cut again, or nil when none is held.
func (s *appTails) newest(project string, match func(tailKey, *appTail) bool) []byte {
	s.mu.Lock()
	var newest *appTail
	for k, t := range s.m {
		if k.project == project && match(k, t) && (newest == nil || t.last > newest.last) {
			newest = t
		}
	}
	var text []byte
	if newest != nil {
		text = append([]byte(nil), newest.buf...)
	}
	s.mu.Unlock()
	if text == nil {
		return nil
	}
	return cutAgain(text)
}

// AppOutputTails is every container's tail, cut again as AppOutputTail cuts
// it, in the order the containers first printed, and how many containers'
// tails of the projects' current runs were dropped whole to stay in budget.
func AppOutputTails() ([]AppOutput, int) {
	s := appOutput
	s.mu.Lock()
	type held struct {
		out   AppOutput
		first time.Time
	}
	all := make([]held, 0, len(s.m))
	for k, t := range s.m {
		all = append(all, held{
			out:   AppOutput{Project: k.project, Name: k.name, Service: t.service, Text: append([]byte(nil), t.buf...), Dropped: t.dropped},
			first: t.first,
		})
	}
	gone := 0
	for _, r := range s.runs {
		gone += r.gone
	}
	s.mu.Unlock()

	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.first.Equal(b.first) {
			return a.first.Before(b.first)
		}
		if a.out.Project != b.out.Project {
			return a.out.Project < b.out.Project
		}
		return a.out.Name < b.out.Name
	})
	outs := make([]AppOutput, len(all))
	for i, h := range all {
		h.out.Text = cutAgain(h.out.Text)
		outs[i] = h.out
	}
	return outs, gone
}

// cutAgain cuts each line of text, a copy the caller owns, of the values the
// log redactor holds now.
func cutAgain(text []byte) []byte {
	if !keployLog.CutsChildOutput() {
		return text
	}
	var b bytes.Buffer
	b.Grow(len(text))
	for rest := text; len(rest) > 0; {
		body, after, ended := bytes.Cut(rest, []byte{'\n'})
		b.WriteString(keployLog.RedactChildOutput(string(body)))
		if ended {
			b.WriteByte('\n')
		}
		rest = after
	}
	return b.Bytes()
}

// ForgetAppOutput drops every container's tail and every count of tails
// dropped. For tests. A run in progress keeps recording into fresh tails.
func ForgetAppOutput() {
	s := appOutput
	s.mu.Lock()
	s.m = map[tailKey]*appTail{}
	for _, r := range s.runs {
		r.gone = 0
	}
	s.mu.Unlock()
}
