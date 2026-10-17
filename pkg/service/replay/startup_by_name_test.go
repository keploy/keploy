package replay

import (
	"bytes"
	"context"
	"encoding/gob"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// A replay that loads by name reaches only the mocks the set's mapping lists:
// a mapping-mode test loads its own and the startup section's, and a fetch of
// the mapped mocks alone (the cloud's) brings the agent nothing else. Nothing
// re-stages the rest by its recorded time there, so the app's startup traffic
// is served only if the startup section lists it.
//
// A boot consumer's fetches are per-test mocks recorded before the set's
// first test (as the Kafka recorder states its data mocks): one consumed
// before the first test (partitionInitialConsumed re-arms it), and one still
// being fetched while test-1 ran (the window filter maps it to no test). The
// mapping write lists both in the startup section, beside the reusable tier;
// test-1's /health SELECT, which the readiness gate consumed before the first
// test, stays test-1's.
//
// Then a second replay of the set, by name only: its store holds only the
// mocks the written mapping lists, and a real agent stages them on a real
// mock manager. The boot consumer is served both fetches while the app starts
// and in test-1.
func TestABootConsumerIsServedByNameFromTheStartupSection(t *testing.T) {
	boot := func(name string, ms int) *models.Mock {
		return &models.Mock{
			Version: "api.keploy.io/v1beta1", Name: name, Kind: models.KAFKA,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks"},
				ReqTimestampMock: attrAt(ms), ResTimestampMock: attrAt(ms + 1),
			},
		}
	}
	recording := []*models.Mock{
		boot("kafka-early", 400), boot("kafka-boot", 500), boot("kafka-late", 600),
		boot("own-1", 1050), boot("gate-select", 1060), boot("own-2", 2050),
	}
	consumed := func(name string, ms int) models.MockState {
		st := mongoAt(name, ms)
		st.Kind = models.KAFKA
		return st
	}
	// Boot fetches answered once test-1 had begun (each carries test-1's
	// data). A replay answers a fetch as soon as the app asks, so where one
	// is mapped follows the drain that caught it: test-1's own drain keeps
	// it in test-1's entry (kafka-o1-own); the set's setup drain
	// (kafka-o1-boot) and another test's (kafka-o1-late, caught in test-2's)
	// list it as startup, where a by-name replay serves it as early as this
	// one did.
	answeredInTest1 := func(name string, ms int) models.MockState {
		mk := boot(name, ms)
		mk.Spec.ResTimestampMock = attrAt(1050)
		recording = append(recording, mk)
		st := consumed(name, ms)
		st.ResTimestampMock = models.FormatMockTimestamp(attrAt(1050))
		return st
	}
	o1Boot, o1Own, o1Late := answeredInTest1("kafka-o1-boot", 700), answeredInTest1("kafka-o1-own", 800), answeredInTest1("kafka-o1-late", 750)

	// The writer: an auto-replay with --update-test-mapping.
	w := newPartialRunHarness(t, 2, 0)
	pinWindows(w)
	w.replayer.config.Test.UpdateTestMapping = true
	db := mappingFile(t, w, nil)
	w.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(w, map[string][]models.MockState{
		"":       {consumed("kafka-boot", 500), consumed("gate-select", 1060), o1Boot},
		"test-1": {consumed("own-1", 1050), consumed("kafka-late", 600), consumed("kafka-early", 400), o1Own},
		"test-2": {consumed("own-2", 2050), o1Late},
	})}
	if status := w.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("the writing replay: status %q, want PASSED", status)
	}
	section, err := db.GetStartup(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("reading the startup section back: %v", err)
	}
	startup := (&models.Mapping{Startup: section}).StartupMockNames()
	if !slices.Contains(startup, "kafka-boot") || !slices.Contains(startup, "kafka-late") {
		t.Fatalf("the startup section lists %v; want the boot consumer's kafka-boot and kafka-late", startup)
	}
	if slices.Contains(startup, "gate-select") {
		t.Fatalf("the startup section lists %v; gate-select is test-1's, consumed early by the readiness gate", startup)
	}
	written, _, err := db.Get(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("reading the mapping back: %v", err)
	}
	var test1 []string
	for _, e := range written["test-1"] {
		test1 = append(test1, e.Name)
	}
	if !slices.Equal(test1, []string{"own-1", "kafka-o1-own"}) {
		t.Fatalf("test-1's entry lists %v; want own-1 and kafka-o1-own, the boot fetch answered in its window that its own drain caught", test1)
	}
	var test2 []string
	for _, e := range written["test-2"] {
		test2 = append(test2, e.Name)
	}
	if !slices.Equal(test2, []string{"own-2"}) {
		t.Fatalf("test-2's entry lists %v; want own-2 only", test2)
	}
	// kafka-boot and kafka-o1-boot were drained before the first test,
	// kafka-late and kafka-early (recorded first) while test-1 ran,
	// kafka-o1-late while test-2 ran: the section is in recorded order
	// whichever drain caught each.
	if got := entryNames(section); !slices.Equal(got, []string{"kafka-early", "kafka-boot", "kafka-late", "kafka-o1-boot", "kafka-o1-late"}) {
		t.Fatalf("the startup section lists %v; want the boot consumer's fetches other drains caught, in recorded order", got)
	}

	// The reader: the same set, by name only.
	r := newPartialRunHarness(t, 2, 0)
	pinWindows(r)
	r.replayer.config.Path = w.replayer.config.Path
	// The strict mock window, as config.New seeds it: the agent's staging
	// reads its on-disk store as a replay's does.
	r.replayer.config.Test.StrictMockWindow = true
	mappingFile(t, r, nil)
	r.replayer.mockDB = &byNameMockDB{recording: recording}
	r.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(r, map[string][]models.MockState{
		"test-1": {consumed("own-1", 1050)},
		"test-2": {consumed("own-2", 2050)},
	})}
	agent := newManagedAgent(t, r.instr)
	r.replayer.instrumentation = agent
	if status := r.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("the by-name replay: status %q, want PASSED", status)
	}
	for _, at := range []struct {
		when  string
		after time.Time
	}{{"while the app starts", models.BaseTime}, {"in test-1", r.cases[0].HTTPReq.Timestamp}} {
		got, ok := agent.startupAt(at.after)
		if !ok {
			t.Fatalf("the agent was never staged %s", at.when)
		}
		// The store holds only what the mapping lists, so the boot
		// consumer's fetches reach the agent because the startup section
		// lists them, and each staging serves them from the startup tier.
		if !slices.Contains(got, "kafka-boot") || !slices.Contains(got, "kafka-late") {
			t.Fatalf("%s the startup tier holds %v; the boot consumer is not served kafka-boot and kafka-late", at.when, got)
		}
	}
}

