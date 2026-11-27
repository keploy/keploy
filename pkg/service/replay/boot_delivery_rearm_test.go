package replay

import (
	"context"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// exactManagerProxy extends managerProxy with ExactServer, ExactWorkerServer,
// InFlightReader, and AcceptCounter so end-to-end RunTestSet tests exercise
// real MockManager staging under both KEPLOY_PROVEN="" and KEPLOY_PROVEN="1".
type exactManagerProxy struct {
	*managerProxy
	inFlightCh chan struct{}
}

func (p *exactManagerProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.mm.ServeExactTest(test, owned)
	return nil
}

func (p *exactManagerProxy) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	p.mm.OpenExactTest(test, owned, given)
	return nil
}

func (p *exactManagerProxy) ServeExactPool(key string, names []string) error {
	p.mm.ServeExactPool(key, names)
	return nil
}

func (p *exactManagerProxy) ExactTestOutcome() (string, []string, []string, bool) {
	return p.mm.ExactTestOutcome()
}

func (p *exactManagerProxy) ServeExactWorker(names []string) error {
	p.mm.ServeExactWorker(names)
	return nil
}

func (p *exactManagerProxy) InFlightChanged() <-chan struct{}               { return p.inFlightCh }
func (p *exactManagerProxy) InFlightPendingProgress(bool) map[uint64]uint64 { return nil }
func (p *exactManagerProxy) InFlightState(bool) (int, []string)             { return 0, nil }
func (p *exactManagerProxy) Accepted() (uint64, error)                      { return 0, nil }
func (p *exactManagerProxy) WaitAccepted(context.Context, uint64) error     { return nil }
func (p *exactManagerProxy) ListenDrops() (models.ListenDrops, error) {
	return models.ListenDrops{}, nil
}
func (p *exactManagerProxy) WaitPairedBytes(context.Context, uint64) error { return nil }
func (p *exactManagerProxy) PairedBytesProgress() uint64                   { return 0 }

type idleExactHooks struct {
	coreAgent.Hooks
}

func (idleExactHooks) MarkProxyListener(syscall.RawConn) error { return nil }
func (idleExactHooks) ProxyConnPair(syscall.RawConn) (coreAgent.ProxyConnPairing, error) {
	return coreAgent.ProxyConnPairing{}, nil
}
func (idleExactHooks) ProxyConnsEstablished(context.Context) (uint64, error) { return 0, nil }
func (idleExactHooks) AppConnsEstablished(context.Context) (uint64, error)   { return 0, nil }
func (idleExactHooks) AppConnsSettled() (uint64, error)                      { return 0, nil }
func (idleExactHooks) WaitAppConnsSettled(context.Context, uint64) error     { return nil }

var (
	_ coreAgent.ExactServer        = (*exactManagerProxy)(nil)
	_ coreAgent.ExactWorkerServer  = (*exactManagerProxy)(nil)
	_ coreAgent.InFlightReader     = (*exactManagerProxy)(nil)
	_ coreAgent.AcceptCounter      = (*exactManagerProxy)(nil)
	_ coreAgent.PairedBytesWaiter  = (*exactManagerProxy)(nil)
	_ coreAgent.ProxyConnsReporter = idleExactHooks{}
)

func newExactManagedAgent(t *testing.T, instr *prInstr) *managedAgent {
	t.Helper()
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	prevIncoming := coreAgent.ActiveIncomingProxy
	t.Cleanup(func() {
		coreAgent.RegisterIncomingProxy(prevIncoming)
		proxyPkg.SetConnectionIdleRetention(0)
	})
	cfg := &config.Config{}
	cfg.Test.StrictMockWindow = true
	proxy := &exactManagerProxy{
		managerProxy: &managerProxy{mm: mm},
		inFlightCh:   make(chan struct{}),
	}
	return &managedAgent{
		prInstr: instr,
		agent:   agentsvc.New(zap.NewNop(), idleExactHooks{}, proxy, nil, nil, cfg),
		mm:      mm,
		startup: map[time.Time][]string{},
	}
}

