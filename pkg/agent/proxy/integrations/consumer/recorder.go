package consumer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// defaultMaxCachedViews bounds the effect views one open unit may hold.
//
// The cache exists because a projected view cannot be recovered later: by the
// time a test's mock mapping resolves, the payloads have already been written
// to disk by an independent goroutine and the mapping itself carries only
// names and timestamps, no bytes. So the views have to be kept as the mocks
// pass through — and a bound is needed because a runaway producer would
// otherwise grow this without limit inside the recording process.
//
// Overflow REFUSES the unit (CONSUMER_EFFECT_CACHE_OVERFLOW) rather than
// dropping a view. A dropped view under-counts expectEffects, and an
// under-counted expectEffects turns a real over-production into a pass — the
// exact silent green this contract exists to remove.
const defaultMaxCachedViews = 256

// graceSampleSize bounds the latency sample the record-time grace is derived
// from.
const graceSampleSize = 512

// RecorderOptions configures a Recorder.
type RecorderOptions struct {
	// Logger receives the recorder's diagnostics. Nil is tolerated.
	Logger *zap.Logger

	// Clock is the time source. Nil means the production clock.
	Clock Clock

	// TestCases is where minted consumer test cases are pushed. It is the
	// same channel the HTTP ingress pushes onto, so consumer tests reach
	// persistence through the unchanged path.
	TestCases chan<- *models.TestCase

	// EnableMapping mirrors the recorder's mapping setting and is passed
	// straight through to the dedup queue.
	EnableMapping bool

	// MaxCachedViews overrides defaultMaxCachedViews. Zero means the
	// default.
	MaxCachedViews int
}

// Refusal is one named reason a consumer unit was not persisted, or a
// recording-level reason the whole recording cannot be trusted.
type Refusal struct {
	// Units are the unit ids the refusal applies to. A straddling effect
	// names TWO units, because a frame carrying records for both makes
	// both untrustworthy and silently dropping one is how a spurious
	// "extra" or a missing effect gets manufactured.
	Units []string
	// Tests are the test names those units had already been given. A unit
	// refused before it closed has a name but no file.
	Tests    []string
	Category models.FailureCategory
	Detail   string
}

func (r Refusal) String() string {
	return fmt.Sprintf("%s: %s (units %s)", r.Category, r.Detail, strings.Join(r.Units, ", "))
}

// RecorderStats is the reconciliation §3 R6 requires the recording to print.
//
// It is a COUNTED, SURFACED statistic rather than a debug line because the
// consumer path is more fragile than the HTTP one: units are buffered until
// their mapping resolves, a mapping that never correlates revokes its test,
// and a user who watched ten messages go by must be told when only eight
// files exist. "10 units observed, 8 persisted" is the number the agent loop
// reads; the test count alone cannot say it.
type RecorderStats struct {
	UnitsObserved  int
	UnitsPersisted int
	UnitsRefused   int
	// IdlePolls counts empty poll responses. They are not units; they are
	// what closes one.
	IdlePolls int
	// OrphanEffects counts effect records observed while no unit was open.
	// Non-zero means the worker produced outside every window, which the
	// replay side would have nothing to compare against.
	OrphanEffects int
	// UnitsUnattributed counts PERSISTED units whose only claim is the
	// sideEffects count — no view the parser attributed to the message, so
	// the test asserts nothing this build can check.
	//
	// IT IS NOT A PROBLEM(), AND IT IS NOT SILENT EITHER. Such a unit is a
	// faithful record of what happened; what is missing is attribution, which
	// no parser in this repository supplies (see onOther). The replay judge
	// refuses exactly these tests BY NAME — CONSUMER_NO_OBSERVABLE_EFFECT,
	// rule 7 — rather than passing them, so the user would otherwise meet the
	// consequence one CI run later with no idea it was decided at record
	// time. Close warns on it, naming the fix.
	UnitsUnattributed int
	Refusals          []Refusal
}

// UnitsLost is the units that were neither persisted nor refused by name.
// Anything here vanished silently, which is the one outcome §3 R6 forbids.
func (s RecorderStats) UnitsLost() int {
	lost := s.UnitsObserved - s.UnitsPersisted - s.UnitsRefused
	if lost < 0 {
		return 0
	}
	return lost
}

// Problems names every reason this recording must not be replayed as-is: a
// unit was refused by name, a unit was lost, or the worker produced outside
// every window. Empty means the recording reconciled.
func (s RecorderStats) Problems() []string {
	var problems []string
	if lost := s.UnitsLost(); lost > 0 {
		problems = append(problems, fmt.Sprintf("%s: %d of %d consumer units were neither persisted nor refused by name", models.CategoryConsumerUnitsLost, lost, s.UnitsObserved))
	}
	for _, ref := range s.Refusals {
		problems = append(problems, ref.String())
	}
	if s.OrphanEffects > 0 {
		problems = append(problems, fmt.Sprintf("%d effect records were produced while no consumer unit was open; they belong to no test and replay has nothing to compare them against", s.OrphanEffects))
	}
	return problems
}

