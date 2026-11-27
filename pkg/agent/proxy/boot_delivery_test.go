package proxy

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func bootSQSJSONDelivery(name string, req, res time.Time, body string) *models.Mock {
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
					"X-Amz-Target": "AmazonSQS.ReceiveMessage",
				},
				Body: `{"MaxNumberOfMessages":10,"QueueUrl":"http://sqs/q","WaitTimeSeconds":4}`,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/x-amz-json-1.0"},
				Body:       body,
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

func bootSQSXMLDelivery(name string, req, res time.Time) *models.Mock {
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
				URL:    "/",
				Header: map[string]string{
					"Content-Type": "application/x-www-form-urlencoded; charset=utf-8",
				},
				Body: "Action=ReceiveMessage&MaxNumberOfMessages=10&WaitTimeSeconds=4",
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "text/xml"},
				Body:       `<?xml version='1.0' encoding='utf-8'?><ReceiveMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/"><ReceiveMessageResult><Message><MessageId>xml-1</MessageId><Body>Test message 2</Body></Message></ReceiveMessageResult></ReceiveMessageResponse>`,
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

func bootKafkaFetchDelivery(name string, req, res time.Time) *models.Mock {
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
				{Message: []models.OutputBinary{{Data: `{"body":{"errorCode":0,"topics":[{"name":"events","partitions":[{"partitionIndex":0,"errorCode":0,"recordBatches":[{"records":[{"key":"k","value":"Test message 1"}]}]}]}]}}`}}},
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

func bootPlainHTTPMock(name, urlPath string, req, res time.Time) *models.Mock {
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
				Method: models.Method("GET"),
				URL:    urlPath,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Body:       `{"ok":true}`,
			},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true},
	}
	m.DeriveLifetime()
	return m
}

func poolNames(mocks []*models.Mock) map[string]bool {
	out := make(map[string]bool, len(mocks))
	for _, m := range mocks {
		if m != nil {
			out[m.Name] = true
		}
	}
	return out
}

