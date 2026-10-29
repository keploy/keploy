package docker

import (
	"bytes"
	"sort"
	"strings"
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
// What it holds is already cut: composeLogConsumer records the line it
// printed, after log.RedactChildOutput, never the line as it came. A read
// cuts it again, so a value registered after its line was recorded is cut
// too.
//
// It is bounded by bytes, never by time, so a chatty app cannot grow it
// without limit over a long run:
//
//   - all containers together hold at most appTailBudget bytes of output;
//   - each holds at most appTailShare(n) of them, n being how many it holds
//     now: appTailMaxContainerBytes while few are held, an equal part of the
//     budget once more are, and never less than appTailMinContainerBytes;
//   - so it holds at most appTailBudget/appTailMinContainerBytes containers.
//     Past that, the one that printed least recently goes, whole.
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
	appTailMinContainerBytes = 64 << 10
	appTailMaxContainers     = appTailBudget / appTailMinContainerBytes
)

// AppOutput is one container's tail: Name as compose names the container in
// its output (the container_name its service declares, else
// <service>-<number>), Service its compose service when known, Text its last
// lines, each ended by "\n", and Dropped the bytes of older output it no
// longer holds.
type AppOutput struct {
	Name    string
	Service string
	Text    []byte
	Dropped int64
}

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

type appTails struct {
	mu   sync.Mutex
	m    map[string]*appTail
	seq  uint64
	gone int
}

var appOutput = &appTails{m: map[string]*appTail{}}

// appTailShare is the most one container holds while n are held.
func appTailShare(n int) int {
	if n < 1 {
		n = 1
	}
	share := appTailBudget / n
	if share > appTailMaxContainerBytes {
		share = appTailMaxContainerBytes
	}
	if share < appTailMinContainerBytes {
		share = appTailMinContainerBytes
	}
	return share
}

// RecordAppOutput adds one line the container compose names name printed,
// already cut, without its line end, to that container's tail. service is
// its compose service, or "" when not known.
func RecordAppOutput(name, service, line string) {
	appOutput.record(name, service, line)
}

func (s *appTails) record(name, service, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.m[name]
	if t == nil {
		if len(s.m) >= appTailMaxContainers {
			s.evictOldestLocked()
		}
		t = &appTail{first: time.Now()}
		s.m[name] = t
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
	share := appTailShare(len(s.m))
	if len(line)+1 > share {
		cut := len(line) + 1 - share
		t.dropped += int64(cut)
		line = line[cut:]
	}
	t.buf = append(t.buf, line...)
	t.buf = append(t.buf, '\n')
	t.trim(share)
}

func (s *appTails) evictOldestLocked() {
	var oldest string
	var oldestTail *appTail
	for name, t := range s.m {
		if oldestTail == nil || t.last < oldestTail.last {
			oldest, oldestTail = name, t
		}
	}
	delete(s.m, oldest)
	s.gone++
}

// trim drops the tail's oldest whole lines until it holds at most limit
// bytes. It drops down to three quarters of limit, so the copy it makes is
// paid once per quarter of limit appended, not once per line.
//
// The last line is never dropped for being long: the cut never moves past
// its start. When it alone is longer than three quarters of limit it is kept
// whole if it fits in limit, else its last limit bytes are kept (the line's
// end, where a stack trace ends), and the rest of it counts as dropped.
func (t *appTail) trim(limit int) {
	if len(t.buf) <= limit {
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
	kept := make([]byte, len(t.buf)-cut, limit)
	copy(kept, t.buf[cut:])
	t.buf = kept
}

// AppOutputTail is one container's tail, cut again of the values the log
// redactor holds now, or nil when it holds none for name.
func AppOutputTail(name string) []byte {
	s := appOutput
	s.mu.Lock()
	return s.copyAndUnlock(s.m[name])
}

// ServiceOutputTail is the tail of the compose service's container that
// printed last, cut again as AppOutputTail cuts it, or nil when it holds
// none for service. It needs no daemon, so it answers after the container is
// gone.
func ServiceOutputTail(service string) []byte {
	s := appOutput
	s.mu.Lock()
	var newest *appTail
	for _, t := range s.m {
		if service != "" && t.service == service && (newest == nil || t.last > newest.last) {
			newest = t
		}
	}
	return s.copyAndUnlock(newest)
}

// copyAndUnlock copies t's text, unlocks s, and cuts the copy again.
func (s *appTails) copyAndUnlock(t *appTail) []byte {
	var text []byte
	if t != nil {
		text = append([]byte(nil), t.buf...)
	}
	s.mu.Unlock()
	if text == nil {
		return nil
	}
	return cutAgain(text)
}

// AppOutputTails is every container's tail, cut again as AppOutputTail cuts
// it, in the order the containers first printed, and how many containers'
// tails were dropped whole to stay in budget.
func AppOutputTails() ([]AppOutput, int) {
	s := appOutput
	s.mu.Lock()
	type held struct {
		out   AppOutput
		first time.Time
	}
	all := make([]held, 0, len(s.m))
	for name, t := range s.m {
		all = append(all, held{
			out:   AppOutput{Name: name, Service: t.service, Text: append([]byte(nil), t.buf...), Dropped: t.dropped},
			first: t.first,
		})
	}
	gone := s.gone
	s.mu.Unlock()

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].first.Equal(all[j].first) {
			return all[i].out.Name < all[j].out.Name
		}
		return all[i].first.Before(all[j].first)
	})
	outs := make([]AppOutput, len(all))
	for i, h := range all {
		h.out.Text = cutAgain(h.out.Text)
		outs[i] = h.out
	}
	return outs, gone
}

// cutAgain cuts each line of text of the values the log redactor holds now.
func cutAgain(text []byte) []byte {
	if !keployLog.CutsChildOutput() {
		return text
	}
	lines := strings.SplitAfter(string(text), "\n")
	var b strings.Builder
	b.Grow(len(text))
	for _, l := range lines {
		body, ended := strings.CutSuffix(l, "\n")
		b.WriteString(keployLog.RedactChildOutput(body))
		if ended {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// ForgetAppOutput drops every container's tail. For tests, and for a process
// that runs more than one replay and wants each one's tails alone.
func ForgetAppOutput() {
	s := appOutput
	s.mu.Lock()
	s.m = map[string]*appTail{}
	s.gone = 0
	s.mu.Unlock()
}
