package async

import (
	"sort"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type laneStream struct {
	lane   models.AsyncLane
	mocks  []*models.Mock // sorted by asyncSeq
	cursor int            // next unconsumed index
}

// ReportSnapshot is a point-in-time copy of the engine's verdict tallies.
type ReportSnapshot struct {
	Pass         int
	Flag         int
	NotExercised int
	Flags        []string
}

// Engine is transport-agnostic: it holds only *models.Mock, models.AsyncLane,
// and AsyncParser delegates. Safe for concurrent use.
type Engine struct {
	logger    *zap.Logger
	laneOrder []models.AsyncLane          // caller-declared order; first match wins in LaneFor
	lanes     map[string]models.AsyncLane // by name, for O(1) lookup
	parsers   map[string]AsyncParser      // by lane.Type

	mu        sync.Mutex
	loaded    bool                   // true once Load has partitioned+sorted streams; makes Load idempotent
	streams   map[string]*laneStream // by lane name
	completed int                    // number of testcases completed

	pass, flag int
	flags      []string

	logOnce sync.Once // LogReport fires at most once (multiple shutdown seams call it)
}

func NewEngine(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]AsyncParser) *Engine {
	lm := make(map[string]models.AsyncLane, len(lanes))
	order := make([]models.AsyncLane, 0, len(lanes))
	for _, l := range lanes {
		lm[l.Name] = l
		order = append(order, l)
	}
	return &Engine{
		logger:    logger,
		laneOrder: order,
		lanes:     lm,
		parsers:   parsers,
		streams:   make(map[string]*laneStream),
	}
}

// Load partitions async-tagged mocks into per-lane, seq-ordered streams.
// It is run-once: the first call partitions and sorts; subsequent calls are
// no-ops so a re-Load after Decide has advanced a cursor cannot re-serve an
// already-consumed mock.
func (e *Engine) Load(mocks []*models.Mock) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loaded {
		return
	}
	for _, m := range mocks {
		if m == nil || m.Spec.Metadata[models.MetaAsync] != "true" {
			continue
		}
		name := m.Spec.Metadata[models.MetaAsyncLane]
		lane, ok := e.lanes[name]
		if !ok {
			continue
		}
		s := e.streams[name]
		if s == nil {
			s = &laneStream{lane: lane}
			e.streams[name] = s
		}
		s.mocks = append(s.mocks, m)
	}
	for _, s := range e.streams {
		sort.SliceStable(s.mocks, func(i, j int) bool {
			return seqOf(s.mocks[i]) < seqOf(s.mocks[j])
		})
	}
	e.loaded = true
}

func (e *Engine) OnTestComplete() {
	e.mu.Lock()
	e.completed++
	e.mu.Unlock()
}

// RegisterParser attaches a parser for a lane type. Called only during
// Proxy.InitIntegrations (startup, before any serving).
func (e *Engine) RegisterParser(t string, p AsyncParser) {
	e.mu.Lock()
	if e.parsers == nil {
		e.parsers = map[string]AsyncParser{}
	}
	e.parsers[t] = p
	e.mu.Unlock()
}

// CompletedForTest exposes the completed-testcase counter for wiring tests.
func (e *Engine) CompletedForTest() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completed
}

// LaneFor returns the lane a live request routes to, by asking each lane's
// parser. Lanes are tried in caller-declared order (as passed to NewEngine)
// so the FIRST declared matching lane always wins, deterministically — Go
// map iteration order must not be relied upon here.
func (e *Engine) LaneFor(m *models.Mock) (models.AsyncLane, bool) {
	for _, lane := range e.laneOrder {
		p := e.parsers[lane.Type]
		if p != nil && p.MatchesLane(m, lane) {
			return lane, true
		}
	}
	return models.AsyncLane{}, false
}

// Decide returns the recorded mock to serve, or a keep-alive payload.
func (e *Engine) Decide(lane models.AsyncLane, live *models.Mock) (*models.Mock, []byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	p := e.parsers[lane.Type]
	s := e.streams[lane.Name]
	if s != nil && s.cursor < len(s.mocks) && e.isArmedLocked(s.mocks[s.cursor]) {
		recorded := s.mocks[s.cursor]
		s.cursor++
		if p != nil {
			if ok, detail := p.MatchRequestShape(live, recorded, lane); ok {
				e.pass++
			} else {
				e.flag++
				e.flags = append(e.flags, lane.Name+": "+detail)
			}
		} else if e.logger != nil {
			e.logger.Warn("async: no parser registered for lane type; serving recorded mock unverified",
				zap.String("lane", lane.Name), zap.String("type", lane.Type))
		}
		return recorded, nil, nil // serve recorded either way
	}
	if p == nil {
		return nil, nil, nil
	}
	ka, err := p.EmptyResponse(lane)
	return nil, ka, err
}

// isArmedLocked reports whether a mock's anchor has been reached. Caller holds mu.
func (e *Engine) isArmedLocked(m *models.Mock) bool {
	return e.completed >= anchorPosOf(m)
}

// Report snapshots verdict tallies, counting undrained armed mocks as not-exercised.
func (e *Engine) Report() ReportSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	ne := 0
	for _, s := range e.streams {
		for i := s.cursor; i < len(s.mocks); i++ {
			if e.isArmedLocked(s.mocks[i]) {
				ne++
			}
		}
	}
	out := ReportSnapshot{Pass: e.pass, Flag: e.flag, NotExercised: ne}
	out.Flags = append(out.Flags, e.flags...)
	return out
}

// LogReport emits the async verdict tally so it is visible in keploy's output
// at end of replay: served-and-shape-matched, shape-flags (served despite
// request drift), and armed-but-never-polled (not-exercised). Each shape flag
// is logged at Warn since drift is an anomaly, not expected-default state.
func (e *Engine) LogReport(logger *zap.Logger) {
	e.logOnce.Do(func() {
		s := e.Report()
		if s.Pass == 0 && s.Flag == 0 && s.NotExercised == 0 {
			return // no async activity (e.g. record mode) — stay quiet
		}
		logger.Info("async egress verdict",
			zap.Int("served", s.Pass),
			zap.Int("shape_flags", s.Flag),
			zap.Int("not_exercised", s.NotExercised))
		for _, f := range s.Flags {
			logger.Warn("async egress shape drift (served recorded response anyway)",
				zap.String("detail", f))
		}
	})
}

func seqOf(m *models.Mock) int       { return atoiOr(m.Spec.Metadata[models.MetaAsyncSeq], 0) }
func anchorPosOf(m *models.Mock) int { return atoiOr(m.Spec.Metadata[models.MetaAnchorPos], 0) }

func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