// Err returns a non-nil error when the recording must not be trusted. It is
// what makes a degraded consumer recording FAIL rather than quietly produce a
// smaller suite than the user watched being made: the proxy publishes it as a
// models.ConsumerRecordingReport, the record command fetches that at teardown
// and sets a non-zero exit code from it.
func (s RecorderStats) Err() error {
	problems := s.Problems()
	if len(problems) == 0 {
		return nil
	}
	return errors.New("consumer recording is not trustworthy: " + strings.Join(problems, "; "))
}

// Report projects the reconciliation into the cross-process shape the record
// command reads. The counters are always filled in; Problems is what decides
// whether the recording failed.
func (s RecorderStats) Report() models.ConsumerRecordingReport {
	return models.ConsumerRecordingReport{
		UnitsObserved:  s.UnitsObserved,
		UnitsPersisted: s.UnitsPersisted,
		UnitsRefused:   s.UnitsRefused,
		UnitsLost:      s.UnitsLost(),
		OrphanEffects:  s.OrphanEffects,
		Problems:       s.Problems(),
	}
}

// unit is one open consumer unit: a delivery, and everything the worker did
// while handling it.
type unit struct {
	id       string
	testName string
	protocol string
	connID   string
	kind     models.Kind

	mgr *syncMock.SyncMockManager
	job *syncMock.DedupJob

	// start is the window's left edge: the trigger's RESPONSE time, i.e.
	// the instant the payload reached the application.
	start   time.Time
	trigger models.EffectView
	// triggerReqAt is kept only for diagnostics; the persisted window is
	// [start, end].
	triggerReqAt time.Time

	// orderBy is the Coords key the PROTOCOL RECORDER named as this
	// stream's lane discriminator (models.MetaKeyOrderBy on the trigger
	// mock), copied verbatim onto the minted spec. Empty means one lane per
	// (protocol, target) — the stricter reading, which is the right default
	// for a recording that did not claim its effects are independently
	// ordered.
	orderBy string

	effects       []models.EffectView
	effectRecords int
	lastEffectAt  time.Time

	// sideEffects counts mocks of a DIFFERENT protocol family than the
	// trigger seen while this unit was open — the consume-and-write-to-a-
	// database shape. Same-family untagged mocks are the consumer
	// protocol's own coordination traffic (heartbeats, offset commits,
	// metadata refreshes) and are not effects.
	sideEffects int

	refusal    models.FailureCategory
	refusalDet string
}

// Recorder turns a stream of role-tagged mocks into consumer units and mints
// one test case per unit.
//
// ONE OPEN UNIT AT A TIME, ACROSS THE WHOLE RECORDING. The unit boundary is
// defined by the trigger stream, but a unit's effects arrive on OTHER
// connections — a producer socket, a database socket — so attribution cannot
// be per-connection. Two concurrent trigger streams would therefore make every
// effect ambiguous. Rather than guess, a second trigger-bearing connection (a
// reconnect, a rebalance, a second consumer in one process) is refused by name
// with CONSUMER_MULTI_CONNECTION_RECORDING, which is also what keeps recorded
// broker session identity replayable.
//
// The zero value is not usable; construct it with NewRecorder.
type Recorder struct {
	logger   *zap.Logger
	clock    Clock
	tcChan   chan<- *models.TestCase
	mapping  bool
	maxViews int

	mu sync.Mutex

	open *unit
	// unitSeq numbers units for the `unit` mock metadata and for refusal
	// messages. It is in-memory identity only and never reaches disk.
	unitSeq int
	// prevUnitID/prevTestName name the unit that closed most recently, so
	// a straddling effect can refuse BOTH units by name.
	prevUnitID   string
	prevTestName string

	// triggerConn is the connection group the trigger stream arrived on,
	// per protocol. A second entry for one protocol is the
	// multi-connection refusal.
	triggerConn map[string]string
	multiConn   bool

	stats RecorderStats

	// latencies is a bounded sample of (last effect - unit start) used to
	// derive the completion grace at mint time.
	latencies []time.Duration
}

// NewRecorder returns a Recorder.
func NewRecorder(opts RecorderOptions) *Recorder {
	maxViews := opts.MaxCachedViews
	if maxViews <= 0 {
		maxViews = defaultMaxCachedViews
	}
	return &Recorder{
		logger:      opts.Logger,
		clock:       clockOrReal(opts.Clock),
		tcChan:      opts.TestCases,
		mapping:     opts.EnableMapping,
		maxViews:    maxViews,
		triggerConn: map[string]string{},
	}
}