func replaySQSMock(name, target string, req, res time.Time, respBody string) *models.Mock {
	m := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":     "HTTP_CLIENT",
				"lifetime": models.MetadataLifetimePerTest,
			},
			HTTPReq: &models.HTTPReq{
				Method: models.Method("POST"),
				URL:    "/000000000000/localstack-queue/",
				Header: map[string]string{
					"Content-Type": "application/x-amz-json-1.0",
					"X-Amz-Target": target,
				},
				Body: `{"MaxNumberOfMessages":10,"QueueUrl":"http://sqs/q","WaitTimeSeconds":4}`,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/x-amz-json-1.0"},
				Body:       respBody,
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

func replayKafkaDeliveryMock(name string, req, res time.Time) *models.Mock {
	m := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.KAFKA,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			GenericRequests: []models.Payload{
				{Message: []models.OutputBinary{{Data: `{"header":{"apiKey":1,"apiVersion":11,"correlationId":1}}`}}},
			},
			GenericResponses: []models.Payload{
				{Message: []models.OutputBinary{{Data: `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"recordBatches":[{"records":[{"key":"k1","value":"v1"}]}]}]}]}}`}}},
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

func replayHTTPDepMock(name, urlPath string, req, res time.Time) *models.Mock {
	m := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":     "HTTP_CLIENT",
				"lifetime": models.MetadataLifetimePerTest,
			},
			HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: urlPath},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

// bootDeliveryMocksDB returns a fixed slice of per-test mocks for RunTestSet.
type bootDeliveryMocksDB struct {
	prMockDB
	mocks []*models.Mock
}

func (db *bootDeliveryMocksDB) GetFilteredMocks(_ context.Context, _ string, _, _ time.Time, _, _ map[string]bool) ([]*models.Mock, error) {
	out := make([]*models.Mock, 0, len(db.mocks))
	for _, m := range db.mocks {
		out = append(out, m.DeepCopy())
	}
	return out, nil
}

// bootConsumerAgent wraps managedAgent to simulate a background consumer
// taking message deliveries during startup (before the first test) and records
// every mock staged into filtered/startup/session when each test opens.
type bootConsumerAgent struct {
	*managedAgent
	bootConsumeNames []string
	bootDone         bool

	inspectMu    sync.Mutex
	lastErr      error
	bootConsumed map[string]bool
	stagedInT1   map[string]bool
	secondHit    map[string]bool
	bootCopies   map[string]models.Mock
	firstTest    time.Time
}

func (a *bootConsumerAgent) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	if err := a.managedAgent.UpdateMockParams(ctx, params); err != nil {
		a.inspectMu.Lock()
		a.lastErr = err
		a.inspectMu.Unlock()
		return err
	}
	isBootStage := params.AfterTime.Equal(models.BaseTime) || (params.Exact != nil && params.Exact.Stage)
	if isBootStage && !a.bootDone {
		a.bootDone = true
		byName := map[string]*models.Mock{}
		for _, fn := range []func() ([]*models.Mock, error){a.mm.GetStartupMocks, a.mm.GetSessionScopedMocks, a.mm.GetFilteredMocks} {
			list, err := fn()
			if err != nil {
				return err
			}
			for _, mk := range list {
				if mk != nil {
					if _, exists := byName[mk.Name]; !exists {
						byName[mk.Name] = mk
					}
				}
			}
		}
		a.bootCopies = make(map[string]models.Mock)
		consumed := map[string]bool{}
		for _, name := range a.bootConsumeNames {
			if mk := byName[name]; mk != nil {
				a.bootCopies[name] = *mk
				if a.mm.DeleteFilteredMock(*mk) {
					consumed[name] = true
				}
			}
		}
		a.inspectMu.Lock()
		a.bootConsumed = consumed
		a.inspectMu.Unlock()
		return nil
	}
	isTest1 := params.AfterTime.Equal(a.firstTest) || (params.Exact != nil && params.Exact.Test == "test-1")
	if isTest1 {
		flt, _ := a.mm.GetFilteredMocks()
		stp, _ := a.mm.GetStartupMocks()
		sess, _ := a.mm.GetSessionScopedMocks()
		staged := map[string]bool{}
		for _, list := range [][]*models.Mock{flt, stp, sess} {
			for _, mk := range list {
				if mk != nil {
					staged[mk.Name] = true
				}
			}
		}
		hits := map[string]bool{}
		for name, cp := range a.bootCopies {
			if a.mm.DeleteFilteredMock(cp) || a.mm.DeleteStartupMock(cp) {
				hits[name] = true
			}
		}
		a.inspectMu.Lock()
		a.stagedInT1 = staged
		a.secondHit = hits
		a.inspectMu.Unlock()
	}
	return nil
}

// liveDrainHooks drains GetConsumedMocks directly from the real MockManager.
type liveDrainHooks struct {
	prHooks
	drain func() []models.MockState
}

func (h liveDrainHooks) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return h.drain(), nil
}

