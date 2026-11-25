package manager

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

func mongoOwnerReadAt(ts time.Time, serviceID, lsid string, sortDir int) *models.Mock {
	return &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    fmt.Sprintf("mock-orig-%s-%s", serviceID, lsid),
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":     "mocks",
				"destAddr": "mongo:27017",
			},
			MongoRequests: []models.MongoRequest{
				{
					Header: &models.MongoHeader{Opcode: 2013},
					Message: &models.MongoOpMessage{
						Sections: []string{
							fmt.Sprintf(`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":%q,"owner.id":{"$nin":[null,""]}},"sort":{"version":%d},"projection":{"owner.id":1},"hint":{"serviceId":1,"branchId":1,"version":-1},"lsid":{"id":%q},"$db":"appdb"} }`, serviceID, sortDir, lsid),
						},
					},
				},
			},
			MongoResponses: []models.MongoResponse{
				{
					Header: &models.MongoHeader{Opcode: 2013},
					Message: &models.MongoOpMessage{
						Sections: []string{
							fmt.Sprintf(`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"doc-%s","owner":{"id":"owner-%s"}}],"id":{"$numberLong":"0"},"ns":"appdb.service_configs"},"ok":{"$numberDouble":"1.0"}} }`, serviceID, serviceID),
						},
					},
				},
			},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(5 * time.Millisecond),
		},
	}
}

func mongoWriteAt(ts time.Time, cmd string) *models.Mock {
	return &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    fmt.Sprintf("mock-write-%s", cmd),
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":     "mocks",
				"destAddr": "mongo:27017",
			},
			MongoRequests: []models.MongoRequest{
				{
					Header: &models.MongoHeader{Opcode: 2013},
					Message: &models.MongoOpMessage{
						Sections: []string{
							fmt.Sprintf(`{ SectionSingle msg: {%q:"service_configs","ordered":true,"$db":"appdb"} }`, cmd),
						},
					},
				},
			},
			MongoResponses: []models.MongoResponse{
				{
					Header: &models.MongoHeader{Opcode: 2013},
					Message: &models.MongoOpMessage{
						Sections: []string{`{ SectionSingle msg: {"n":1,"ok":1} }`},
					},
				},
			},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(5 * time.Millisecond),
		},
	}
}

func TestSyncMock_SampledOutReadsRescuedCopyOnlyAndWritesDropped(t *testing.T) {
	t.Parallel()
	outCh := make(chan *models.Mock, 64)
	mapCh := make(chan models.TestMockMapping, 64)
	mgr := lateMockTestManager(outCh, mapCh, false)

	now := time.Now()
	ownerRead := mongoOwnerReadAt(now.Add(-10*time.Second), "svc-alpha", "sess-1", 1)
	origName := ownerRead.Name
	updateWrite := mongoWriteAt(now.Add(-10*time.Second+time.Millisecond), "update")
	insertWrite := mongoWriteAt(now.Add(-10*time.Second+2*time.Millisecond), "insert")
	deleteWrite := mongoWriteAt(now.Add(-10*time.Second+3*time.Millisecond), "delete")
	mgr.AddMock(ownerRead)
	mgr.AddMock(updateWrite)
	mgr.AddMock(insertWrite)
	mgr.AddMock(deleteWrite)

	winStart := now.Add(-500 * time.Millisecond)
	winEnd := now
	keptMock := mongoOwnerReadAt(winStart.Add(10*time.Millisecond), "svc-beta", "sess-2", -1)
	mgr.AddMock(keptMock)

	mgr.ResolveRange(winStart, winEnd, "test-get-config-1", true, true)

	// Original mock pointer must NEVER be mutated by read-through rescue.
	if ownerRead.Name != origName {
		t.Fatalf("read-through rescue mutated caller mock Name: got %q, want %q", ownerRead.Name, origName)
	}
	if ownerRead.Spec.Metadata[models.MetadataReadThroughKey] != "" {
		t.Fatalf("read-through rescue mutated caller mock Metadata: %v", ownerRead.Spec.Metadata)
	}

	emitted := sentMocks(outCh)
	var rtMocks, normalMocks []*models.Mock
	for _, m := range emitted {
		if m.IsReadThrough() {
			rtMocks = append(rtMocks, m)
		} else {
			normalMocks = append(normalMocks, m)
		}
	}
	if len(rtMocks) != 1 {
		t.Fatalf("expected exactly 1 rescued read-through mock (writes dropped), got %d (total emitted=%d)", len(rtMocks), len(emitted))
	}
	if len(normalMocks) != 1 || normalMocks[0].Name != keptMock.Name {
		t.Fatalf("expected kept mock %s, got %d normal mocks", keptMock.Name, len(normalMocks))
	}
	ids := mappedIDs(mapCh, "test-get-config-1")
	if ids[rtMocks[0].Name] {
		t.Fatalf("read-through mock %s leaked into test-get-config-1 mapping", rtMocks[0].Name)
	}
	if !ids[normalMocks[0].Name] {
		t.Fatalf("kept mock %s missing from test-get-config-1 mapping", normalMocks[0].Name)
	}
}