// Stats returns a snapshot of the reconciliation counters.
func (r *Recorder) Stats() RecorderStats {
	if r == nil {
		return RecorderStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.stats
	out.Refusals = append([]Refusal(nil), r.stats.Refusals...)
	return out
}

// OnMock is the CONSUMER PARSER's ingest surface. A parser calls it for every
// ROLE-TAGGED mock it emits, in emit order, and BEFORE handing that mock to
// the syncMock manager. That ordering is load-bearing twice over: the first
// trigger is what sets the manager's first-request flag (nothing else does it
// for a headless worker, and a mock added before the flag is forwarded
// straight out instead of being buffered for windowing), and a trigger has to
// be tagged with its unit before the buffer can bin it.
//
// The recorder decides what a mock means from its role metadata:
//
//	role=trigger  a delivery. With records, it closes the previous unit and
//	              opens a new one; with none, it is an idle poll and only
//	              closes.
//	role=effect   something the worker produced inside the open unit.
//	absent        NOTHING HAPPENS HERE. An untagged mock is an ordinary mock,
//	              and it is counted — if it is counted at all — by OnEgress,
//	              which the syncMock manager runs for EVERY mock in the
//	              process. Counting it here as well would double-count exactly
//	              the mocks a cooperative parser announces, and would still
//	              miss every mock emitted by a parser that has never heard of
//	              this contract, which is all of them.
//
// It is harmless to call for an untagged mock, so "call it for every mock you
// emit" is still safe advice for a parser author; it simply has no effect.
//
// Nil-safe on the receiver so a parser can call it whether or not a recorder
// was installed on the context.
func (r *Recorder) OnMock(ctx context.Context, m *models.Mock) {
	if r == nil || m == nil {
		return
	}
	switch m.Spec.Metadata[models.MetaKeyRole] {
	case models.RoleTrigger:
		r.onTrigger(ctx, m)
	case models.RoleEffect:
		r.onEffect(m)
	}
}

// OnEgress is the CROSS-PARSER ingest surface: the syncMock manager calls it
// for every mock it keeps, whatever emitted it (see
// SyncMockManager.SetEgressObserver). It is what makes ConsumerSpec.SideEffects
// mean what its name says.
//
// WHY IT CANNOT BE THE PARSER'S JOB. The side-effect count is about calls of
// ANOTHER protocol family made while a unit is open — a Kafka worker's
// database INSERT, its outgoing HTTP call, its cache set. Those mocks are
// emitted by the postgres/http/generic parsers, which are not consumer-aware
// and never will be: making them call OnMock would put a consumer-contract
// obligation on every parser in the tree. The syncMock manager is the one
// place all of them meet.
//
// ROLE-TAGGED MOCKS ARE SKIPPED. The consumer parser announces those through
// OnMock before it hands them over, so counting them again here would
// double-count them — and onOther would discard them anyway, since a trigger
// and its effects are of the unit's OWN Kind.
//
// IT TAKES ONLY THE RECORDER'S OWN MUTEX and calls nothing back into the
// syncMock manager, which is what lets the manager run it under its lock. See
// the egressObserver field for that contract.
func (r *Recorder) OnEgress(m *models.Mock) {
	if r == nil || m == nil {
		return
	}
	switch m.Spec.Metadata[models.MetaKeyRole] {
	case models.RoleTrigger, models.RoleEffect:
		return
	}
	r.onOther(m)
}

// OnIdlePoll closes the open unit without opening a new one.
//
// It is a first-class event, not an optimisation. Setting the syncMock
// first-request flag (which the consumer unit must do, because nothing else
// does it for a headless worker) makes every mock buffer, and the buffer's
// safety valve reaps per-test mocks older than seven seconds. A consumer that
// idles for more than seven seconds between messages is the NORMAL case, so a
// unit left open across an idle stretch would straddle that horizon. Closing
// on an idle poll keeps every window short enough that it cannot.
func (r *Recorder) OnIdlePoll(ctx context.Context, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.IdlePolls++
	r.mu.Unlock()
	if open := r.takeOpen(nil); open != nil {
		r.closeUnit(ctx, open, r.orNow(at))
	}
}

// Close closes the last open unit at parser teardown and returns the
// reconciliation.
func (r *Recorder) Close(ctx context.Context) RecorderStats {
	if r == nil {
		return RecorderStats{}
	}
	if open := r.takeOpen(nil); open != nil {
		r.closeUnit(ctx, open, r.clock.Now())
	}
	stats := r.Stats()
	// SILENT WHEN THERE WAS NOTHING TO RECONCILE. A recorder is created for
	// every recording session, including the overwhelming majority that never
	// see a consumer protocol at all, and printing "0 units observed" at Info
	// on every HTTP recording would be pure noise on a path this contract is
	// meant not to touch.
	if r.logger != nil && (stats.UnitsObserved > 0 || len(stats.Refusals) > 0) {
		r.logger.Info("consumer recording reconciliation",
			zap.Int("units_observed", stats.UnitsObserved),
			zap.Int("test_cases_persisted", stats.UnitsPersisted),
			zap.Int("units_refused", stats.UnitsRefused),
			zap.Int("units_lost", stats.UnitsLost()),
			zap.Int("idle_polls", stats.IdlePolls),
			zap.Int("orphan_effect_records", stats.OrphanEffects),
			zap.Int("units_without_attributed_effects", stats.UnitsUnattributed),
		)
		// SAID AT RECORD TIME, BECAUSE IT IS DECIDED AT RECORD TIME. These
		// units are persisted and are honest records, but their only claim is
		// a count of calls nothing attributes to the message — the count
		// includes anything else the process did inside the same window —
		// which is why the judge refuses to grade a test that rests on it.
		// Without this line the user meets that refusal one CI run later.
		if stats.UnitsUnattributed > 0 {
			r.logger.Warn("some consumer test cases were recorded with no effect attributed to the message",
				zap.Int("test_cases", stats.UnitsUnattributed),
				zap.String("why", "their only claim is sideEffects — a count of calls of other protocols that fell inside the same window, which includes anything else the process did at the same time (a /health handler's database ping, a cron job, a metrics push). Nothing ties one connection's work to another's, so replay cannot tell that count apart from unrelated traffic"),
				zap.String("category", string(models.CategoryConsumerNoObservableEffect)),
				zap.String("next_step", "replay reports these tests FAILED with "+string(models.CategoryConsumerNoObservableEffect)+" rather than passing them; they become gradeable when the parser recording those calls tags them "+models.MetaKeyRole+"="+models.RoleEffect),
			)
		}
	}
	return stats
}

func (r *Recorder) onTrigger(ctx context.Context, m *models.Mock) {
	protocol := ProtocolOf(m)
	closeAt := r.triggerCloseTime(m)

	views, err := Project(r.logger, protocol, m)
	if err != nil {
		// A trigger we cannot describe cannot open a unit, and the unit
		// it would have closed still has to close: its window ends
		// here whatever we can say about the frame.
		r.closeOpen(ctx, closeAt)
		r.noteRefusal(Refusal{
			Units:    []string{fmt.Sprintf("u-%d(unopened)", r.nextSeqPeek())},
			Category: projectorRefusal(err),
			Detail:   "a trigger frame could not be projected, so no consumer unit could be opened for it: " + err.Error(),
		}, true)
		return
	}

	records := 0
	for _, v := range views {
		records += v.RecordCount()
	}
	if records == 0 || len(views) == 0 {
		// An empty poll response. Not a unit — the thing that ends one.
		r.OnIdlePoll(ctx, closeAt)
		return
	}

	trigger := views[0]
	if len(views) > 1 {
		// One poll response carrying several targets or partitions.
		// v1 records ONE test per poll response, so the extra views
		// cannot each become a trigger; the record count is summed so
		// the test is labelled honestly as a batch instead of quietly
		// describing a fraction of the frame.
		if r.logger != nil {
			r.logger.Warn("a consumer trigger frame projected to more than one view; recording it as a single batched unit",
				zap.String("protocol", protocol),
				zap.Int("views", len(views)),
				zap.Int("records", records),
				zap.String("next_step", "seed one message at a time (see the seed.sh convention) to keep one recorded test per message; a batched unit asserts the frame's records together"),
			)
		}
	}
	trigger.Records = records

	r.mu.Lock()
	// MULTI-CONNECTION DETECTION. Only trigger-bearing connections count:
	// a consumer legitimately holds several sockets (the group coordinator,
	// one per leader broker), and refusing on those would refuse every
	// real recording. What cannot be replayed coherently is a trigger
	// STREAM that moved.
	if prev, seen := r.triggerConn[protocol]; seen && prev != m.ConnectionID {
		r.multiConn = true
	} else if !seen {
		r.triggerConn[protocol] = m.ConnectionID
	}
	multi := r.multiConn
	r.unitSeq++
	seq := r.unitSeq
	r.stats.UnitsObserved++
	r.mu.Unlock()

	mgr := syncMock.FromContextOrGlobal(ctx)
	// POPULATE THE BUFFER. Nothing else does this for a headless worker:
	// the first-request flag has exactly two non-test callers in the tree
	// and both are HTTP ingress, so without this the syncMock buffer stays
	// empty, ResolveRange has nothing to bin, and mappings.yaml comes out
	// empty for the entire recording.
	if mgr != nil && !mgr.GetFirstReqSeen() {
		mgr.SetFirstRequestSignaled()
	}

	u := &unit{
		id:           fmt.Sprintf("u-%d", seq),
		protocol:     protocol,
		connID:       m.ConnectionID,
		kind:         m.Kind,
		mgr:          mgr,
		start:        closeAt,
		trigger:      trigger,
		triggerReqAt: m.Spec.ReqTimestampMock,
		// The lane discriminator is read off the TRIGGER mock's metadata
		// because that is the one frame a protocol recorder always stamps
		// per unit, and because OSS must never choose the key itself: see
		// models.MetaKeyOrderBy. Absent means the stricter one-lane-per-
		// (protocol, target) reading.
		orderBy: strings.TrimSpace(m.Spec.Metadata[models.MetaKeyOrderBy]),
	}
	if mgr != nil {
		// Mint the name UP FRONT so the window, the mapping entry and
		// the test case all carry the same identity, and open the
		// window through the DEDUP QUEUE rather than calling
		// ResolveRange directly. The queue is the real window
		// authority: it drains strictly from its FIFO head, so a
		// parser resolving ranges on its own produces windows that
		// interleave with queued HTTP jobs the moment the application
		// has any ingress at all — a single /health endpoint is
		// enough — and the failure mode is silent cross-attribution.
		u.testName = fmt.Sprintf("test-%d", mgr.NextTestID())
		u.job = mgr.DedupQueue().Enqueue(u.start)
	}
	// Bind the trigger to its unit BY IDENTITY. The unit's window starts at
	// this frame's response time, so the frame's own request time falls
	// outside it — a timestamp match would give the trigger to the previous
	// test, or to no test at all once the recording is past the startup
	// window. The unit tag is the in-flight half and the window resolver
	// strips it before the mock is written; the unit id is the durable half
	// a parser also stamps.
	if m.Spec.Metadata == nil {
		m.Spec.Metadata = map[string]string{}
	}
	m.Spec.Metadata[models.MetaKeyUnit] = u.id
	if u.testName != "" {
		m.Spec.Metadata[models.MetaKeyUnitTest] = u.testName
	}
	if multi {
		u.refusal = models.CategoryConsumerMultiConnectionRecording
		u.refusalDet = fmt.Sprintf("the trigger stream for protocol %q moved to connection %q; recorded broker session identity cannot be replayed across a reconnect", protocol, m.ConnectionID)
	}

	// THE NEW UNIT IS INSTALLED BEFORE THE PREVIOUS ONE IS CLOSED, and the
	// order is load-bearing rather than tidy. Closing a unit resolves its
	// dedup job — which walks the whole sync-mock buffer and forwards every
	// matched mock through a channel — and then BLOCKS handing the minted test
	// case to persistence. Effects arrive on a different connection and
	// therefore a different goroutine, so a worker whose handler is quick
	// enough to produce during that stretch would find no unit open, and a
	// single effect record attributed to no unit fails the whole recording as
	// an orphan. Swapping first means the window in which nothing is open is
	// exactly zero: the new unit adopts what the application is doing right
	// now, which is what it did anyway once the swap happened a few
	// milliseconds later.
	//
	// Nothing about the PREVIOUS unit's window moves as a result. Its window
	// still ends at this trigger's response time, its dedup job is still
	// enqueued ahead of this one (the queue drains strictly from its head, so
	// enqueuing this unit's job first cannot let it resolve early), and its
	// mocks are still binned by timestamp. An effect frame that began before
	// this unit opened is still caught by the straddle check on the effect
	// path, which refuses BOTH units by name rather than guessing.
	prev := r.takeOpen(u)
	if prev != nil {
		r.closeUnit(ctx, prev, r.orNow(closeAt))
	}
}

func (r *Recorder) onEffect(m *models.Mock) {
	protocol := ProtocolOf(m)
	views, err := Project(r.logger, protocol, m)

	r.mu.Lock()
	u := r.open
	if u == nil {
		records := 0
		for _, v := range views {
			records += v.RecordCount()
		}
		if records == 0 {
			records = 1
		}
		r.stats.OrphanEffects += records
		r.mu.Unlock()
		if r.logger != nil {
			r.logger.Warn("the worker produced while no consumer unit was open",
				zap.String("protocol", protocol),
				zap.Int("records", records),
				zap.String("next_step", "these records belong to no test, so replay has nothing to compare them against; they are counted in the recording reconciliation and fail it"),
			)
		}
		return
	}
	if err != nil {
		r.refuseUnitLocked(u, projectorRefusal(err), "an effect frame could not be projected: "+err.Error())
		r.mu.Unlock()
		return
	}
	// STRADDLE DETECTION. A produced frame whose request began BEFORE this
	// unit's window opened carries records the worker emitted while the
	// PREVIOUS unit was open — the producer accumulator batches by its own
	// linger/size rules, not by consumer unit. Splitting it per record is
	// post-v1; refusing both units by name is what v1 does instead of
	// silently dropping one side or reporting a spurious extra.
	if at := m.Spec.ReqTimestampMock; !at.IsZero() && at.Before(u.start) && r.prevUnitID != "" {
		detail := fmt.Sprintf("a produced frame that began at %s carries records from before unit %s opened at %s, so it straddles the boundary with unit %s", at.Format(time.RFC3339Nano), u.id, u.start.Format(time.RFC3339Nano), r.prevUnitID)
		r.refuseUnitLocked(u, models.CategoryConsumerEffectStraddlesUnit, detail)
		r.noteRefusalLocked(Refusal{
			Units:    []string{r.prevUnitID, u.id},
			Tests:    []string{r.prevTestName, u.testName},
			Category: models.CategoryConsumerEffectStraddlesUnit,
			Detail:   detail + " — unit " + r.prevUnitID + " has already been persisted, so this recording must be discarded and retaken",
		}, false)
		r.mu.Unlock()
		return
	}
	if len(u.effects)+len(views) > r.maxViews {
		r.refuseUnitLocked(u, models.CategoryConsumerEffectCacheOverflow,
			fmt.Sprintf("unit %s produced more than %d effect views; keeping them all would grow without bound inside the recording process, and dropping one would under-count the expected effects and turn a real over-production into a pass", u.id, r.maxViews))
		r.mu.Unlock()
		return
	}
	for _, v := range views {
		u.effects = append(u.effects, v)
		// A PRESENCE STAND-IN IS NOT COUNTED BY THE COMPLETION RULE.
		// effectRecords becomes ConsumerCompletion.ExpectEffects, and
		// the replay side that has to satisfy that number
		// (Gate.pendingLocked) excludes presence views from what it
		// counts, because nothing on the replay path calls
		// ObserveEffect for a database write. Counting them here and
		// not there would make the two ends disagree by construction:
		// every such test would burn its whole timeout and then fail
		// EFFECT_MISSING with a bogus count row, on a worker doing
		// exactly what it recorded. The view is still kept — it is
		// rendered, and it still extends the grace anchor — and its
		// claim is asserted by the sync path's deps[i] presence row.
		if v.IsPresenceOnly() {
			continue
		}
		u.effectRecords += v.RecordCount()
	}
	if len(views) > 0 {
		u.lastEffectAt = r.effectTime(m)
	}
	r.mu.Unlock()
}

// onOther counts a mock of a different protocol family than the trigger: the
// consume-and-write shape — an INSERT, an outgoing call, a cache set. It is
// not field-diffed in v1 (its presence claim is delivered by the dependency
// rows on the synchronous path) but it IS what stops a consume-to-a-database
// worker being refused at mint for having no observable effect.
//
// IT COUNTS WHAT THE MAPPING WILL COUNT, AND NOTHING MORE. Its input is
// SyncMockManager.AddMock's kept stream (see OnEgress), which is every mock
// the process records — a /health handler's database ping, a background cron
// INSERT, a metrics push — and which is also exactly the stream mappings.yaml
// is built from. Binning on the unit's own window is the same authority the
// test-mock mapping uses (design §0.6: the DedupQueue window is what decides
// which mocks belong to which test), so this count agrees with what will
// actually land in mappings.yaml for this test instead of being a larger,
// differently-derived number.
//
// WHAT IT STILL CANNOT DO, stated plainly because the count is load-bearing: a
// call made by an unrelated handler INSIDE the window is still counted, exactly
// as it is still mapped. Telling "the worker made this call while handling
// this message" apart from "this call happened at the same time" needs
// attribution the parser has and OSS does not — the mock carries a connection
// id and nothing that ties one connection's work to another's.
//
// SO THE COUNT CANNOT CARRY A VERDICT, AND NEITHER CAN THE MOCKS IT COUNTED.
// A test whose ONLY claim is this number is refused at replay unless its
// mapping holds a mock the recording ATTRIBUTED to the worker (role=effect).
// "A mapped mock of another protocol family" was tried as that evidence and is
// not evidence at all: mappings.yaml is built from this same window, so the
// ambient /health ping this counts is also the mock that would have vouched
// for it — the count vouching for itself. See
// replay.mappingCanCarryAnEffectClaim.
//
// The practical consequence in THIS build, where no parser tags what a worker
// produced: a unit kept alive only by this count is persisted and then refused
// by name at replay. RecorderStats.UnitsUnattributed counts those and Close
// warns about them at record time.
func (r *Recorder) onOther(m *models.Mock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.open
	if u == nil {
		return
	}
	if m.Kind == u.kind {
		return
	}
	// Outside the unit's window: this call began before the trigger that
	// opened the unit was answered, so it belongs to whatever was happening
	// before — the previous unit, or no unit at all. A zero timestamp is a
	// parser that does not record one; counting it is the pre-existing
	// behaviour and no worse than dropping it.
	if at := m.Spec.ReqTimestampMock; !at.IsZero() && at.Before(u.start) {
		return
	}
	u.sideEffects++
	if at := r.effectTime(m); at.After(u.lastEffectAt) {
		u.lastEffectAt = at
	}
}

// closeOpen closes whatever unit is open, if any.
func (r *Recorder) closeOpen(ctx context.Context, at time.Time) {
	if open := r.takeOpen(nil); open != nil {
		r.closeUnit(ctx, open, r.orNow(at))
	}
}

// takeOpen installs next as the open unit — nil to leave none open — and
// returns whatever was open before, recording its identity as the previous
// unit in the SAME critical section.
//
// ONE LOCK, NOT TWO, AND THAT IS THE WHOLE POINT. A unit's effects arrive on a
// DIFFERENT connection from its trigger (a producer socket, a database
// socket), so onEffect runs on a different parser goroutine and genuinely
// races the open/close of a unit. Any instant in which r.open is nil while the
// application is still working attributes that work to no unit at all:
// onEffect takes its orphan branch, and a single orphan record fails the
// entire recording. Closing a unit is not cheap — it resolves the dedup job
// (which forwards every matched mock through a channel) and then blocks
// handing the minted test case to persistence — so a nil window held across it
// is milliseconds wide, per unit, on exactly the path a fast worker is
// producing into.
//
// The previous unit's identity moves here rather than living in closeUnit for
// the same reason: it is read by the straddle check on the effect path, and
// leaving it a step behind the swap would name the wrong unit in a refusal.
func (r *Recorder) takeOpen(next *unit) *unit {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.open
	if prev != nil {
		r.prevUnitID, r.prevTestName = prev.id, prev.testName
	}
	r.open = next
	return prev
}

// closeUnit resolves the unit's window and mints its test case.
//
// THE WINDOW IS ALWAYS RESOLVED, even for a refused unit. The dedup queue
// drains strictly from its head, so a job left unresolved wedges every job
// behind it for the rest of the recording — including the HTTP ingress jobs of
// a worker that also serves a health endpoint. A refusal must cost one test
// case, never the whole recording's mock attribution.
//
// ORDER MATTERS: the window resolves FIRST, because resolving is what emits
// the test-mock mapping, and the mapping consumer resolves mock names against
// mocks that must already be on their way to disk. Only then is the test case
// pushed.
func (r *Recorder) closeUnit(ctx context.Context, u *unit, end time.Time) {
	if !end.After(u.start) {
		// A zero-length window bins nothing. Give it the smallest
		// positive extent rather than silently attributing the unit's
		// mocks to no window at all.
		end = u.start.Add(time.Nanosecond)
	}
	if u.mgr != nil && u.job != nil {
		u.mgr.DedupQueue().ResolveJob(u.job, false, end, u.testName, r.mapping, u.mgr)
	}

	r.mu.Lock()
	refusal, detail := u.refusal, u.refusalDet
	if refusal == "" && len(u.effects) == 0 && u.sideEffects == 0 {
		// A unit with nothing observable can only ever pass. Recording
		// it would manufacture a vacuous green, which is worse than
		// having no test at all.
		//
		// THE TEST IS len(u.effects), NOT u.effectRecords. A projected
		// view whose confidence is `presence` contributes nothing to
		// effectRecords (see onEffect) but is still something the
		// worker demonstrably did, and its claim is asserted by the
		// sync path's deps[i] presence row. Refusing such a unit would
		// refuse exactly the projector shape design §2 describes for a
		// database write.
		refusal = models.CategoryConsumerNoObservableEffect
		detail = fmt.Sprintf("unit %s produced no effects and made no calls of any other kind while handling its message, so a test made from it could only ever pass", u.id)
	}
	if refusal != "" {
		r.stats.UnitsRefused++
		r.noteRefusalLocked(Refusal{
			Units:    []string{u.id},
			Tests:    []string{u.testName},
			Category: refusal,
			Detail:   detail,
		}, false)
		r.mu.Unlock()
		return
	}
	grace := r.deriveGraceLocked(u)
	r.stats.UnitsPersisted++
	if len(u.effects) == 0 {
		// Kept alive by the side-effect count alone: nothing here is
		// attributed to the message, so the replay judge will refuse it by
		// name rather than pass it. Counted so Close can say so once, at the
		// end of the recording, instead of leaving it to be discovered a CI
		// run later. See RecorderStats.UnitsUnattributed.
		r.stats.UnitsUnattributed++
	}
	r.mu.Unlock()

	tc := r.mint(u, end, grace)
	if r.tcChan == nil {
		return
	}
	// A NON-BLOCKING SEND FIRST, AND IT IS NOT AN OPTIMISATION. The blocking
	// select below has two ready cases whenever ctx is ALREADY cancelled and
	// the channel has room, and Go picks between ready cases uniformly at
	// random — so a hand-off that would have succeeded is refused as
	// CONSUMER_UNITS_LOST about half the time. Measured: the regression test
	// for the detached wind-down context failed 19 times in 40 with the coin
	// flip in place. onTrigger -> closeOpen -> closeUnit runs on the LIVE
	// parser context during normal recording, so a Ctrl-C landing while a unit
	// closes made the reconciliation non-deterministic — and
	// CONSUMER_UNITS_LOST is precisely the category design §3 R6 says must
	// never fire by accident. Trying the send on its own first removes the
	// race entirely: cancellation only decides what happens when the channel
	// genuinely cannot take the unit.
	select {
	case r.tcChan <- tc:
		return
	default:
	}
	select {
	case r.tcChan <- tc:
	case <-ctx.Done():
		// COUNTED AS REFUSED, NOT AS LOST. This unit did not vanish —
		// we know exactly what happened to it and we are about to say
		// so by name. Leaving it out of UnitsRefused would make
		// UnitsLost() count it a SECOND time, and the reconciliation
		// would then print "1 of 1 units were neither persisted nor
		// refused by name" directly above the by-name refusal for that
		// very unit. UnitsLost() is the safety net for a unit nothing
		// can account for; anything with a name belongs to the named
		// tally.
		r.mu.Lock()
		r.stats.UnitsPersisted--
		r.stats.UnitsRefused++
		r.noteRefusalLocked(Refusal{
			Units:    []string{u.id},
			Tests:    []string{u.testName},
			Category: models.CategoryConsumerUnitsLost,
			Detail:   "the recording was cancelled before this unit's test case could be handed to persistence",
		}, false)
		r.mu.Unlock()
	}
}

// mint builds the test case for a closed unit.
func (r *Recorder) mint(u *unit, end time.Time, grace time.Duration) *models.TestCase {
	spec := &models.ConsumerSpec{
		Protocol: u.protocol,
		Trigger:  u.trigger,
		Effects:  append([]models.EffectView(nil), u.effects...),
		OrderBy:  u.orderBy,
		// SIDE EFFECTS ARE A COUNT, AND THE JUDGE NEEDS THEM. closeUnit
		// deliberately lets a unit through that produced nothing but made
		// calls of another protocol family — the consume-and-write-to-a-
		// database shape, one of the two most common. Without this count on
		// the file, such a test is indistinguishable from a unit in which
		// nothing at all happened, and the judge's vacuity guard (which
		// refuses a test that can only ever pass) would refuse every one of
		// them: recorded cleanly, then red 100% of the time at replay. The
		// calls themselves are asserted by the sync path's deps[i] presence
		// rows, not here.
		SideEffects: u.sideEffects,
		Completion: models.ConsumerCompletion{
			// EXPECT WHAT THE GATE CAN OBSERVE, WHICH IS PRODUCED
			// RECORDS. Design §5 says this counts produced records
			// PLUS mapped write-mocks; it cannot. Nothing in the
			// replay path calls ObserveEffect for a database write
			// — only a protocol parser with a projector does — so
			// counting writes here would make every
			// consume-and-write-to-a-database worker wait out its
			// whole timeout and fail, and that is one of the two
			// most common consumer shapes. The write's claim is not
			// lost: an unconsumed mapped write-mock is already
			// reported by the dependency rows on the synchronous
			// path, and a consumer verdict is non-demotable, so it
			// fails the test there. Records, not requests, because
			// batching makes requests-per-record nondeterministic
			// between runs and a request-count rule would flake by
			// construction.
			ExpectEffects: u.effectRecords,
			GraceMs:       grace.Milliseconds(),
			TimeoutMs:     models.ConsumerDefaultTimeoutMs,
		},
		// The persisted window is [delivery, unit close] — the same
		// window the dedup queue just resolved, so regenerating the
		// mapping from the test files reproduces the mapping the
		// recording made. Cutting at the trigger's RESPONSE rather
		// than its request is what keeps the NEXT trigger out of this
		// window: every mainstream client issues the following poll
		// from inside the current one, before the application has
		// processed the record it is holding.
		ReqTimestampMock: u.start,
		ResTimestampMock: end,
	}
	return &models.TestCase{
		Version:      models.GetVersion(),
		Kind:         models.CONSUMER,
		Name:         u.testName,
		Created:      u.start.Unix(),
		ConsumerSpec: spec,
		Noise:        map[string][]string{},
	}
}

// deriveGraceLocked derives the completion grace from the recording itself:
// p99 of (trigger -> last effect) times three, clamped. Caller holds r.mu.
//
// Derived rather than fixed because the right drain is a property of the
// worker, not of keploy: a 20ms in-memory transform and a 400ms enrichment
// call need different windows, and a grace that is too short reports the
// worker's own latency as a missing effect.
func (r *Recorder) deriveGraceLocked(u *unit) time.Duration {
	if !u.lastEffectAt.IsZero() && u.lastEffectAt.After(u.start) {
		r.latencies = append(r.latencies, u.lastEffectAt.Sub(u.start))
		if len(r.latencies) > graceSampleSize {
			r.latencies = r.latencies[len(r.latencies)-graceSampleSize:]
		}
	}
	if len(r.latencies) == 0 {
		return time.Duration(models.ConsumerGraceMinMs) * time.Millisecond
	}
	sorted := append([]time.Duration(nil), r.latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted)*99 + 99) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	ms := (sorted[idx] * 3).Milliseconds()
	if ms < models.ConsumerGraceMinMs {
		ms = models.ConsumerGraceMinMs
	}
	if ms > models.ConsumerGraceMaxMs {
		ms = models.ConsumerGraceMaxMs
	}
	return time.Duration(ms) * time.Millisecond
}