// TestBootMessageDeliveryFlaggedCarryOverAndNeverReStaged proves that an SQS
// ReceiveMessage carrying Messages (JSON or Query XML) or a Kafka Fetch with
// records consumed before the first test is flagged Deleted+CarryOver by the
// agent and is NEVER re-staged or served again inside any test window — while
// empty SQS polls and readiness-gate mocks keep their existing behavior.
func TestBootMessageDeliveryFlaggedCarryOverAndNeverReStaged(t *testing.T) {
	base := time.Date(2026, 11, 27, 17, 11, 12, 0, time.UTC)
	t1Start := base.Add(5 * time.Second)
	t1End := t1Start.Add(200 * time.Millisecond)

	makeSet := func() []*models.Mock {
		return []*models.Mock{
			bootSQSJSONDelivery("sqs-delivery-json", base, base.Add(10*time.Millisecond), `{"Messages":[{"MessageId":"1","Body":"Test message 1"}]}`),
			// Zero-timestamp SQS XML delivery mirrors unmarked mapping-mode recordings.
			bootSQSXMLDelivery("sqs-delivery-xml", time.Time{}, time.Time{}),
			bootKafkaFetchDelivery("kafka-delivery", base.Add(20*time.Millisecond), base.Add(30*time.Millisecond)),
			bootSQSJSONDelivery("sqs-empty-poll", base.Add(40*time.Millisecond), base.Add(4*time.Second), `{}`),
			bootPlainHTTPMock("health-probe-dep", "/db-ping", t1Start.Add(5*time.Millisecond), t1Start.Add(10*time.Millisecond)),
			bootPlainHTTPMock("test1-own", "/items", t1Start.Add(20*time.Millisecond), t1Start.Add(30*time.Millisecond)),
		}
	}

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(t1Start)

	initial := makeSet()
	mm.SetMocksWithWindow(initial, nil, models.BaseTime, time.Now())

	// Consume deliveries, empty poll, and readiness-gate mock during boot.
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	byName := make(map[string]*models.Mock, len(startup))
	for _, mk := range startup {
		byName[mk.Name] = mk
	}
	for _, name := range []string{"sqs-delivery-json", "sqs-delivery-xml", "kafka-delivery", "sqs-empty-poll", "health-probe-dep"} {
		mk := byName[name]
		if mk == nil {
			t.Fatalf("missing %q in initial startup tree", name)
		}
		if !mm.DeleteFilteredMock(*mk) {
			t.Fatalf("boot DeleteFilteredMock(%q) returned false", name)
		}
	}

	consumed := mm.GetConsumedMocks()
	states := make(map[string]models.MockState, len(consumed))
	for _, st := range consumed {
		states[st.Name] = st
	}

	for _, name := range []string{"sqs-delivery-json", "sqs-delivery-xml", "kafka-delivery"} {
		st, ok := states[name]
		if !ok {
			t.Fatalf("boot delivery %q missing from GetConsumedMocks", name)
		}
		if st.Usage != models.Deleted || !st.CarryOver {
			t.Fatalf("boot delivery %q: got Usage=%q CarryOver=%v, want Usage=deleted CarryOver=true", name, st.Usage, st.CarryOver)
		}
	}
	for _, name := range []string{"sqs-empty-poll", "health-probe-dep"} {
		st, ok := states[name]
		if !ok {
			t.Fatalf("non-delivery %q missing from GetConsumedMocks", name)
		}
		if st.CarryOver || st.Usage != models.Updated {
			t.Fatalf("non-delivery %q: got Usage=%q CarryOver=%v, want Usage=updated CarryOver=false", name, st.Usage, st.CarryOver)
		}
	}

	// Stage test-1 even with the full mock list (simulating pre-drain or
	// mapping-mode re-staging of startup mocks) — deliveries consumed at boot
	// must NEVER be staged into filtered, startup, or session pools.
	test1Mocks := makeSet()
	mm.SetMocksWithWindow(test1Mocks, nil, t1Start, t1End)

	flt, _ := mm.GetFilteredMocks()
	stp, _ := mm.GetStartupMocks()
	sess, _ := mm.GetSessionScopedMocks()
	for poolName, names := range map[string]map[string]bool{
		"filtered": poolNames(flt),
		"startup":  poolNames(stp),
		"session":  poolNames(sess),
	} {
		for _, deliv := range []string{"sqs-delivery-json", "sqs-delivery-xml", "kafka-delivery"} {
			if names[deliv] {
				t.Fatalf("boot-consumed delivery %q was re-staged into %s pool for test-1", deliv, poolName)
			}
		}
	}
	if !poolNames(flt)["health-probe-dep"] {
		t.Fatal("readiness-gate mock health-probe-dep must remain staged in test-1 filtered pool")
	}

	// Even three-tier explicitStartup staging must strip boot-consumed deliveries.
	explicit := []*models.Mock{
		bootSQSJSONDelivery("sqs-delivery-json", base, base.Add(10*time.Millisecond), `{"Messages":[{"MessageId":"1","Body":"Test message 1"}]}`),
	}
	mm.SetMocksWithWindowThreeTier(makeSet(), nil, explicit, t1Start, t1End)
	stp, _ = mm.GetStartupMocks()
	if poolNames(stp)["sqs-delivery-json"] {
		t.Fatal("boot-consumed delivery sqs-delivery-json was re-staged via explicitStartup")
	}

	// Re-consuming any boot-consumed delivery copy in test-1 must return false.
	for _, deliv := range []string{"sqs-delivery-json", "sqs-delivery-xml", "kafka-delivery"} {
		if mm.DeleteFilteredMock(*byName[deliv]) || mm.DeleteStartupMock(*byName[deliv]) {
			t.Fatalf("boot-consumed delivery %q was consumed a second time in test-1", deliv)
		}
	}
}

