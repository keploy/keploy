package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

func mongoFindMock(name, serviceID string, sortDir int, projField, ownerID string, ts time.Time) *models.Mock {
	return &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    name,
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":     "mocks",
				"destAddr": "mongo:27017",
			},
			MongoRequests: []models.MongoRequest{{
				Header: &models.MongoHeader{Opcode: 2013},
				Message: &models.MongoOpMessage{
					Sections: []string{
						fmt.Sprintf(`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":%q,"owner.id":{"$nin":[null,""]}},"sort":{"version":%d},"projection":{%q:1},"hint":{"serviceId":1,"branchId":1,"version":-1},"$db":"appdb"} }`, serviceID, sortDir, projField),
					},
				},
			}},
			MongoResponses: []models.MongoResponse{{
				Header: &models.MongoHeader{Opcode: 2013},
				Message: &models.MongoOpMessage{
					Sections: []string{
						fmt.Sprintf(`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"doc","owner":{"id":%q}}],"id":{"$numberLong":"0"},"ns":"appdb.service_configs"},"ok":{"$numberDouble":"1.0"}} }`, ownerID),
					},
				},
			}},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(5 * time.Millisecond),
		},
	}
}

func mongoWriteMock(name, cmd string, ts time.Time) *models.Mock {
	return &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    name,
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":     "mocks",
				"destAddr": "mongo:27017",
			},
			MongoRequests: []models.MongoRequest{{
				Header: &models.MongoHeader{Opcode: 2013},
				Message: &models.MongoOpMessage{
					Sections: []string{fmt.Sprintf(`{ SectionSingle msg: {%q:"service_configs","ordered":true,"$db":"appdb"} }`, cmd)},
				},
			}},
			MongoResponses: []models.MongoResponse{{
				Header:  &models.MongoHeader{Opcode: 2013},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"n":1,"ok":1} }`}},
			}},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(5 * time.Millisecond),
		},
	}
}

func TestMockManager_ReadThroughTierExactReusableAndWriteExcludedBothProvenModes(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			logger := zap.NewNop()
			mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), logger)

			var observed atomic.Int64
			mm.SetServedObserver(func(_ *models.Mock) { observed.Add(1) })

			t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

			rtOwner := mongoFindMock("mock-rt-owner", "svc-alpha", 1, "owner.id", "owner-from-rt", t0.Add(-30*time.Second))
			if !models.MarkReadThrough(rtOwner) {
				t.Fatal("MarkReadThrough failed for Mongo find")
			}
			rtHTTP := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "mock-rt-http",
				Kind:    models.HTTP,
				Spec: models.MockSpec{
					Metadata: map[string]string{
						"type":                     "mocks",
						models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					},
					HTTPReq:  &models.HTTPReq{Method: models.Method("GET"), URL: "http://auth.internal/v1/jwks", Header: map[string]string{"Host": "auth.internal"}},
					HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"keys":["k1"]}`},
				},
				TestModeInfo: models.TestModeInfo{
					Lifetime:        models.LifetimePerTest,
					LifetimeDerived: true,
				},
			}
			if !models.MarkReadThrough(rtHTTP) {
				t.Fatal("MarkReadThrough failed for HTTP GET")
			}
			rtSQL := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "mock-rt-mysql",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:      map[string]string{"type": "mocks"},
					MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants WHERE slug = 'acme'"}}}},
					MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextResultSet{
						FinalResponse: &mysql.GenericResponse{Data: []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00}, Type: "EOF"},
					}}}},
				},
			}
			if !models.MarkReadThrough(rtSQL) {
				t.Fatal("MarkReadThrough failed for MySQL SELECT")
			}
			// Hand-tagged write mock MUST be dropped fail-closed on staging.
			rtWrite := mongoWriteMock("mock-rt-bad-write", "update", t0.Add(-25*time.Second))
			rtWrite.Spec.Metadata[models.MetadataReadThroughKey] = models.MetadataReadThroughValue

			ownLatest := mongoFindMock("mock-101", "svc-alpha", -1, "owner.id", "owner-latest", t0.Add(10*time.Second))
			ownLatest.TestModeInfo.IsFiltered = true

			mm.SetMocksWithWindow([]*models.Mock{ownLatest, rtOwner, rtHTTP, rtSQL, rtWrite}, nil, t0, t0.Add(time.Minute))
			if proven == "1" {
				mm.ServeExactTest("test-get-config-1", []*models.Mock{ownLatest})
			}

			// 1. Per-test filtered pool holds ONLY ownLatest; GetSessionMocks
			//    holds NO read-through mocks (no fuzzy HTTP2/MySQL/session leakage).
			flt, err := mm.GetPerTestMocksInWindow()
			if err != nil {
				t.Fatalf("GetPerTestMocksInWindow: %v", err)
			}
			for _, m := range flt {
				if m.Name == rtOwner.Name || m.Name == rtWrite.Name || m.Name == rtHTTP.Name || m.Name == rtSQL.Name {
					t.Fatalf("read-through mock %s leaked into per-test pool", m.Name)
				}
			}
			sess, err := mm.GetSessionMocks()
			if err != nil {
				t.Fatalf("GetSessionMocks: %v", err)
			}
			for _, m := range sess {
				if m.Name == rtOwner.Name || m.Name == rtHTTP.Name || m.Name == rtSQL.Name {
					t.Fatalf("read-through mock %s leaked into GetSessionMocks", m.Name)
				}
			}
			sessWin, err := mm.GetSessionMocksInWindow(t0.Add(-time.Hour), t0.Add(time.Hour))
			if err != nil {
				t.Fatalf("GetSessionMocksInWindow: %v", err)
			}
			for _, m := range sessWin {
				if m.Name == rtOwner.Name || m.Name == rtHTTP.Name || m.Name == rtSQL.Name {
					t.Fatalf("read-through mock %s leaked into GetSessionMocksInWindow", m.Name)
				}
			}

			// 2. GetStartupMocks / GetStartupMocksByKind must NEVER expose read-through
			//    mocks (Mongo, HTTP, MySQL, or write) in EITHER mode — all three kinds
			//    are served strictly via exact-key MatchReadThrough on miss, preventing
			//    loose matchStartup cross-entity serving in default mode and keeping
			//    both modes unified.
			startup1, err := mm.GetStartupMocks()
			if err != nil {
				t.Fatalf("GetStartupMocks: %v", err)
			}
			for _, s := range startup1 {
				switch s.Name {
				case rtOwner.Name, rtHTTP.Name, rtSQL.Name, rtWrite.Name:
					t.Fatalf("read-through mock %s must NOT appear in GetStartupMocks (KEPLOY_PROVEN=%q)", s.Name, proven)
				}
			}
			startupMongoByKind, err := mm.GetStartupMocksByKind(models.Mongo)
			if err != nil {
				t.Fatalf("GetStartupMocksByKind(Mongo): %v", err)
			}
			for _, s := range startupMongoByKind {
				if s.Name == rtOwner.Name || s.Name == rtWrite.Name {
					t.Fatalf("read-through mock %s must NOT appear in GetStartupMocksByKind(Mongo) (KEPLOY_PROVEN=%q)", s.Name, proven)
				}
			}

			// 3. Defense-in-depth: DeleteStartupMock on a staged read-through mock
			//    records ReadThrough=true, stays reusable, and observes serve ONCE.
			beforeObs := observed.Load()
			if !mm.DeleteStartupMock(*rtOwner) {
				t.Fatalf("DeleteStartupMock defense-in-depth failed on read-through mock (KEPLOY_PROVEN=%q)", proven)
			}
			if gotObs := observed.Load() - beforeObs; gotObs != 1 {
				t.Fatalf("expected exactly 1 observeServed call on DeleteStartupMock, got %d", gotObs)
			}
			if !mm.DeleteStartupMock(*rtOwner) {
				t.Fatalf("second DeleteStartupMock must also succeed (read-through is reusable)")
			}

			// 4. MatchReadThrough works on MockManager AND through scopedMockDb
			//    in BOTH modes for exact HTTP/1.1, MySQL, and Mongo hits.
			scoped := &scopedMockDb{MockMemDb: mm}
			liveSQL := &models.Mock{
				Kind: models.MySQL,
				Spec: models.MockSpec{
					MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT id FROM tenants WHERE slug = 'acme'"}}}},
				},
			}
			if gotSQL, hitSQL := integrations.MatchReadThrough(scoped, liveSQL); !hitSQL || gotSQL == nil || gotSQL.Name != rtSQL.Name {
				t.Fatalf("expected exact MySQL MatchReadThrough hit in KEPLOY_PROVEN=%q, got hit=%v mock=%v", proven, hitSQL, gotSQL)
			}
			liveOwner := mongoFindMock("", "svc-alpha", 1, "owner.id", "", time.Time{})
			got, hit := integrations.MatchReadThrough(scoped, liveOwner)
			if !hit || got == nil || got.Name != rtOwner.Name {
				t.Fatalf("expected exact MatchReadThrough hit via scopedMockDb, got hit=%v mock=%v", hit, got)
			}
			for _, tc := range []struct {
				name string
				live *models.Mock
			}{
				{"wrong-sort", mongoFindMock("", "svc-alpha", -1, "owner.id", "", time.Time{})},
				{"wrong-projection", mongoFindMock("", "svc-alpha", 1, "owner.name", "", time.Time{})},
				{"wrong-filter", mongoFindMock("", "svc-other", 1, "owner.id", "", time.Time{})},
				{"write-update", mongoWriteMock("", "update", time.Time{})},
			} {
				if m, ok := integrations.MatchReadThrough(scoped, tc.live); ok || m != nil {
					t.Fatalf("%s MUST miss read-through tier, got %v", tc.name, m)
				}
			}

			// 5. Consumed telemetry records ReadThrough=true and Updated (never Deleted).
			consumed := mm.GetConsumedMocks()
			var sawRT bool
			for _, st := range consumed {
				if st.Name == rtOwner.Name {
					sawRT = true
					if !st.ReadThrough {
						t.Fatalf("read-through mock %s missing ReadThrough=true on MockState", st.Name)
					}
					if st.Usage != models.Updated {
						t.Fatalf("read-through mock usage=%v, want Updated", st.Usage)
					}
				}
			}
			if !sawRT {
				t.Fatalf("read-through mock %s missing from GetConsumedMocks()", rtOwner.Name)
			}

			// 6. Repeated read-through serves (including mocks recorded with
			//    LifetimePerTest) must increment ServedCounts while NEVER
			//    incrementing m.setServed (per_test_recordings_served_twice_or_more).
			liveHTTP := &models.Mock{
				Kind: models.HTTP,
				Spec: models.MockSpec{
					HTTPReq: &models.HTTPReq{
						Method: models.Method("GET"),
						URL:    "http://auth.internal/v1/jwks",
						Header: map[string]string{"Host": "auth.internal"},
					},
				},
			}
			for i := 0; i < 3; i++ {
				gotHTTP, hitHTTP := integrations.MatchReadThrough(scoped, liveHTTP)
				if !hitHTTP || gotHTTP == nil || gotHTTP.Name != rtHTTP.Name {
					t.Fatalf("read-through HTTP hit #%d failed: hit=%v mock=%v", i+1, hitHTTP, gotHTTP)
				}
			}
			servedCounts := mm.ServedCounts()
			if servedCounts[rtHTTP.Name] != 3 {
				t.Fatalf("expected 3 served counts on repeated HTTP read-through mock, got %d", servedCounts[rtHTTP.Name])
			}
			mm.consumedMu.Lock()
			setServedRT := mm.setServed[rtOwner.Name]
			setServedHTTP := mm.setServed[rtHTTP.Name]
			mm.consumedMu.Unlock()
			if setServedRT != 0 || setServedHTTP != 0 {
				t.Fatalf("read-through hits must not increment setServed, got owner=%d http=%d", setServedRT, setServedHTTP)
			}

			// 7. isReadThroughMock must verify membership in m.readThrough.byName
			//    and reject hand-tagged write mocks or unstaged mocks carrying the tag.
			if !mm.isReadThroughMock(rtOwner) {
				t.Fatal("isReadThroughMock must return true for staged read-through mock")
			}
			if mm.isReadThroughMock(rtWrite) {
				t.Fatal("isReadThroughMock must return false for hand-tagged write mock")
			}
			unstagedRT := mongoFindMock("mock-rt-unstaged", "svc-unstaged", 1, "owner.id", "owner-x", t0)
			if !models.MarkReadThrough(unstagedRT) {
				t.Fatal("MarkReadThrough failed for unstagedRT")
			}
			if mm.isReadThroughMock(unstagedRT) {
				t.Fatal("isReadThroughMock must return false for unstaged mock not in m.readThrough.byName")
			}

			// 8. Conflicting responses recorded for the SAME canonical key (e.g., two
			//    separate servers sharing port and schema, or a refreshed cache) MUST
			//    fail closed end-to-end through SyncMockManager -> YAML -> MockManager,
			//    while identical duplicates differing only in wire metadata (RequestID,
			//    ReadDelay, $clusterTime, operationTime) remain servable.
			rtConflictA := mongoFindMock("mock-rt-conflict-a", "svc-conflict", 1, "owner.id", "owner-server-A", t0)
			rtConflictB := mongoFindMock("mock-rt-conflict-b", "svc-conflict", 1, "owner.id", "owner-server-B", t0.Add(time.Second))
			rtIdentical1 := mongoFindMock("mock-rt-ident-1", "svc-ident", 1, "owner.id", "owner-same", t0)
			rtIdentical1.Spec.MongoResponses[0].Header = &models.MongoHeader{RequestID: 11, ResponseTo: 1, Opcode: 2013}
			rtIdentical1.Spec.MongoResponses[0].ReadDelay = 3
			rtIdentical2 := mongoFindMock("mock-rt-ident-2", "svc-ident", 1, "owner.id", "owner-same", t0.Add(time.Second))
			rtIdentical2.Spec.MongoResponses[0].Header = &models.MongoHeader{RequestID: 99, ResponseTo: 55, Opcode: 2013}
			rtIdentical2.Spec.MongoResponses[0].ReadDelay = 88
			rtIdentical2.Spec.MongoResponses[0].Message = &models.MongoOpMessage{
				Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"doc","owner":{"id":"owner-same"}}],"id":{"$numberLong":"0"},"ns":"appdb.service_configs"},"ok":{"$numberDouble":"1.0"},"$clusterTime":{"clusterTime":777},"operationTime":777} }`},
			}
			mgrC := syncMock.New(logger)
			mgrC.StartRecordSession()
			outC := make(chan *models.Mock, 32)
			mapC := make(chan models.TestMockMapping, 16)
			mgrC.SetOutputChannel(outC)
			mgrC.SetMappingChannel(context.Background(), mapC)
			mgrC.SetFirstRequestSignaled()
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				tBoot := t0.Add(-2 * time.Minute).Add(time.Duration(i) * time.Second)
				mgrC.ResolveRange(tBoot, tBoot.Add(10*time.Millisecond), fmt.Sprintf("c-boot-%d", i), true, true)
			}
			mgrC.AddMock(rtConflictA)
			mgrC.AddMock(rtConflictB)
			mgrC.AddMock(rtIdentical1)
			mgrC.AddMock(rtIdentical2)
			mgrC.ResolveRange(t0.Add(-time.Millisecond), t0.Add(2*time.Second), "c-sampled-out", false, true)
			mgrC.CloseOutChan()

			dirC := t.TempDir()
			diskC := mockdb.New(logger, dirC, "mocks")
			for mk := range outC {
				if err := diskC.InsertMock(context.Background(), mk, "conflict-set"); err != nil {
					t.Fatalf("InsertMock(%s): %v", mk.Name, err)
				}
			}
			relC, err := diskC.GetTestSetMocks(context.Background(), "conflict-set", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks(conflict-set): %v", err)
			}
			mmC := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), logger)
			t.Cleanup(mmC.Close)
			mmC.SetMocksWithWindow(append(relC.Filtered, relC.Unfiltered...), nil, t0.Add(10*time.Second), t0.Add(20*time.Second))
			liveConflict := mongoFindMock("", "svc-conflict", 1, "owner.id", "", time.Time{})
			if gotC, hitC := integrations.MatchReadThrough(mmC, liveConflict); hitC || gotC != nil {
				t.Fatalf("conflicting read-through responses recorded via SyncMockManager must fail closed on replay, got %v", gotC)
			}
			liveIdent := mongoFindMock("", "svc-ident", 1, "owner.id", "", time.Time{})
			if gotI, hitI := integrations.MatchReadThrough(mmC, liveIdent); !hitI || gotI == nil {
				t.Fatal("identical duplicate read-through responses differing only in wire metadata must remain servable")
			}
		})
	}
}

func TestEndToEnd_SampledOutRecordToYAMLReloadToColdReplayExactFallback(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			ctx := context.Background()
			logger := zap.NewNop()

			// 1. RECORD: advance past startup window, then capture a sampled-out
			//    cache-warming Mongo find + sampled-out write.
			mgr := syncMock.Get()
			mgr.StartRecordSession()
			outCh := make(chan *models.Mock, 32)
			mapCh := make(chan models.TestMockMapping, 32)
			mgr.SetOutputChannel(outCh)
			mgr.SetMappingChannel(ctx, mapCh)
			mgr.SetFirstRequestSignaled()
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				tBoot := time.Now().Add(-2 * time.Minute).Add(time.Duration(i) * time.Second)
				mgr.ResolveRange(tBoot, tBoot.Add(10*time.Millisecond), fmt.Sprintf("boot-test-%d", i), true, true)
			}

			t0 := time.Now().Add(-30 * time.Second)
			sampledRead := mongoFindMock("orig-sampled-read", "svc-alpha", 1, "owner.id", "owner-warmed", t0)
			sampledWrite := mongoWriteMock("orig-sampled-write", "update", t0.Add(time.Millisecond))
			// Origin-form HTTPS/1.1 mock recorded over TLS (meta["tls"]="true").
			sampledHTTPS := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "orig-sampled-https",
				Kind:    models.HTTP,
				Spec: models.MockSpec{
					Metadata: map[string]string{
						"name":                     "Http",
						"type":                     models.HTTPClient,
						"operation":                "GET",
						"tls":                      "true",
						models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					},
					HTTPReq: &models.HTTPReq{
						Method: models.Method("GET"),
						URL:    "/v1/jwks?tenant=secure",
						Header: map[string]string{"Host": "auth.internal", "Accept": "application/json"},
					},
					HTTPResp: &models.HTTPResp{
						StatusCode: 200,
						Body:       `{"source":"https-read-through"}`,
					},
					ReqTimestampMock: t0.Add(2 * time.Millisecond),
					ResTimestampMock: t0.Add(7 * time.Millisecond),
				},
			}
			mgr.AddMock(sampledRead)
			mgr.AddMock(sampledWrite)
			mgr.AddMock(sampledHTTPS)

			winStart := time.Now().Add(-500 * time.Millisecond)
			winEnd := time.Now()
			keptOwn := mongoFindMock("mock-own-test", "svc-alpha", -1, "owner.id", "owner-latest", winStart.Add(10*time.Millisecond))
			mgr.AddMock(keptOwn)
			mgr.ResolveRange(winStart, winEnd, "test-cold-1", true, true)
			mgr.CloseOutChan()

			// Persist emitted mocks to YAML via real mockdb.
			dir := t.TempDir()
			db := mockdb.New(logger, dir, "mocks")
			for mk := range outCh {
				if err := db.InsertMock(ctx, mk, "test-set-0"); err != nil {
					t.Fatalf("InsertMock(%s): %v", mk.Name, err)
				}
			}

			// 2. RELOAD from disk via real mockdb (exact replay loader path).
			reloaded, err := db.GetTestSetMocks(ctx, "test-set-0", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks: %v", err)
			}
			loaded := append(reloaded.Filtered, reloaded.Unfiltered...)
			var hasRT, hasWrite bool
			for _, mk := range loaded {
				if mk.IsReadThrough() {
					hasRT = true
					if len(mk.Name) <= len("mock-rt-") || mk.Name[:8] != "mock-rt-" {
						t.Fatalf("persisted read-through mock lost mock-rt- name: %q", mk.Name)
					}
				}
				if mk.Name == "orig-sampled-write" {
					hasWrite = true
				}
			}
			if !hasRT {
				t.Fatal("expected rescued read-through mock persisted and reloaded from YAML")
			}
			if hasWrite {
				t.Fatal("sampled-out write mock must never be persisted")
			}

			// 3. COLD REPLAY: stage reloaded mocks into MockManager.
			mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), logger)
			var observed atomic.Int64
			mm.SetServedObserver(func(_ *models.Mock) { observed.Add(1) })
			mm.SetMocksWithWindow(loaded, nil, winStart, winEnd)
			if proven == "1" {
				mm.ServeExactTest("test-cold-1", []*models.Mock{keptOwn})
			}

			// 3a. Production Mongo cold-replay path in BOTH modes:
			//     - GetStartupMocksByKind(Mongo) and GetStartupMocksByKind(HTTP)
			//       must NEVER expose read-through mocks in EITHER mode.
			//     - Exact MatchReadThrough through heldScope + scopedMockDb serves
			//       the YAML-reloaded Mongo read-through mock repeatedly (including
			//       across ephemeral pod IP changes on the same port), observes once
			//       per hit, records ReadThrough=true + Usage=Updated, never
			//       increments setServed, and rejects wrong-entity / wrong-port /
			//       write queries fail-closed.
			call := integrations.MarkCallRound(mm)
			held, restaged := integrations.HoldToRound(mm, call)
			if restaged() {
				t.Fatal("freshly held round must not be restaged")
			}
			scoped := &scopedMockDb{MockMemDb: held}
			startupMongo, err := scoped.GetStartupMocksByKind(models.Mongo)
			if err != nil {
				t.Fatalf("GetStartupMocksByKind(Mongo) via heldScope+scopedMockDb: %v", err)
			}
			for _, sm := range startupMongo {
				if sm.IsReadThrough() {
					t.Fatalf("Mongo read-through mock %s leaked onto GetStartupMocksByKind(Mongo) (KEPLOY_PROVEN=%q)", sm.Name, proven)
				}
			}
			startupHTTP, err := scoped.GetStartupMocksByKind(models.HTTP)
			if err != nil {
				t.Fatalf("GetStartupMocksByKind(HTTP): %v", err)
			}
			for _, sm := range startupHTTP {
				if sm.IsReadThrough() {
					t.Fatalf("HTTP read-through mock %s leaked onto GetStartupMocksByKind(HTTP)", sm.Name)
				}
			}
			liveMongo := mongoFindMock("", "svc-alpha", 1, "owner.id", "", time.Time{})
			liveMongo.Spec.Metadata = map[string]string{"destPort": "27017", "destAddr": "127.0.0.1:27017"}
			beforeObs := observed.Load()
			gotMongo1, ok1 := integrations.MatchReadThrough(scoped, liveMongo)
			if !ok1 || gotMongo1 == nil {
				t.Fatalf("exact MatchReadThrough for YAML-reloaded Mongo mock failed in KEPLOY_PROVEN=%q", proven)
			}
			if gotObs := observed.Load() - beforeObs; gotObs != 1 {
				t.Fatalf("expected 1 observeServed on MatchReadThrough, got %d", gotObs)
			}
			gotMongo2, ok2 := integrations.MatchReadThrough(scoped, liveMongo)
			if !ok2 || gotMongo2 == nil || gotMongo2.Name != gotMongo1.Name {
				t.Fatalf("second MatchReadThrough must reuse YAML-reloaded Mongo mock in KEPLOY_PROVEN=%q", proven)
			}
			// Wrong-entity and wrong-port Mongo calls must fail closed.
			wrongEntity := mongoFindMock("", "svc-other", 1, "owner.id", "", time.Time{})
			wrongEntity.Spec.Metadata = map[string]string{"destPort": "27017"}
			if m, ok := integrations.MatchReadThrough(scoped, wrongEntity); ok || m != nil {
				t.Fatalf("wrong-entity Mongo call must fail closed in KEPLOY_PROVEN=%q, got %v", proven, m)
			}
			wrongPort := liveMongo.DeepCopy()
			wrongPort.Spec.Metadata = map[string]string{"destPort": "27018"}
			if m, ok := integrations.MatchReadThrough(scoped, wrongPort); ok || m != nil {
				t.Fatalf("wrong-port Mongo call must fail closed in KEPLOY_PROVEN=%q, got %v", proven, m)
			}
			consumed := mm.GetConsumedMocks()
			var sawRTState bool
			for _, st := range consumed {
				if st.Name == gotMongo1.Name {
					sawRTState = true
					if !st.ReadThrough || st.Usage != models.Updated {
						t.Fatalf("Mongo read-through MockState = %+v, want ReadThrough=true Usage=Updated", st)
					}
				}
			}
			if !sawRTState {
				t.Fatalf("Mongo read-through mock %s missing from GetConsumedMocks()", gotMongo1.Name)
			}
			mm.consumedMu.Lock()
			if got := mm.setServed[gotMongo1.Name]; got != 0 {
				mm.consumedMu.Unlock()
				t.Fatalf("Mongo read-through MatchReadThrough must not increment setServed, got %d", got)
			}
			mm.consumedMu.Unlock()

			// 3b. Origin-form HTTPS read-through mock persisted to YAML and reloaded
			//     must match live TLS replay via heldScope+scopedMockDb MatchReadThrough
			//     and miss plaintext HTTP replay.
			liveHTTPS := &models.Mock{
				Kind: models.HTTP,
				Spec: models.MockSpec{
					Metadata: map[string]string{"tls": "true"},
					HTTPReq: &models.HTTPReq{
						Method: models.Method("GET"),
						URL:    "/v1/jwks?tenant=secure",
						Header: map[string]string{"Host": "auth.internal", "Accept": "application/json"},
					},
				},
			}
			servedHTTPS, hitHTTPS := integrations.MatchReadThrough(scoped, liveHTTPS)
			if !hitHTTPS || servedHTTPS == nil || servedHTTPS.Spec.HTTPResp == nil || servedHTTPS.Spec.HTTPResp.Body != `{"source":"https-read-through"}` {
				t.Fatalf("cold replay missed YAML-reloaded origin-form HTTPS read-through mock in KEPLOY_PROVEN=%q: hit=%v mock=%+v", proven, hitHTTPS, servedHTTPS)
			}
			livePlainHTTP := &models.Mock{
				Kind: models.HTTP,
				Spec: models.MockSpec{
					HTTPReq: &models.HTTPReq{
						Method: models.Method("GET"),
						URL:    "/v1/jwks?tenant=secure",
						Header: map[string]string{"Host": "auth.internal", "Accept": "application/json"},
					},
				},
			}
			if _, ok := integrations.MatchReadThrough(scoped, livePlainHTTP); ok {
				t.Fatal("plaintext HTTP replay MUST miss YAML-reloaded HTTPS read-through mock")
			}

			// 3c. Restaging the next set (BaseTime initial staging after ResetForReplaySession
			//     + ArmSetFence) bumps stagingEpoch, clears readThrough, and invalidates heldScope.
			mm.ResetForReplaySession()
			mm.ArmSetFence()
			mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Time{})
			if afterMove, err := held.GetStartupMocksByKind(models.Mongo); err != nil || len(afterMove) != 0 {
				t.Fatalf("old heldScope must return empty startup mocks after restage, got len=%d err=%v", len(afterMove), err)
			}
			if _, ok := integrations.MatchReadThrough(held, liveHTTPS); ok {
				t.Fatal("old heldScope must refuse MatchReadThrough after restage")
			}
			if proven == "1" && !restaged() {
				t.Fatal("expected restaged() == true after reading old heldScope post-SetMocksWithWindow")
			}
		})
	}
}