// refuseUnitLocked marks an open unit refused. The first refusal wins: it is
// the one closest to the cause. Caller holds r.mu.
func (r *Recorder) refuseUnitLocked(u *unit, category models.FailureCategory, detail string) {
	if u.refusal != "" {
		return
	}
	u.refusal, u.refusalDet = category, detail
}

func (r *Recorder) noteRefusal(ref Refusal, countUnit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteRefusalLocked(ref, countUnit)
}

func (r *Recorder) noteRefusalLocked(ref Refusal, countUnit bool) {
	if countUnit {
		r.stats.UnitsObserved++
		r.stats.UnitsRefused++
	}
	r.stats.Refusals = append(r.stats.Refusals, ref)
	if r.logger != nil {
		r.logger.Error("a consumer unit was refused",
			zap.Strings("units", ref.Units),
			zap.Strings("tests", ref.Tests),
			zap.String("category", string(ref.Category)),
			zap.String("detail", ref.Detail),
			zap.String("next_step", "this recording fails; fix the named cause and re-record rather than replaying a suite with a unit missing"),
		)
	}
}

// triggerCloseTime is the instant a trigger frame reached the application: its
// response time, falling back to its request time and then to now.
func (r *Recorder) triggerCloseTime(m *models.Mock) time.Time {
	if !m.Spec.ResTimestampMock.IsZero() {
		return m.Spec.ResTimestampMock
	}
	if !m.Spec.ReqTimestampMock.IsZero() {
		return m.Spec.ReqTimestampMock
	}
	return r.clock.Now()
}

