package mockdb

import (
	"fmt"
	"os"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The prune keeps, with each mock it keeps, the mocks that mock was recorded
// against (models.MockDeps): pruneKeeps judges each mock alone, and on its own
// verdict a MySQL COM_STMT_EXECUTE that a test consumed was kept while the
// COM_STMT_PREPARE of its statement was deleted. A live PREPARE is answered by
// the first recorded PREPARE of its query, so every later copy of it goes
// unconsumed, and the next replay could no longer tell which statement the
// kept EXECUTE ran (keploy #4810).
//
// A rewrite reads the file one mock at a time and writes each mock it keeps
// before it reads the next (rewrite.go), and a mock is recorded before the
// mocks that require it, so a pass cannot know, when it drops a mock, that a
// later one needs it. The first pass therefore also gathers the links
// (pruneDeps). When no mock it kept requires a mock it dropped, its file is
// the prune's; otherwise it is discarded and a second pass keeps those mocks
// too, by their place in the file.

// The prune also keeps every async epoch the replay can serve. An async lane's
// mocks (models.AsyncMeta) are not consumed as other mocks are: the replay
// serves a lane's timeline, the last epoch recorded at the position the test
// set reached (async.Engine.Decide), to any poll, as often as polled. Whether
// a poll fetched an epoch in one replay says only when the app polled: the
// prune dropped every epoch after the first tests', and the next replay
// answered the tests after a value changed with the value before it. The
// epochs a later one at the same position supersedes are never served, and go.

// pruneDeps gathers, over one pass of the prune, the links between the mocks
// of the file: what the kept mocks require, and which dropped mocks provide
// what; and the async epochs a replay can serve. It holds keys and places,
// never mocks.
type pruneDeps struct {
	// seen counts the mocks the pass judged: the next one's place.
	seen int
	// need are the keys the kept mocks require.
	need map[string]struct{}
	// providers are the places of the dropped mocks that provide each key.
	providers map[string][]int
	// dropped are the dropped mocks that provide a key, by place: their names
	// and the keys they require themselves, which keeping them makes needed.
	dropped map[int]droppedProvider
	// served are, per async lane and position, the epochs a replay serves
	// there: the last recorded (the highest seq; every one of them on a tie,
	// which the replay orders as it loads them).
	served map[asyncPosition]*servedEpochs
}

type droppedProvider struct {
	name     string
	requires []string
}

// asyncPosition is a place in an async lane's timeline: the lane, and the
// number of tests completed before its epochs take effect
// (models.AsyncMeta.AnchorPos).
type asyncPosition struct {
	lane string
	pos  int
}

type servedEpochs struct {
	seq    int
	epochs []servedEpoch
}

type servedEpoch struct {
	place int
	kept  bool
	droppedProvider
}

// observeAsync notes m, the mock at place, if it is an async epoch.
func (p *pruneDeps) observeAsync(m *models.Mock, place int, kept bool, deps models.MockDeps) {
	a := m.Spec.Async
	if a == nil {
		return
	}
	at := asyncPosition{lane: a.Lane, pos: a.AnchorPos}
	e := servedEpoch{place: place, kept: kept, droppedProvider: droppedProvider{name: m.Name}}
	for _, r := range deps.Requires {
		e.requires = append(e.requires, r.Key)
	}
	if p.served == nil {
		p.served = map[asyncPosition]*servedEpochs{}
	}
	cur := p.served[at]
	switch {
	case cur == nil || a.Seq > cur.seq:
		p.served[at] = &servedEpochs{seq: a.Seq, epochs: []servedEpoch{e}}
	case a.Seq == cur.seq:
		cur.epochs = append(cur.epochs, e)
	}
}

// observe reads the links of the next mock of the file, which the pass kept
// or dropped.
func (p *pruneDeps) observe(m *models.Mock, kept bool) {
	place := p.seen
	p.seen++
	deps := models.DepsOf(m)
	p.observeAsync(m, place, kept, deps)
	if kept {
		for _, r := range deps.Requires {
			if p.need == nil {
				p.need = map[string]struct{}{}
			}
			p.need[r.Key] = struct{}{}
		}
		return
	}
	if len(deps.Provides) == 0 {
		// Nothing can need it.
		return
	}
	if p.providers == nil {
		p.providers = map[string][]int{}
		p.dropped = map[int]droppedProvider{}
	}
	for _, key := range deps.Provides {
		p.providers[key] = append(p.providers[key], place)
	}
	d := droppedProvider{name: m.Name}
	for _, r := range deps.Requires {
		d.requires = append(d.requires, r.Key)
	}
	p.dropped[place] = d
}

// keptAlso is a mock the first pass dropped and the second keeps: its name,
// and whether it is kept as an async epoch a replay serves (else as a mock a
// kept mock requires).
type keptAlso struct {
	name     string
	timeline bool
}

// keepAlso is what the pass dropped and must keep: every async epoch a replay
// serves (see pruneDeps.served), every dropped mock that provides a key a kept
// mock requires, and, as keeping it makes its own requirements needed, what
// those require in turn. Every provider of a needed key is kept, so a replay
// resolves the key as it did before the prune. It maps each mock's place in
// the file to it; nil when the pass dropped nothing it must keep.
func (p *pruneDeps) keepAlso() map[int]keptAlso {
	var out map[int]keptAlso
	queued := make(map[string]struct{}, len(p.need))
	queue := make([]string, 0, len(p.need))
	enqueue := func(key string) {
		if _, ok := queued[key]; !ok {
			queued[key] = struct{}{}
			queue = append(queue, key)
		}
	}
	for key := range p.need {
		enqueue(key)
	}
	for _, at := range p.served {
		for _, e := range at.epochs {
			if e.kept {
				continue
			}
			if out == nil {
				out = map[int]keptAlso{}
			}
			out[e.place] = keptAlso{name: e.name, timeline: true}
			for _, req := range e.requires {
				enqueue(req)
			}
		}
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		for _, place := range p.providers[key] {
			if _, kept := out[place]; kept {
				continue
			}
			if out == nil {
				out = map[int]keptAlso{}
			}
			d := p.dropped[place]
			out[place] = keptAlso{name: d.name}
			for _, req := range d.requires {
				enqueue(req)
			}
		}
	}
	return out
}

// pruneVerdict judges, in a pass of the prune, each mock of the file in turn:
// whether pruneKeeps keeps it, or the first pass found that a replay serves
// it as an async epoch or a kept mock requires it (keepAlso).
type pruneVerdict struct {
	keeps func(*models.Mock) bool
	// keepAlso is the first pass's keepAlso, for the second pass; nil in the
	// first.
	keepAlso map[int]keptAlso
	// deps gathers the links in the first pass.
	deps pruneDeps
	// place is the next mock's place in the file.
	place int
	// firstPass is how many mocks the first pass read, in the second.
	firstPass int
	// asDependency counts the mocks kept only because a kept mock requires
	// them, and asDependencyNames names the first maxPrunedMocksLogged.
	asDependency      int
	asDependencyNames []string
	// asTimeline counts the async epochs kept only because a replay serves
	// them, and asTimelineNames names the first maxPrunedMocksLogged.
	asTimeline      int
	asTimelineNames []string
	// track, set by DeleteMocks, has each pass name every mock it drops, in
	// dropped (the last pass's are what the file loses), and leave a file
	// it drops nothing of unwritten.
	track   bool
	dropped []string
	// asRead, set by DeleteMocks, says the pass changes no mock it keeps,
	// so the yaml/json pass copies each kept document as it was read, byte
	// for byte (prunePass), rather than encoding the decoded mock again:
	// what this keploy's encoder would write differs from a mock saved by
	// hand, by another keploy or with fields this keploy does not know, and
	// taking one mock out must not change another.
	asRead bool
}

// judge reports whether the pass keeps m, the next mock of the file.
func (v *pruneVerdict) judge(m *models.Mock) (bool, error) {
	keep, err := v.judgeAt(m)
	if err == nil && !keep && v.track {
		v.dropped = append(v.dropped, m.Name)
	}
	return keep, err
}

// judgeAt is judge before tracking.
func (v *pruneVerdict) judgeAt(m *models.Mock) (bool, error) {
	place := v.place
	v.place++
	kept := v.keeps(m)
	if v.keepAlso == nil {
		v.deps.observe(m, kept)
		return kept, nil
	}
	also, required := v.keepAlso[place]
	if !required {
		return kept, nil
	}
	if also.name != m.Name {
		// The places are the first pass's: a file that changed between the
		// passes would have the prune keep other mocks than the ones needed.
		return false, fmt.Errorf("the mock file changed while it was pruned: mock %d is %q, the first pass read %q", place, m.Name, also.name)
	}
	switch {
	case kept:
	case also.timeline:
		v.asTimeline++
		if len(v.asTimelineNames) < maxPrunedMocksLogged {
			v.asTimelineNames = append(v.asTimelineNames, also.name)
		}
	default:
		v.asDependency++
		if len(v.asDependencyNames) < maxPrunedMocksLogged {
			v.asDependencyNames = append(v.asDependencyNames, also.name)
		}
	}
	return true, nil
}

// secondPass reports whether the first pass dropped a mock it must keep (see
// keepAlso), and sets v up for the pass that keeps it.
func (v *pruneVerdict) secondPass() bool {
	if v.keepAlso != nil {
		return false
	}
	also := v.deps.keepAlso()
	if also == nil {
		return false
	}
	*v = pruneVerdict{keeps: v.keeps, keepAlso: also, firstPass: v.place, track: v.track, asRead: v.asRead}
	return true
}

// checkPlaces fails a second pass that read a different number of mocks
// than the first, by the same reasoning as judge's name check.
func (v *pruneVerdict) checkPlaces() error {
	if v.keepAlso != nil && v.place != v.firstPass {
		return fmt.Errorf("the mock file changed while it was pruned: it holds %d mocks, the first pass read %d", v.place, v.firstPass)
	}
	return nil
}

// maxUnmetLogged caps how many of a test set's mocks with unmet requirements
// one warning names.
const maxUnmetLogged = 20

// unmetWarned holds the mock files whose unmet requirements were logged, by
// path, size and modification time: a replay reads a set's mocks more than
// once, and a long-lived process (the k8s-proxy) replays the same set again
// and again.
var unmetWarned sync.Map

// warnUnmetRequirements logs, once per version of the mock file at path, the
// mocks of a test set recorded against a mock the set does not hold
// (models.UnmetRequirement). Nothing in the set can stand in for what they
// lost, so the read cannot repair them: it says which they are and the two
// ways a set comes to lack them, before a test that runs one fails on it or
// is answered by another mock (the MySQL replayer refuses to serve another
// test's COM_STMT_EXECUTE in place of one whose statement the set lost, and
// names it).
func warnUnmetRequirements(logger *zap.Logger, testSetID, path string, unmet []models.UnmetRequirement) {
	if len(unmet) == 0 {
		return
	}
	if info, err := os.Stat(path); err == nil {
		key := fmt.Sprintf("%s\x00%d\x00%d", path, info.Size(), info.ModTime().UnixNano())
		if _, logged := unmetWarned.LoadOrStore(key, struct{}{}); logged {
			return
		}
	}
	var mocks, needs []string
	seen := map[string]struct{}{}
	for _, u := range unmet {
		if _, dup := seen[u.Mock]; dup {
			continue
		}
		seen[u.Mock] = struct{}{}
		if len(mocks) < maxUnmetLogged {
			mocks = append(mocks, u.Mock)
			needs = append(needs, u.Key)
		}
	}
	logger.Warn(fmt.Sprintf("%d mocks in %s were recorded against mocks the test set does not hold, so they may not replay as they were recorded; "+
		"the first is %s, which needs %s. Either an older keploy's mock prune (--remove-unused-mocks, or the prune after an auto-replay) deleted them, "+
		"judging each mock alone, or the test set never held them (a statement prepared in an earlier test set of the same recording, or a COM_STMT_PREPARE of 16 MiB or more, which the recorder leaves out)",
		len(seen), testSetID, unmet[0].Mock, unmet[0].Key),
		zap.String("testSetID", testSetID),
		zap.Int("count", len(seen)),
		zap.Strings("mocks", mocks),
		zap.Strings("need", needs),
		zap.Bool("truncated", len(seen) > len(mocks)),
		zap.String("next_step", "if the test set was pruned by a keploy older than this one, re-record it, or restore its mock file from a copy made before the prune"))
}