// TestBootMessageDeliveryNeverReArmedOrServedInTestWindow is the end-to-end
// regression test for the sqs-localstack double-delivery bug: an SQS
// ReceiveMessage carrying Messages (and a Kafka Fetch carrying records)
// consumed by a background consumer before the first test must NEVER be
// re-armed or served again inside a test window — in both timestamp and
// mapping modes, and with both KEPLOY_PROVEN="" and KEPLOY_PROVEN="1" —
// while a readiness-gate mock (/health SELECT 1) IS still re-armed for its test.
func TestBootMessageDeliveryNeverReArmedOrServedInTestWindow(t *testing.T) {
	modes := []struct {
		name    string
		proven  string
		mapping bool
	}{
		{name: "switch-off/timestamp", proven: "", mapping: false},
		{name: "switch-off/mapping", proven: "", mapping: true},
		{name: "proven-unmarked/timestamp", proven: "1", mapping: false},
		{name: "proven-unmarked/mapping", proven: "1", mapping: true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode.proven)

			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.replayer.config.Test.StrictMockWindow = true
			t1Start := h.cases[0].HTTPReq.Timestamp

			mocks := []*models.Mock{
				// Recorded 500ms before test-1 (matches sqs-localstack mock-2).
				replaySQSMock("mock-2-sqs-delivery", "AmazonSQS.ReceiveMessage", attrAt(500), attrAt(510), `{"Messages":[{"MessageId":"m1","Body":"Test message 1"},{"MessageId":"m2","Body":"Test message 2"}]}`),
				// Zero-timestamp SQS delivery (matches unmarked mapping-mode test-run-3).
				replaySQSMock("mock-2b-sqs-zero-ts", "AmazonSQS.ReceiveMessage", time.Time{}, time.Time{}, `{"Messages":[{"MessageId":"m3","Body":"Zero-ts message"}]}`),
				// Kafka Fetch with records before test-1.
				replayKafkaDeliveryMock("mock-kafka-delivery", attrAt(550), attrAt(560)),
				// Empty SQS long-poll before test-1 (NOT a delivery).
				replaySQSMock("mock-9-sqs-empty", "AmazonSQS.ReceiveMessage", attrAt(600), attrAt(650), `{}`),
				// Readiness-gate dependency owned by test-1 (MUST be re-armed).
				replayHTTPDepMock("mock-health-select", "/db-health", attrAt(1020), attrAt(1030)),
				// Test-1's own mock.
				replayHTTPDepMock("mock-own-1", "/orders", attrAt(1050), attrAt(1060)),
			}
			h.replayer.mockDB = &bootDeliveryMocksDB{mocks: mocks}

			if mode.mapping {
				h.mappings.exists = true
				h.mappings.onDiskStartup = []models.MockEntry{
					{Name: "mock-2-sqs-delivery", Kind: string(models.HTTP)},
					{Name: "mock-2b-sqs-zero-ts", Kind: string(models.HTTP)},
					{Name: "mock-kafka-delivery", Kind: string(models.KAFKA)},
					{Name: "mock-9-sqs-empty", Kind: string(models.HTTP)},
				}
				h.mappings.onDisk = map[string][]models.MockEntry{
					"test-1": {
						{Name: "mock-health-select", Kind: string(models.HTTP)},
						{Name: "mock-own-1", Kind: string(models.HTTP)},
					},
					"test-2": {},
				}
			}

			baseAgent := newExactManagedAgent(t, h.instr)
			agent := &bootConsumerAgent{
				managedAgent: baseAgent,
				bootConsumeNames: []string{
					"mock-2-sqs-delivery",
					"mock-2b-sqs-zero-ts",
					"mock-kafka-delivery",
					"mock-9-sqs-empty",
					"mock-health-select",
				},
				firstTest: t1Start,
			}
			h.replayer.instrumentation = agent
			h.replayer.hookImpl = liveDrainHooks{drain: baseAgent.mm.GetConsumedMocks}

			_ = h.run(t)

			agent.inspectMu.Lock()
			lastErr := agent.lastErr
			bootConsumed := agent.bootConsumed
			staged := agent.stagedInT1
			secondHit := agent.secondHit
			agent.inspectMu.Unlock()

			if staged == nil {
				t.Fatalf("test-1 window was never staged on the agent (last UpdateMockParams err=%v)", lastErr)
			}
			for _, deliv := range []string{"mock-2-sqs-delivery", "mock-2b-sqs-zero-ts", "mock-kafka-delivery"} {
				if !bootConsumed[deliv] {
					t.Fatalf("precondition failed: delivery %q was not consumed once during boot (consumed=%v)", deliv, bootConsumed)
				}
				if staged[deliv] {
					t.Fatalf("boot-consumed delivery %q was re-armed and re-staged into test-1 window", deliv)
				}
				if secondHit[deliv] {
					t.Fatalf("boot-consumed delivery %q was served a second time inside test-1 window", deliv)
				}
			}
			if !staged["mock-health-select"] {
				t.Fatal("readiness-gate mock mock-health-select was not re-armed for test-1")
			}
			if (mode.proven != "1" || !mode.mapping) && !staged["mock-9-sqs-empty"] {
				t.Fatal("empty SQS poll mock-9-sqs-empty must not be treated as a single-use delivery")
			}
		})
	}
}