// effectTime is the instant an effect frame was made.
func (r *Recorder) effectTime(m *models.Mock) time.Time {
	if !m.Spec.ReqTimestampMock.IsZero() {
		return m.Spec.ReqTimestampMock
	}
	if !m.Spec.ResTimestampMock.IsZero() {
		return m.Spec.ResTimestampMock
	}
	return r.clock.Now()
}

func (r *Recorder) orNow(t time.Time) time.Time {
	if t.IsZero() {
		return r.clock.Now()
	}
	return t
}

func (r *Recorder) nextSeqPeek() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unitSeq + 1
}

// projectorRefusal names the category for a projection failure.
//
// A projector that RETURNS an error has declined to model the payload, which
// is the refuse-don't-guess contract working as intended — that is an
// unsupported wire version. A projector that PANICS, or a protocol with no
// projector registered at all, is a keploy defect rather than a wire-version
// limitation, and saying "unsupported wire version" for it would send whoever
// reads the recording to look for the wrong thing.
func projectorRefusal(err error) models.FailureCategory {
	var noProj *ErrNoProjector
	var panicked *ErrProjectorPanic
	if errors.As(err, &noProj) || errors.As(err, &panicked) {
		return models.CategoryConsumerProjectorFailed
	}
	return models.CategoryConsumerUnsupportedWireVersion
}