func entryNames(entries []models.MockEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// A streaming test's mismatch compares what it consumed with its own entry.
// The app's startup traffic it consumed (a boot consumer still catching up:
// a fetch the startup section lists, or one requested and answered before the
// set's first test) is no other test's, so it is no divergence of its
// mapping: a wrong answer stays FAILED. A mock outside its entry that is not
// startup traffic still demotes it to OBSOLETE.
func TestAStreamingTestsStartupConsumptionIsNoMismatch(t *testing.T) {
	kafka := func(name string, ms int) models.MockState {
		st := mongoAt(name, ms)
		st.Kind = models.KAFKA
		return st
	}
	for _, c := range []struct {
		name  string
		extra models.MockState
		want  models.TestStatus
	}{
		{"a fetch the startup section lists", kafka("kafka-late", 600), models.TestStatusFailed},
		{"a fetch requested and answered before the first test", kafka("kafka-boot", 500), models.TestStatusFailed},
		{"another test's mock", kafka("own-1b", 1060), models.TestStatusObsolete},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.streaming("test-2")
			h.mappings.exists = true
			h.mappings.onDisk = map[string][]models.MockEntry{"test-1": mongoEntries("own-1", "own-1b"), "test-2": mongoEntries("own-2")}
			h.mappings.onDiskStartup = []models.MockEntry{{Name: "kafka-late", Kind: string(models.KAFKA)}}
			h.replayer.hookImpl = prHooks{
				wrongBody: map[string]bool{"test-2": true},
				consumedFor: consumedByWindow(h, map[string][]models.MockState{
					"test-1": {mongoAt("own-1", 1050), mongoAt("own-1b", 1060)},
					"test-2": {mongoAt("own-2", 2050), c.extra},
				}),
			}
			h.run(t)
			if got := statusOf(t, h, "test-2"); got != c.want {
				t.Fatalf("streaming test-2 is %s; want %s", got, c.want)
			}
		})
	}
}

// byNameMockDB is a store that holds, of the recording, only the mocks the
// set's mapping lists (mocksThatHaveMappings: its tests' and its startup
// section's), as a fetch of the mapped mocks does.
type byNameMockDB struct {
	prMockDB
	recording []*models.Mock
}