// TestBootMessageDeliveryDefenseInDepthWhenAgentOmitsCarryOver proves the
// replayer-side guard: even if an older agent returns a boot-consumed SQS or
// Kafka message delivery with CarryOver=false and Usage=Updated, replay refuses
// to re-arm it and sends Usage=Deleted in TotalConsumedMocks for test-1.
func TestBootMessageDeliveryDefenseInDepthWhenAgentOmitsCarryOver(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.StrictMockWindow = true
	t1Start := h.cases[0].HTTPReq.Timestamp

	mocks := []*models.Mock{
		replaySQSMock("mock-2-sqs-delivery", "AmazonSQS.ReceiveMessage", attrAt(500), attrAt(510), `{"Messages":[{"MessageId":"m1","Body":"Test message 1"}]}`),
		replayKafkaDeliveryMock("mock-kafka-delivery", attrAt(550), attrAt(560)),
		replaySQSMock("mock-9-sqs-empty", "AmazonSQS.ReceiveMessage", attrAt(600), attrAt(650), `{}`),
		replayHTTPDepMock("mock-health-select", "/db-health", attrAt(1020), attrAt(1030)),
		replayHTTPDepMock("mock-own-1", "/orders", attrAt(1050), attrAt(1060)),
	}
	h.replayer.mockDB = &bootDeliveryMocksDB{mocks: mocks}

	// Simulate an older agent that does NOT flag CarryOver or Deleted on boot
	// consumes from the startup tree.
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
		"": {
			{Name: "mock-2-sqs-delivery", Kind: models.HTTP, Usage: models.Updated, Lifetime: models.LifetimePerTest, CarryOver: false},
			{Name: "mock-kafka-delivery", Kind: models.KAFKA, Usage: models.Updated, Lifetime: models.LifetimePerTest, CarryOver: false},
			{Name: "mock-9-sqs-empty", Kind: models.HTTP, Usage: models.Updated, Lifetime: models.LifetimePerTest, CarryOver: false},
			{Name: "mock-health-select", Kind: models.HTTP, Usage: models.Updated, Lifetime: models.LifetimePerTest, CarryOver: false},
		},
	})}

	agent := newManagedAgent(t, h.instr)
	h.replayer.instrumentation = agent

	_ = h.run(t)

	// Verify test-1's startup tier does NOT contain either delivery (filtered
	// out by filterOutDeleted because replay forced Usage=Deleted), while
	// mock-9-sqs-empty remains available and mock-health-select is re-armed.
	t1Startup, ok := agent.startupAt(t1Start)
	if !ok {
		t.Fatal("test-1 was never staged on the agent")
	}
	for _, deliv := range []string{"mock-2-sqs-delivery", "mock-kafka-delivery"} {
		if slices.Contains(t1Startup, deliv) {
			t.Fatalf("defense-in-depth failed: delivery %q survived into test-1 startup tier: %v", deliv, t1Startup)
		}
	}
	if !slices.Contains(t1Startup, "mock-9-sqs-empty") {
		t.Fatalf("empty SQS poll mock-9-sqs-empty was wrongly stripped from test-1 startup tier: %v", t1Startup)
	}

	// Check TotalConsumedMocks sent on test-1's UpdateMockParams call directly.
	var t1Consumed map[string]models.MockState
	h.instr.mu.Lock()
	for _, call := range h.instr.allParams {
		if call.AfterTime.Equal(t1Start) {
			t1Consumed = call.TotalConsumedMocks
			break
		}
	}
	h.instr.mu.Unlock()
	if t1Consumed == nil {
		t.Fatal("no UpdateMockParams call recorded for test-1")
	}
	for _, deliv := range []string{"mock-2-sqs-delivery", "mock-kafka-delivery"} {
		if st, ok := t1Consumed[deliv]; !ok || st.Usage != models.Deleted {
			t.Fatalf("test-1 TotalConsumedMocks[%q] = %+v (ok=%v), want Usage=deleted", deliv, st, ok)
		}
	}
	if _, rearmed := t1Consumed["mock-health-select"]; rearmed {
		t.Fatalf("mock-health-select should have been re-armed (withheld from TotalConsumedMocks), got %+v", t1Consumed["mock-health-select"])
	}
}