func TestBootMessageDelivery_ConcurrentSingleConsume(t *testing.T) {
	base := time.Date(2026, 11, 27, 17, 11, 12, 0, time.UTC)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SetMocksWithWindow([]*models.Mock{
		bootSQSJSONDelivery("sqs-race", base, base.Add(5*time.Millisecond), `{"Messages":[{"Body":"only-once"}]}`),
	}, nil, models.BaseTime, time.Now())

	startup, _ := mm.GetStartupMocks()
	if len(startup) != 1 {
		t.Fatalf("want 1 startup mock, got %d", len(startup))
	}
	target := *startup[0]

	const workers = 16
	var wins atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if mm.DeleteFilteredMock(target) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("concurrent boot consumes succeeded %d times, want exactly 1", got)
	}
}

func TestBootMessageDelivery_SpilledResponseThroughDiskMocks(t *testing.T) {
	base := time.Date(2026, 11, 27, 17, 11, 12, 0, time.UTC)
	t1Start := base.Add(5 * time.Second)
	t1End := t1Start.Add(200 * time.Millisecond)

	// Exceed responseSpillMinBytes (8 KiB) so DiskMocks elides Spec.HTTPResp.
	bigBody := `{"Messages":[{"MessageId":"big-1","Body":"` + strings.Repeat("x", responseSpillMinBytes+1024) + `"}]}`
	bigMock := bootSQSJSONDelivery("sqs-big-delivery", base, base.Add(10*time.Millisecond), bigBody)

	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer func() { _ = disk.Close() }()
	if err := disk.Add(bigMock); err != nil {
		t.Fatalf("disk.Add: %v", err)
	}
	if err := disk.Flush(); err != nil {
		t.Fatalf("disk.Flush: %v", err)
	}
	disk.Finalize()

	loaded, err := disk.LoadBefore(t1Start)
	if err != nil {
		t.Fatalf("disk.LoadBefore: %v", err)
	}
	if len(loaded) != 1 || !loaded[0].HasSpilledResponse() || loaded[0].Spec.HTTPResp != nil {
		t.Fatalf("expected 1 spilled mock with nil HTTPResp, got len=%d", len(loaded))
	}

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(t1Start)
	mm.SetMocksWithWindow(loaded, nil, models.BaseTime, time.Now())

	startup, _ := mm.GetStartupMocks()
	if len(startup) != 1 || !mm.DeleteFilteredMock(*startup[0]) {
		t.Fatal("failed to consume spilled SQS delivery during boot")
	}
	consumed := mm.GetConsumedMocks()
	if len(consumed) != 1 || consumed[0].Usage != models.Deleted || !consumed[0].CarryOver {
		t.Fatalf("spilled boot delivery consumed state = %+v, want Usage=deleted CarryOver=true", consumed)
	}

	// Reload from disk and stage test-1: withoutBootCarry must still strip it
	// even though Spec.HTTPResp is nil on the freshly loaded spilled copy.
	reloaded, err := disk.LoadBefore(t1Start)
	if err != nil {
		t.Fatalf("second disk.LoadBefore: %v", err)
	}
	mm.SetMocksWithWindow(reloaded, nil, t1Start, t1End)
	stp, _ := mm.GetStartupMocks()
	if poolNames(stp)["sqs-big-delivery"] {
		t.Fatal("spilled boot delivery was re-staged into test-1 startup pool")
	}
}