func TestSyncMock_ReadThroughDeduplicatedAndBounded(t *testing.T) {
	t.Parallel()
	outCh := make(chan *models.Mock, 128)
	mapCh := make(chan models.TestMockMapping, 64)
	mgr := lateMockTestManager(outCh, mapCh, false)
	mgr.setReadThroughLimits(5, 1<<20)

	now := time.Now()
	old := now.Add(-15 * time.Second)
	for i := 0; i < 100; i++ {
		mgr.AddMock(mongoOwnerReadAt(old.Add(time.Duration(i)*time.Microsecond), "svc-alpha", fmt.Sprintf("sess-%d", i), 1))
	}
	for i := 0; i < 20; i++ {
		mgr.AddMock(mongoOwnerReadAt(old.Add(time.Duration(100+i)*time.Microsecond), fmt.Sprintf("svc-uniq-%d", i), "sess-u", 1))
	}

	mgr.ResolveRange(now.Add(-50*time.Millisecond), now, "test-trigger", true, true)
	emitted := sentMocks(outCh)
	rtCount := 0
	for _, m := range emitted {
		if m.IsReadThrough() {
			rtCount++
		}
	}
	if rtCount != 5 {
		t.Fatalf("expected read-through cap=5, got %d emitted read-through mocks", rtCount)
	}
	rescued, deduped, capped, entries, _ := mgr.readThroughStats()
	if rescued != 5 || entries != 5 || deduped != 99 || capped != 16 {
		t.Fatalf("unexpected stats: rescued=%d deduped=%d capped=%d entries=%d", rescued, deduped, capped, entries)
	}
}