func (m *byNameMockDB) GetFilteredMocks(_ context.Context, _ string, _, _ time.Time, mapped, _ map[string]bool) ([]*models.Mock, error) {
	var out []*models.Mock
	for _, mk := range m.recording {
		if mapped[mk.Name] {
			out = append(out, mk.DeepCopy())
		}
	}
	return out, nil
}

// managedAgent is the harness's agent with a real keploy agent behind it: the
// mocks it is stored and every filter call reach the agent, which stages them
// on a real mock manager. The startup tier is read after every call.
type managedAgent struct {
	*prInstr
	agent *agentsvc.Agent
	mm    *proxyPkg.MockManager

	mu      sync.Mutex
	startup map[time.Time][]string
}

func newManagedAgent(t *testing.T, instr *prInstr) *managedAgent {
	t.Helper()
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	// agentsvc.New registers its incoming proxy and the pool's idle
	// retention globally: both are put back as they were.
	prevIncoming := coreAgent.ActiveIncomingProxy
	t.Cleanup(func() {
		coreAgent.RegisterIncomingProxy(prevIncoming)
		proxyPkg.SetConnectionIdleRetention(0)
	})
	// The strict mock window, as config.New seeds it: the agent keeps the
	// per-test mocks it is streamed in its on-disk store, as a replay's does.
	cfg := &config.Config{}
	cfg.Test.StrictMockWindow = true
	return &managedAgent{
		prInstr: instr,
		agent:   agentsvc.New(zap.NewNop(), nil, &managerProxy{mm: mm}, nil, nil, cfg),
		mm:      mm,
		startup: map[time.Time][]string{},
	}
}

// MockOutgoing starts the set's replay session, as the proxy's Mock does.
func (f *managedAgent) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	f.mm.ResetForReplaySession()
	if opts.StartsTestSet {
		f.mm.ArmSetFence()
	}
	return f.prInstr.MockOutgoing(ctx, opts)
}

// StoreMocks streams the mocks to the agent as the replay's client does
// (StoreMocksStream), so the agent keeps the per-test ones in its on-disk
// store and loads each staging from it.
func (f *managedAgent) StoreMocks(ctx context.Context, filtered, unfiltered []*models.Mock) error {
	if err := f.prInstr.StoreMocks(ctx, filtered, unfiltered); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	for _, m := range append(append([]*models.Mock(nil), filtered...), unfiltered...) {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return f.agent.StoreMocksStream(ctx, models.MockStreamHeader{FilteredCount: len(filtered), UnfilteredCount: len(unfiltered)}, gob.NewDecoder(&buf))
}

func (f *managedAgent) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	if err := f.prInstr.UpdateMockParams(ctx, params); err != nil {
		return err
	}
	if err := f.agent.UpdateMockParams(ctx, params); err != nil {
		return err
	}
	startup, err := f.mm.GetStartupMocks()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(startup))
	for _, mk := range startup {
		names = append(names, mk.Name)
	}
	f.mu.Lock()
	f.startup[params.AfterTime] = names
	f.mu.Unlock()
	return nil
}

// startupAt is the startup tier's names after the filter call whose window
// starts at after.
func (f *managedAgent) startupAt(after time.Time) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for at, names := range f.startup {
		if at.Equal(after) {
			return names, true
		}
	}
	return nil, false
}

// managerProxy is the windowed part of the agent's proxy, on a real mock
// manager. The rest of coreAgent.Proxy is not there: a call to it panics, so
// a staging that reaches for it shows.
type managerProxy struct {
	coreAgent.Proxy
	mm *proxyPkg.MockManager
}

// SetAuxiliaryHook is the one other Proxy method agentsvc.New calls, when a
// hook is registered (coreAgent.ProxyHook): there is none to keep.
func (p *managerProxy) SetAuxiliaryHook(coreAgent.AuxiliaryProxyHook) {}

func (p *managerProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(f, u, start, end)
	return nil
}
func (p *managerProxy) SeedStartupCutoff(t time.Time)              { p.mm.SeedStartupCutoff(t) }
func (p *managerProxy) SeedRecordedWindows(ws []models.TestWindow) { p.mm.SeedRecordedWindows(ws) }
func (p *managerProxy) FirstTestWindowStart() time.Time            { return p.mm.FirstTestWindowStart() }
func (p *managerProxy) GetPersistentConsumed() map[string]models.MockState {
	return p.mm.GetPersistentConsumed()
}