func TestBootMessageDelivery_RearmAppStartAndNextSetResetAndInTestDelivery(t *testing.T) {
	base := time.Date(2026, 11, 27, 17, 11, 12, 0, time.UTC)
	t1Start := base.Add(5 * time.Second)
	t1End := t1Start.Add(200 * time.Millisecond)

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(t1Start)

	bootDeliv := bootSQSJSONDelivery("sqs-boot", base, base.Add(10*time.Millisecond), `{"Messages":[{"Body":"boot"}]}`)
	inTestDeliv := bootSQSJSONDelivery("sqs-in-test", t1Start.Add(10*time.Millisecond), t1Start.Add(20*time.Millisecond), `{"Messages":[{"Body":"t1"}]}`)
	mm.SetMocksWithWindow([]*models.Mock{bootDeliv, inTestDeliv}, nil, models.BaseTime, time.Now())

	// First app instance consumes sqs-boot during startup.
	stp, _ := mm.GetStartupMocks()
	var firstBoot *models.Mock
	for _, mk := range stp {
		if mk.Name == "sqs-boot" {
			firstBoot = mk
		}
	}
	if firstBoot == nil || !mm.DeleteFilteredMock(*firstBoot) {
		t.Fatal("first boot instance failed to consume sqs-boot")
	}
	_ = mm.GetConsumedMocks()

	// Kubelet restarts the app before test-1: RearmAppStart must allow the new
	// instance to consume sqs-boot once.
	if err := mm.RearmAppStart(); err != nil {
		t.Fatalf("RearmAppStart: %v", err)
	}
	stp, _ = mm.GetStartupMocks()
	var secondBoot *models.Mock
	for _, mk := range stp {
		if mk.Name == "sqs-boot" {
			secondBoot = mk
		}
	}
	if secondBoot == nil || !mm.DeleteFilteredMock(*secondBoot) {
		t.Fatal("restarted boot instance after RearmAppStart must receive sqs-boot once")
	}
	_ = mm.GetConsumedMocks()

	// Stage test-1: sqs-boot must be absent; sqs-in-test (not consumed at boot)
	// must be staged and consumed once with CarryOver=false.
	mm.SetMocksWithWindow([]*models.Mock{
		bootSQSJSONDelivery("sqs-boot", base, base.Add(10*time.Millisecond), `{"Messages":[{"Body":"boot"}]}`),
		bootSQSJSONDelivery("sqs-in-test", t1Start.Add(10*time.Millisecond), t1Start.Add(20*time.Millisecond), `{"Messages":[{"Body":"t1"}]}`),
	}, nil, t1Start, t1End)

	stp, _ = mm.GetStartupMocks()
	if poolNames(stp)["sqs-boot"] {
		t.Fatal("sqs-boot must not be re-staged into test-1 after restarted instance consumed it")
	}
	flt, _ := mm.GetFilteredMocks()
	if len(flt) != 1 || flt[0].Name != "sqs-in-test" {
		t.Fatalf("expected sqs-in-test in test-1 filtered pool, got %v", poolNames(flt))
	}
	if !mm.DeleteFilteredMock(*flt[0]) {
		t.Fatal("test-1 failed to consume its own sqs-in-test delivery")
	}
	c := mm.GetConsumedMocks()
	if len(c) != 1 || c[0].Name != "sqs-in-test" || c[0].Usage != models.Deleted || c[0].CarryOver {
		t.Fatalf("in-test delivery state = %+v, want Usage=deleted CarryOver=false", c)
	}

	// Next test set resets bootTaken so the next set's boot delivery works once.
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(t1Start)
	mm.SetMocksWithWindow([]*models.Mock{
		bootSQSJSONDelivery("sqs-boot", base, base.Add(10*time.Millisecond), `{"Messages":[{"Body":"boot"}]}`),
	}, nil, models.BaseTime, time.Now())
	stp, _ = mm.GetStartupMocks()
	if !poolNames(stp)["sqs-boot"] || !mm.DeleteFilteredMock(*stp[0]) {
		t.Fatal("next test set must be able to consume its own sqs-boot once")
	}
}