func TestSyncMock_UnservedKindsNeverRescueOrCrowdOutServedKinds(t *testing.T) {
	t.Parallel()
	outCh := make(chan *models.Mock, 128)
	mapCh := make(chan models.TestMockMapping, 64)
	mgr := lateMockTestManager(outCh, mapCh, false)
	mgr.setReadThroughLimits(3, 1<<20)

	now := time.Now()
	old := now.Add(-15 * time.Second)
	// Flood with unserved kinds (Redis, PostgresV3, HTTP2) well past the cap of 3.
	for i := 0; i < 20; i++ {
		ts := old.Add(time.Duration(i) * time.Microsecond)
		key := fmt.Sprintf("user:%d", i)
		mgr.AddMock(&models.Mock{
			Version: models.Version("api.keploy.io/v1beta2"),
			Name:    fmt.Sprintf("redis-%d", i),
			Kind:    models.REDIS,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks", "destAddr": "redis:6379"},
				GenericRequests:  []models.Payload{{Message: []models.OutputBinary{{Type: "utf-8", Data: fmt.Sprintf("*2\r\n$3\r\nGET\r\n$%d\r\n%s\r\n", len(key), key)}}}},
				GenericResponses: []models.Payload{{Message: []models.OutputBinary{{Type: "utf-8", Data: "$2\r\nok\r\n"}}}},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
			},
		})
		mgr.AddMock(&models.Mock{
			Version: models.Version("api.keploy.io/v1beta2"),
			Name:    fmt.Sprintf("pg-%d", i),
			Kind:    models.PostgresV3,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "mocks", "destAddr": "pg:5432"},
				PostgresV3: &models.PostgresV3Spec{
					Type: models.PostgresV3TypeQuery,
					Query: &models.PostgresV3QuerySpec{
						SQLNormalized: "SELECT id FROM users WHERE id = $1",
						Literals:      models.PostgresV3Cells{{Value: fmt.Sprintf("%d", i)}},
						Response:      &models.PostgresV3Response{CommandComplete: "SELECT 1"},
					},
				},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
			},
		})
		mgr.AddMock(&models.Mock{
			Version: models.Version("api.keploy.io/v1beta2"),
			Name:    fmt.Sprintf("h2-%d", i),
			Kind:    models.HTTP2,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks"},
				HTTP2Req:         &models.HTTP2Req{Method: "GET", Authority: "svc.internal", URL: fmt.Sprintf("/v1/item/%d", i)},
				HTTP2Resp:        &models.HTTP2Resp{StatusCode: 200, Body: `{"ok":true}`},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
			},
		})
	}

	// Follow with one of each of the 3 served kinds (Mongo, HTTP/1.1, MySQL).
	mgr.AddMock(mongoOwnerReadAt(old.Add(time.Millisecond), "svc-mongo", "s1", 1))
	mgr.AddMock(&models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "http-served",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
			HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://auth.internal/v1/jwks", Header: map[string]string{"Host": "auth.internal"}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"keys":[]}`},
			ReqTimestampMock: old.Add(2 * time.Millisecond),
			ResTimestampMock: old.Add(3 * time.Millisecond),
		},
	})
	mgr.AddMock(&models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mysql-served",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:      map[string]string{"type": "mocks", "database": "tenant_a", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT * FROM settings LIMIT 1"}}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
				FinalResponse: &mysql.GenericResponse{Data: []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00}, Type: "EOF"},
			}}}},
			ReqTimestampMock: old.Add(4 * time.Millisecond),
			ResTimestampMock: old.Add(5 * time.Millisecond),
		},
	})

	mgr.ResolveRange(now.Add(-50*time.Millisecond), now, "test-trigger", true, true)
	emitted := sentMocks(outCh)
	kinds := map[models.Kind]int{}
	for _, m := range emitted {
		if m.IsReadThrough() {
			kinds[m.Kind]++
		}
	}
	if kinds[models.REDIS] != 0 || kinds[models.PostgresV3] != 0 || kinds[models.HTTP2] != 0 {
		t.Fatalf("unserved kinds must never be rescued into read-through tier: %v", kinds)
	}
	if kinds[models.Mongo] != 1 || kinds[models.HTTP] != 1 || kinds[models.MySQL] != 1 {
		t.Fatalf("all 3 served kinds must be rescued without being crowded out: %v", kinds)
	}
	rescued, _, capped, entries, _ := mgr.readThroughStats()
	if rescued != 3 || entries != 3 || capped != 0 {
		t.Fatalf("unexpected stats after unserved flood: rescued=%d entries=%d capped=%d", rescued, entries, capped)
	}
}

