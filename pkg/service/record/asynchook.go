package record

import (
	"context"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type testWindow struct {
	id        string
	startedAt int64 // unix nanos (ingress request timestamp = window START)
}

// AsyncRecorder stamps async metadata on egress mocks that match a declared
// lane. Lane match is the SOLE discriminator; position only sets the anchor.
type AsyncRecorder struct {
	BaseRecordHooks
	logger  *zap.Logger
	lanes   []models.AsyncLane
	parsers map[string]async.AsyncParser

	mu    sync.Mutex
	tests []testWindow   // ingress windows, by start timestamp
	seq   map[string]int // per-lane counter
}

func NewAsyncRecorder(logger *zap.Logger, lanes []models.AsyncLane, parsers map[string]async.AsyncParser) *AsyncRecorder {
	// Fill in any omitted lane names so the stamped MetaAsyncLane value is the
	// same deterministic key the replay engine re-derives.
	return &AsyncRecorder{logger: logger, lanes: models.WithEffectiveNames(lanes), parsers: parsers, seq: map[string]int{}}
}

// AfterTestCaseInsert tracks each ingress testcase's window START. Uses the
// After hook because TestCase.Name is assigned by InsertTestCase — reading it
// in the Before hook would capture an empty id (anchorAfter). anchorPos is
// timestamp-derived so it is correct either way; the After hook makes the
// human-readable anchorAfter name correct too.
func (r *AsyncRecorder) AfterTestCaseInsert(_ context.Context, info *TestCaseContext) error {
	if info == nil || info.TestCase == nil {
		return nil
	}
	r.mu.Lock()
	r.tests = append(r.tests, testWindow{
		id:        info.TestCase.Name,
		startedAt: info.TestCase.HTTPReq.Timestamp.UnixNano(), // window START
	})
	r.mu.Unlock()
	return nil
}

// BeforeMockInsert stamps async metadata on any egress mock that matches a
// declared lane. MUST be the Before hook: mockDB.InsertMock persists the mock,
// so a mutation applied After would never reach the recorded YAML (mirrors the
// enterprise obfuscator, which also mutates in Before*).
func (r *AsyncRecorder) BeforeMockInsert(_ context.Context, info *MockContext) error {
	if info == nil || info.Mock == nil || len(r.lanes) == 0 {
		return nil
	}
	m := info.Mock
	for _, lane := range r.lanes {
		p := r.parsers[lane.Type]
		if p == nil {
			// Lane is declared but its parser never resolved. ResolveAsyncParsers
			// already reports that once at startup with a fix hint; this fires per
			// mock, so keep it at Debug to avoid flooding a real recording.
			r.logger.Debug("async lane parser unresolved; mock left unstamped — "+
				"ensure lane.type matches a registered integration implementing AsyncParser",
				zap.String("lane", lane.Name), zap.String("type", lane.Type))
			continue
		}
		if !p.MatchesLane(m, lane) {
			continue
		}
		r.mu.Lock()
		r.seq[lane.Name]++
		seq := r.seq[lane.Name]
		// Anchor by the async COMPLETION time (response timestamp).
		anchorID, anchorPos := r.anchorLocked(m.Spec.ResTimestampMock.UnixNano())
		r.mu.Unlock()

		if m.Spec.Metadata == nil {
			m.Spec.Metadata = map[string]string{}
		}
		m.Spec.Metadata[models.MetaAsync] = "true"
		m.Spec.Metadata[models.MetaAsyncLane] = lane.Name
		m.Spec.Metadata[models.MetaAnchorAfter] = anchorID
		m.Spec.Metadata[models.MetaAnchorPos] = strconv.Itoa(anchorPos)
		m.Spec.Metadata[models.MetaAsyncSeq] = strconv.Itoa(seq)
		return nil
	}
	return nil
}

// anchorLocked returns the "effective testcase" for an async completion at ts:
// the last testcase STARTED at or before ts (its effect arms only from the
// NEXT test, never retroactively altering that test). Returns
// (id-or-startup, 1-based index / count started). Order-independent. Caller holds mu.
func (r *AsyncRecorder) anchorLocked(ts int64) (string, int) {
	id, pos := models.AnchorStartup, 0
	var best int64
	var found bool
	for _, w := range r.tests {
		if w.startedAt <= ts {
			pos++
			if !found || w.startedAt >= best {
				best = w.startedAt
				id = w.id
				found = true
			}
		}
	}
	return id, pos
}

// ResolveAsyncParsers builds stateless parser instances from the global
// registry, keyed by lane.Type. Parsers must implement async.AsyncParser.
func ResolveAsyncParsers(logger *zap.Logger, lanes []models.AsyncLane) map[string]async.AsyncParser {
	out := map[string]async.AsyncParser{}
	for _, lane := range lanes {
		if _, done := out[lane.Type]; done {
			continue
		}
		reg := integrations.Registered[integrations.IntegrationType(lane.Type)]
		if reg == nil {
			logger.Error("async lane type not registered; its mocks will not be stamped async — "+
				"set async.lanes[].type to a registered integration name",
				zap.String("type", lane.Type))
			continue
		}
		if ap, ok := reg.Initializer(logger).(async.AsyncParser); ok {
			out[lane.Type] = ap
		} else {
			logger.Error("async lane integration does not implement async.AsyncParser; its mocks will not be stamped async — "+
				"pick a lane type whose integration supports async (currently: http), or add AsyncParser to it",
				zap.String("type", lane.Type))
		}
	}
	return out
}