func TestBootMessageDelivery_ExactServingUnmarkedWorkerAndPool(t *testing.T) {
	base := time.Date(2026, 11, 27, 17, 11, 12, 0, time.UTC)
	t1Start := base.Add(5 * time.Second)
	t1End := t1Start.Add(200 * time.Millisecond)

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(t1Start)

	sqsWorker := bootSQSJSONDelivery("sqs-worker", base, base.Add(10*time.Millisecond), `{"Messages":[{"MessageId":"m1","Body":"boot"}]}`)
	sqsZeroPool := bootSQSJSONDelivery("sqs-zero-pool", time.Time{}, time.Time{}, `{"Messages":[{"MessageId":"m2","Body":"zero"}]}`)
	kafkaWorker := bootKafkaFetchDelivery("kafka-worker", base.Add(20*time.Millisecond), base.Add(30*time.Millisecond))
	sqsEmptyWorker := bootSQSJSONDelivery("sqs-empty-worker", base.Add(40*time.Millisecond), base.Add(90*time.Millisecond), `{}`)
	t1Own := bootPlainHTTPMock("health-t1", "/health", t1Start.Add(10*time.Millisecond), t1Start.Add(20*time.Millisecond))

	mm.SetMocksWithWindow([]*models.Mock{sqsWorker, sqsZeroPool, kafkaWorker, sqsEmptyWorker, t1Own}, nil, models.BaseTime, time.Now())
	mm.ServeExactWorker([]string{"sqs-worker", "kafka-worker", "sqs-empty-worker"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"sqs-zero-pool"})

	// Boot poll before test-1: worker and pool deliveries are each served once.
	if !mm.DeleteFilteredMock(*sqsWorker) {
		t.Fatal("expected first boot consume of worker SQS delivery to succeed")
	}
	if mm.DeleteFilteredMock(*sqsWorker) {
		t.Fatal("worker SQS delivery must not be served a second time during boot")
	}
	if !mm.DeleteFilteredMock(*sqsZeroPool) {
		t.Fatal("expected first boot consume of zero-ts pool SQS delivery to succeed")
	}
	if mm.DeleteFilteredMock(*sqsZeroPool) {
		t.Fatal("zero-ts pool SQS delivery must not be served a second time during boot")
	}
	if !mm.DeleteFilteredMock(*kafkaWorker) {
		t.Fatal("expected first boot consume of worker Kafka delivery to succeed")
	}
	if mm.DeleteFilteredMock(*kafkaWorker) {
		t.Fatal("worker Kafka delivery must not be served a second time during boot")
	}
	// Empty SQS poll is NOT a message delivery and stays reusable between tests.
	if !mm.DeleteFilteredMock(*sqsEmptyWorker) {
		t.Fatal("expected first boot consume of empty SQS poll to succeed")
	}
	if !mm.DeleteFilteredMock(*sqsEmptyWorker) {
		t.Fatal("empty SQS poll must remain available between tests")
	}

	// Open test-1: boot-consumed deliveries must stay hidden and refused in test-1,
	// while empty SQS poll and test-1's own mock remain visible.
	mm.ServeExactTest("test-1", []*models.Mock{t1Own})
	flt, _ := mm.GetFilteredMocks()
	stp, _ := mm.GetStartupMocks()
	sess, _ := mm.GetSessionScopedMocks()
	visible := map[string]bool{}
	for _, list := range [][]*models.Mock{flt, stp, sess} {
		for _, mk := range list {
			visible[mk.Name] = true
		}
	}
	for _, deliv := range []string{"sqs-worker", "sqs-zero-pool", "kafka-worker"} {
		if visible[deliv] {
			t.Fatalf("boot-consumed exact delivery %q must not be visible in test-1", deliv)
		}
	}
	if mm.DeleteFilteredMock(*sqsWorker) || mm.DeleteFilteredMock(*sqsZeroPool) || mm.DeleteFilteredMock(*kafkaWorker) {
		t.Fatal("boot-consumed exact deliveries must be refused inside test-1")
	}
	if !visible["sqs-empty-worker"] {
		t.Fatal("empty SQS poll must remain visible in test-1")
	}
	if !visible["health-t1"] {
		t.Fatal("test-1's own mock must be visible in test-1")
	}

	// RestartExactKey restores them for a retry cycle.
	mm.ServeExactPool(models.PoolKeyNone, []string{"sqs-zero-pool"})
	mm.RestartExactKey(models.PoolKeyNone, []string{"sqs-worker", "sqs-zero-pool", "kafka-worker"})
	if !mm.DeleteFilteredMock(*sqsWorker) || !mm.DeleteFilteredMock(*sqsZeroPool) || !mm.DeleteFilteredMock(*kafkaWorker) {
		t.Fatal("RestartExactKey must allow retried package to consume boot deliveries once")
	}
	_ = t1End
}