func TestSyncMock_FlushUnownedReadThroughRespectsOpenWindows(t *testing.T) {
	t.Parallel()
	outCh := make(chan *models.Mock, 32)
	mapCh := make(chan models.TestMockMapping, 32)
	mgr := lateMockTestManager(outCh, mapCh, false)

	now := time.Now()
	// Earlier sampled-out read BEFORE open window -> rescued at shutdown.
	beforeWin := mongoOwnerReadAt(now.Add(-2*time.Second), "svc-before", "s1", 1)
	mgr.AddMock(beforeWin)

	// Open an in-flight window at now-500ms; mock inside it must NOT be rescued
	// or evicted by flushUnownedReadThrough.
	win := mgr.OpenWindow(now.Add(-500*time.Millisecond), nil)
	inFlight := mongoOwnerReadAt(now.Add(-100*time.Millisecond), "svc-inflight", "s2", 1)
	mgr.AddMock(inFlight)

	mgr.flushUnownedReadThrough()
	emitted := sentMocks(outCh)
	if len(emitted) != 1 {
		t.Fatalf("expected only pre-window read rescued while open window exists, got %d", len(emitted))
	}
	win.Close()
}

// TestSyncMock_EndRecordingFlushesUnownedReadThrough verifies that
// EndRecording (called by endRecordingStreams before the mock stream's end
// marker on both CLI --sync stop and sidecar recording stop) flushes owned
// late mocks to their owners FIRST (so an owned read is never duplicated as
// mock-rt-*), then rescues genuinely unowned read-only mocks still sitting in
// m.buffer before clearing it, while respecting open in-flight windows.
func TestSyncMock_EndRecordingFlushesUnownedReadThrough(t *testing.T) {
	t.Parallel()
	outCh := make(chan *models.Mock, 32)
	mapCh := make(chan models.TestMockMapping, 32)
	mgr := lateMockTestManager(outCh, mapCh, false)

	now := time.Now()
	// 1. Resolve a kept test window [now-4s, now-3s], then ingest a late
	//    read-only mock inside that window: EndRecording MUST deliver it as an
	//    owned mock (via FlushOwnedWindows) and MUST NOT duplicate it as mock-rt-*.
	keptStart := now.Add(-4 * time.Second)
	keptEnd := now.Add(-3 * time.Second)
	mgr.ResolveRange(keptStart, keptEnd, "test-kept-late", true, false)
	lateOwned := mongoOwnerReadAt(keptStart.Add(100*time.Millisecond), "svc-late-owned", "s-owned", 1)
	mgr.AddMock(lateOwned)

	// 2. Sampled-out / filtered-out read captured outside any kept window and
	//    within StaleHorizon of stop -> MUST be rescued as mock-rt-*.
	unowned := mongoOwnerReadAt(now.Add(-2*time.Second), "svc-endrec", "s1", 1)
	mgr.AddMock(unowned)

	// 3. Open in-flight window at now-500ms; mock inside it must NOT be rescued.
	win := mgr.OpenWindow(now.Add(-500*time.Millisecond), nil)
	inFlight := mongoOwnerReadAt(now.Add(-100*time.Millisecond), "svc-inflight-end", "s2", 1)
	mgr.AddMock(inFlight)

	mgr.EndRecording()
	win.Close()

	emitted := sentMocks(outCh)
	var ownedCount, rtCount int
	for _, mk := range emitted {
		if len(mk.Spec.MongoRequests) == 0 {
			t.Fatalf("unexpected mock without MongoRequests: %+v", mk)
		}
		msg, ok := mk.Spec.MongoRequests[0].Message.(*models.MongoOpMessage)
		if !ok || len(msg.Sections) == 0 {
			t.Fatalf("unexpected MongoRequest message: %+v", mk.Spec.MongoRequests[0].Message)
		}
		if mk.IsReadThrough() {
			rtCount++
			if !strings.Contains(msg.Sections[0], "svc-endrec") {
				t.Fatalf("expected only svc-endrec rescued as read-through, got %s", msg.Sections[0])
			}
		} else {
			ownedCount++
			if !strings.Contains(msg.Sections[0], "svc-late-owned") {
				t.Fatalf("expected owned mock to be svc-late-owned, got %s", msg.Sections[0])
			}
		}
	}
	if ownedCount != 1 || rtCount != 1 {
		t.Fatalf("EndRecording emitted owned=%d readThrough=%d, want owned=1 readThrough=1", ownedCount, rtCount)
	}
}
