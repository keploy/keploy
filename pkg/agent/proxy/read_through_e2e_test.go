package proxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpitg "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestE2E_SampledOutHTTPSCacheWarmingRecordToColdReplay exercises an
// in-process record-to-cold-replay flow over net.Pipe, httptest.Server, and
// on-disk YAML:
//  1. http.RecordOutgoing (with TLS DstCfg) records a cache-warming GET
//     /v1/jwks during a sampled-out request (window not kept) and a per-test
//     GET /v1/item during a kept request, both routed through SyncMockManager.
//  2. Emitted mocks are persisted to disk via mockdb.New and reloaded via
//     GetTestSetMocks.
//  3. Reloaded mocks are staged into a real MockManager (both KEPLOY_PROVEN=""
//     and KEPLOY_PROVEN=1) and a cold replay drives HTTP/1.1 requests through
//     http.MockOutgoing (decodeHTTP) over net.Pipe with TLS DstCfg:
//     - GET /v1/jwks succeeds from the read-through tier across two cold tests;
//     - GET /v1/item succeeds from the kept per-test pool;
//     - Plaintext HTTP GET /v1/jwks and POST /v1/jwks both miss (502).
func TestE2E_SampledOutHTTPSCacheWarmingRecordToColdReplay(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			logger := zap.NewNop()
			ctx := context.Background()

			// 1. Set up SyncMockManager past the startup window so sampled-out
			//    requests drop their normal per-test mocks and rescue reads.
			mgr := syncMock.New(logger)
			mgr.StartRecordSession()
			outCh := make(chan *models.Mock, 64)
			mapCh := make(chan models.TestMockMapping, 64)
			mgr.SetOutputChannel(outCh)
			mgr.SetMappingChannel(ctx, mapCh)
			mgr.SetFirstRequestSignaled()
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				tBoot := time.Now().Add(-2 * time.Minute).Add(time.Duration(i) * time.Second)
				mgr.ResolveRange(tBoot, tBoot.Add(10*time.Millisecond), fmt.Sprintf("boot-%d", i), true, true)
			}

			// Upstream HTTP server standing behind MITM TLS recording.
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/jwks":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"keys":["k-warmed"]}`))
				case "/v1/item":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"item":"kept-1"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			upAddr := strings.TrimPrefix(upstream.URL, "http://")

			httpParser := httpitg.New(logger)
			tlsDst := &models.ConditionalDstCfg{
				Addr:   upAddr,
				Port:   443,
				TLSCfg: &tls.Config{MinVersion: tls.VersionTLS12},
			}

			recordOne := func(rawReq string, connID string) {
				t.Helper()
				clientEnd, proxyClient := net.Pipe()
				destConn, err := net.Dial("tcp", upAddr)
				if err != nil {
					t.Fatalf("dial upstream: %v", err)
				}
				recCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				g, gctx := errgroup.WithContext(recCtx)
				gctx = context.WithValue(gctx, models.ErrGroupKey, g)
				gctx = context.WithValue(gctx, models.ClientConnectionIDKey, connID)
				gctx = syncMock.NewContext(gctx, mgr)

				done := make(chan error, 1)
				go func() {
					done <- httpParser.RecordOutgoing(gctx, &integrations.RecordSession{
						Ingress:      proxyClient,
						Egress:       destConn,
						Mocks:        make(chan *models.Mock, 4),
						ErrGroup:     g,
						Logger:       logger,
						ClientConnID: connID,
						Opts: models.OutgoingOptions{
							DstCfg: tlsDst,
						},
					})
				}()
				if _, err := clientEnd.Write([]byte(rawReq)); err != nil {
					t.Fatalf("write record req: %v", err)
				}
				resp, err := http.ReadResponse(bufio.NewReader(clientEnd), nil)
				if err != nil {
					t.Fatalf("read record resp: %v", err)
				}
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				_ = clientEnd.Close()
				_ = proxyClient.Close()
				_ = destConn.Close()
				<-done
				_ = g.Wait()
			}

			// Request 1 (sampled-out): warms cache with GET /v1/jwks.
			warmStart := time.Now().Add(-5 * time.Millisecond)
			recordOne("GET /v1/jwks HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\nConnection: close\r\n\r\n", "conn-warm")
			warmEnd := time.Now().Add(5 * time.Millisecond)
			mgr.ResolveRange(warmStart, warmEnd, "test-sampled-out", false, true)

			// Wait briefly so the kept test window is strictly after the sampled-out window.
			time.Sleep(15 * time.Millisecond)

			// Request 2 (kept): cache already warm, only calls GET /v1/item.
			keptStart := time.Now().Add(-2 * time.Millisecond)
			recordOne("GET /v1/item HTTP/1.1\r\nHost: api.internal\r\nAccept: application/json\r\nConnection: close\r\n\r\n", "conn-kept")
			keptEnd := time.Now().Add(5 * time.Millisecond)
			mgr.ResolveRange(keptStart, keptEnd, "test-kept-1", true, true)
			mgr.CloseOutChan()

			// 2. Persist emitted mocks to YAML on disk and reload via mockdb.
			dir := t.TempDir()
			diskDB := mockdb.New(logger, dir, "mocks")
			for mk := range outCh {
				if err := diskDB.InsertMock(ctx, mk, "test-set-e2e"); err != nil {
					t.Fatalf("InsertMock(%s): %v", mk.Name, err)
				}
			}
			reloaded, err := diskDB.GetTestSetMocks(ctx, "test-set-e2e", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks: %v", err)
			}
			loaded := append(reloaded.Filtered, reloaded.Unfiltered...)
			var keptMock *models.Mock
			var hasRT bool
			for _, mk := range loaded {
				if mk.IsReadThrough() {
					hasRT = true
				} else if mk.Kind == models.HTTP && mk.Spec.HTTPReq != nil && mk.Spec.HTTPReq.URL == "/v1/item" {
					keptMock = mk
				}
			}
			if !hasRT || keptMock == nil {
				t.Fatalf("expected both read-through JWKS and kept /v1/item after reload: hasRT=%v keptMock=%v", hasRT, keptMock)
			}

			// 3. Stage into real MockManager and drive cold replay through http.MockOutgoing.
			mm := proxy.NewMockManager(proxy.NewTreeDb(func(a, b interface{}) int {
				am, bm := a.(*models.Mock), b.(*models.Mock)
				if am.Spec.ReqTimestampMock.Before(bm.Spec.ReqTimestampMock) {
					return -1
				}
				if am.Spec.ReqTimestampMock.After(bm.Spec.ReqTimestampMock) {
					return 1
				}
				if am.Name < bm.Name {
					return -1
				}
				if am.Name > bm.Name {
					return 1
				}
				return 0
			}), proxy.NewTreeDb(func(a, b interface{}) int {
				am, bm := a.(*models.Mock), b.(*models.Mock)
				if am.Name < bm.Name {
					return -1
				}
				if am.Name > bm.Name {
					return 1
				}
				return 0
			}), logger)
			mm.SetMocksWithWindow(loaded, nil, keptStart, keptEnd)
			if proven == "1" {
				mm.ServeExactTest("test-kept-1", []*models.Mock{keptMock})
			}

			replayOne := func(rawReq string, dst *models.ConditionalDstCfg) (int, string) {
				t.Helper()
				clientEnd, proxyClient := net.Pipe()
				repCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					done <- httpParser.MockOutgoing(repCtx, proxyClient, dst, mm, models.OutgoingOptions{DstCfg: dst})
				}()
				if _, err := clientEnd.Write([]byte(rawReq)); err != nil {
					t.Fatalf("write replay req: %v", err)
				}
				resp, err := http.ReadResponse(bufio.NewReader(clientEnd), nil)
				if err != nil {
					t.Fatalf("read replay resp: %v", err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				_ = clientEnd.Close()
				_ = proxyClient.Close()
				<-done
				return resp.StatusCode, string(body)
			}

			// Cold replay #1: GET /v1/jwks over TLS hits read-through tier.
			code1, body1 := replayOne("GET /v1/jwks HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\nConnection: close\r\n\r\n", tlsDst)
			if code1 != 200 || body1 != `{"keys":["k-warmed"]}` {
				t.Fatalf("cold replay #1 GET /v1/jwks = (%d, %q), want (200, keys)", code1, body1)
			}
			// Cold replay #2 (second cold test): read-through is reusable without eviction.
			code2, body2 := replayOne("GET /v1/jwks HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\nConnection: close\r\n\r\n", tlsDst)
			if code2 != 200 || body2 != `{"keys":["k-warmed"]}` {
				t.Fatalf("cold replay #2 GET /v1/jwks = (%d, %q), want (200, keys)", code2, body2)
			}
			// Kept per-test call /v1/item still serves normally.
			codeItem, bodyItem := replayOne("GET /v1/item HTTP/1.1\r\nHost: api.internal\r\nAccept: application/json\r\nConnection: close\r\n\r\n", tlsDst)
			if codeItem != 200 || bodyItem != `{"item":"kept-1"}` {
				t.Fatalf("kept per-test GET /v1/item = (%d, %q), want (200, kept-1)", codeItem, bodyItem)
			}
			// Plaintext HTTP GET /v1/jwks MUST miss TLS-recorded read-through mock (502).
			plainDst := &models.ConditionalDstCfg{Addr: upAddr, Port: 80}
			codePlain, _ := replayOne("GET /v1/jwks HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\nConnection: close\r\n\r\n", plainDst)
			if codePlain != 502 {
				t.Fatalf("plaintext GET /v1/jwks must miss HTTPS read-through mock (502), got %d", codePlain)
			}
			// Mutating POST /v1/jwks MUST miss read-through tier (502).
			codePost, _ := replayOne("POST /v1/jwks HTTP/1.1\r\nHost: auth.internal\r\nAccept: application/json\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", tlsDst)
			if codePost != 502 {
				t.Fatalf("POST /v1/jwks must miss read-through tier (502), got %d", codePost)
			}
		})
	}
}

// TestSampledOutMongoAndMySQLReadThroughYAMLReloadToMatchReadThrough exercises
// full SyncMockManager rescue -> on-disk YAML persistence & reload (mockdb) ->
// MockManager + heldScope MatchReadThrough in BOTH KEPLOY_PROVEN="" and
// KEPLOY_PROVEN=1 for:
//   - MongoDB find, count, distinct, and read-only aggregate (across pod IP
//     change on the same port), while mutating commands, transactional reads,
//     wrong-entity filters, and different ports fail closed and zero
//     read-through mocks enter GetStartupMocks / GetStartupMocksByKind;
//   - MySQL autocommit SELECT (verifying the EOF terminator status flags
//     survive YAML round-trip and cold-replay MatchReadThrough).
func TestSampledOutMongoAndMySQLReadThroughYAMLReloadToMatchReadThrough(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			logger := zap.NewNop()
			ctx := context.Background()

			mgr := syncMock.New(logger)
			mgr.StartRecordSession()
			outCh := make(chan *models.Mock, 64)
			mapCh := make(chan models.TestMockMapping, 64)
			mgr.SetOutputChannel(outCh)
			mgr.SetMappingChannel(ctx, mapCh)
			mgr.SetFirstRequestSignaled()
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				tBoot := time.Now().Add(-2 * time.Minute).Add(time.Duration(i) * time.Second)
				mgr.ResolveRange(tBoot, tBoot.Add(10*time.Millisecond), fmt.Sprintf("boot-%d", i), true, true)
			}

			tWarm := time.Now().Add(-20 * time.Second)
			mkMongo := func(name, reqSec, respSec string, off time.Duration) *models.Mock {
				return &models.Mock{
					Version: models.Version("api.keploy.io/v1beta2"),
					Name:    name,
					Kind:    models.Mongo,
					Spec: models.MockSpec{
						Metadata: map[string]string{"type": "mocks", "destAddr": "192.0.2.10:27017"},
						MongoRequests: []models.MongoRequest{{
							Header:  &models.MongoHeader{Opcode: 2013},
							Message: &models.MongoOpMessage{Sections: []string{reqSec}},
						}},
						MongoResponses: []models.MongoResponse{{
							Header:  &models.MongoHeader{Opcode: 2013},
							Message: &models.MongoOpMessage{Sections: []string{respSec}},
						}},
						ReqTimestampMock: tWarm.Add(off),
						ResTimestampMock: tWarm.Add(off + time.Millisecond),
					},
				}
			}

			const (
				findReq  = `{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"warm-target"},"sort":{"version":1},"lsid":{"id":"rec-1"},"$db":"appdb"} }`
				countReq = `{ SectionSingle msg: {"count":"service_configs","query":{"serviceId":"warm-target"},"$db":"appdb"} }`
				distReq  = `{ SectionSingle msg: {"distinct":"service_configs","key":"owner","query":{"serviceId":"warm-target"},"$db":"appdb"} }`
				aggReq   = `{ SectionSingle msg: {"aggregate":"service_configs","pipeline":[{"$match":{"serviceId":"warm-target"}},{"$project":{"owner":1}}],"cursor":{},"$db":"appdb"} }`
				findResp = `{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"1","owner":"owner-warm-target"}],"id":{"$numberLong":"0"},"ns":"appdb.service_configs"},"ok":1} }`
				cntResp  = `{ SectionSingle msg: {"n":1,"ok":1} }`
				distResp = `{ SectionSingle msg: {"values":["owner-warm-target"],"ok":1} }`
				aggResp  = `{ SectionSingle msg: {"cursor":{"firstBatch":[{"owner":"owner-warm-target"}],"id":{"$numberLong":"0"},"ns":"appdb.service_configs"},"ok":1} }`
			)

			// Sampled-out Mongo reads (eligible) + sampled-out write/txn/out (must be rejected).
			mgr.AddMock(mkMongo("m-find", findReq, findResp, 0))
			mgr.AddMock(mkMongo("m-count", countReq, cntResp, 2*time.Millisecond))
			mgr.AddMock(mkMongo("m-dist", distReq, distResp, 4*time.Millisecond))
			mgr.AddMock(mkMongo("m-agg", aggReq, aggResp, 6*time.Millisecond))
			mgr.AddMock(mkMongo("m-insert", `{ SectionSingle msg: {"insert":"service_configs","documents":[{"_id":"w"}],"$db":"appdb"} }`, `{ SectionSingle msg: {"n":1,"ok":1} }`, 8*time.Millisecond))
			mgr.AddMock(mkMongo("m-out", `{ SectionSingle msg: {"aggregate":"service_configs","pipeline":[{"$out":"archive"}],"cursor":{},"$db":"appdb"} }`, aggResp, 10*time.Millisecond))
			mgr.AddMock(mkMongo("m-txn", `{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"txn-only"},"txnNumber":{"$numberLong":"1"},"$db":"appdb"} }`, findResp, 12*time.Millisecond))

			// Sampled-out MySQL autocommit SELECT (verifies YAML round-trip of EOF terminator).
			eofAutocommit := []byte{0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00}
			sqlMock := &models.Mock{
				Version: models.Version("api.keploy.io/v1beta2"),
				Name:    "m-mysql-select",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata: map[string]string{
						"type":                     "mocks",
						"destAddr":                 "192.0.2.10:3306",
						"destPort":                 "3306",
						"database":                 "appdb",
						models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					},
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 42, SequenceID: 0}, Type: "COM_QUERY"},
							Message: &mysql.QueryPacket{Command: 3, Query: "SELECT owner FROM configs WHERE id = 'warm'"},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "TextResultSet"},
							Message: &mysql.TextResultSet{
								FinalResponse: &mysql.GenericResponse{Data: eofAutocommit, Type: "EOF"},
							},
						},
					}},
					ReqTimestampMock: tWarm.Add(14 * time.Millisecond),
					ResTimestampMock: tWarm.Add(15 * time.Millisecond),
				},
				TestModeInfo: models.TestModeInfo{
					Lifetime:        models.LifetimePerTest,
					LifetimeDerived: true,
				},
			}
			mgr.AddMock(sqlMock)
			mgr.ResolveRange(tWarm.Add(-time.Millisecond), tWarm.Add(20*time.Millisecond), "test-sampled-out", false, true)

			// Kept test window after the sampled-out warm request (owns zero of the warmed mocks).
			keptStart := time.Now().Add(-5 * time.Millisecond)
			keptEnd := time.Now()
			mgr.ResolveRange(keptStart, keptEnd, "test-kept-cold", true, true)
			mgr.CloseOutChan()

			dir := t.TempDir()
			diskDB := mockdb.New(logger, dir, "mocks")
			for mk := range outCh {
				if err := diskDB.InsertMock(ctx, mk, "test-set-mongo-mysql"); err != nil {
					t.Fatalf("InsertMock(%s): %v", mk.Name, err)
				}
			}
			reloaded, err := diskDB.GetTestSetMocks(ctx, "test-set-mongo-mysql", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks: %v", err)
			}
			loaded := append(reloaded.Filtered, reloaded.Unfiltered...)
			var mongoRTCount, mysqlRTCount int
			for _, mk := range loaded {
				if !mk.IsReadThrough() {
					t.Fatalf("unexpected non-read-through mock %q (%s) persisted from sampled-out window", mk.Name, mk.Kind)
				}
				switch mk.Kind {
				case models.Mongo:
					mongoRTCount++
				case models.MySQL:
					mysqlRTCount++
				}
			}
			if mongoRTCount != 4 {
				t.Fatalf("expected exactly 4 Mongo read-through mocks (find/count/distinct/aggregate) after YAML reload, got %d", mongoRTCount)
			}
			if mysqlRTCount != 1 {
				t.Fatalf("expected exactly 1 MySQL read-through mock surviving YAML terminator round-trip, got %d", mysqlRTCount)
			}

			mm := proxy.NewMockManager(proxy.NewTreeDb(func(a, b interface{}) int {
				am, bm := a.(*models.Mock), b.(*models.Mock)
				if am.Spec.ReqTimestampMock.Before(bm.Spec.ReqTimestampMock) {
					return -1
				}
				if am.Spec.ReqTimestampMock.After(bm.Spec.ReqTimestampMock) {
					return 1
				}
				if am.Name < bm.Name {
					return -1
				}
				if am.Name > bm.Name {
					return 1
				}
				return 0
			}), proxy.NewTreeDb(func(a, b interface{}) int {
				am, bm := a.(*models.Mock), b.(*models.Mock)
				if am.Name < bm.Name {
					return -1
				}
				if am.Name > bm.Name {
					return 1
				}
				return 0
			}), logger)
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(loaded, nil, keptStart, keptEnd)
			if proven == "1" {
				mm.ServeExactTest("test-kept-cold", nil)
			}

			// Zero read-through mocks may appear in GetStartupMocks or GetStartupMocksByKind in EITHER mode.
			if st, err := mm.GetStartupMocks(); err != nil || len(st) != 0 {
				t.Fatalf("GetStartupMocks must stay empty for read-through mocks (KEPLOY_PROVEN=%q), got len=%d err=%v", proven, len(st), err)
			}
			if stM, err := mm.GetStartupMocksByKind(models.Mongo); err != nil || len(stM) != 0 {
				t.Fatalf("GetStartupMocksByKind(Mongo) must stay empty (KEPLOY_PROVEN=%q), got len=%d err=%v", proven, len(stM), err)
			}

			call := integrations.MarkCallRound(mm)
			held, _ := integrations.HoldToRound(mm, call)

			// Cold replay on a different pod IP (127.0.0.1:27017) with different volatile lsid hits all 4 Mongo reads.
			for name, reqSec := range map[string]string{
				"find":      `{ SectionSingle msg: {"find":"service_configs","sort":{"version":1},"filter":{"serviceId":"warm-target"},"lsid":{"id":"replay-cold"},"$db":"appdb"} }`,
				"count":     countReq,
				"distinct":  distReq,
				"aggregate": aggReq,
			} {
				live := &models.Mock{
					Kind: models.Mongo,
					Spec: models.MockSpec{
						Metadata: map[string]string{"destPort": "27017", "destAddr": "127.0.0.1:27017"},
						MongoRequests: []models.MongoRequest{{
							Header:  &models.MongoHeader{Opcode: 2013},
							Message: &models.MongoOpMessage{Sections: []string{reqSec}},
						}},
					},
				}
				got1, ok1 := integrations.MatchReadThrough(held, live)
				got2, ok2 := integrations.MatchReadThrough(held, live)
				if !ok1 || !ok2 || got1 == nil || got2 == nil || got1.Name != got2.Name {
					t.Fatalf("cold Mongo %s replay failed (KEPLOY_PROVEN=%q): ok=(%v,%v)", name, proven, ok1, ok2)
				}
			}

			// Wrong-entity Mongo query and different port MUST fail closed.
			for name, live := range map[string]*models.Mock{
				"wrong-entity": {
					Kind: models.Mongo,
					Spec: models.MockSpec{
						Metadata: map[string]string{"destPort": "27017"},
						MongoRequests: []models.MongoRequest{{
							Header:  &models.MongoHeader{Opcode: 2013},
							Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"find":"service_configs","filter":{"serviceId":"other-entity"},"sort":{"version":1},"$db":"appdb"} }`}},
						}},
					},
				},
				"wrong-port": {
					Kind: models.Mongo,
					Spec: models.MockSpec{
						Metadata: map[string]string{"destPort": "27018"},
						MongoRequests: []models.MongoRequest{{
							Header:  &models.MongoHeader{Opcode: 2013},
							Message: &models.MongoOpMessage{Sections: []string{findReq}},
						}},
					},
				},
			} {
				if got, ok := integrations.MatchReadThrough(held, live); ok || got != nil {
					t.Fatalf("Mongo negative case %s must fail closed (KEPLOY_PROVEN=%q), got %v", name, proven, got)
				}
			}

			// YAML-reloaded MySQL read-through mock matches on exact (port, db, query) and rejects wrong db.
			liveSQL := &models.Mock{
				Kind: models.MySQL,
				Spec: models.MockSpec{
					Metadata:      map[string]string{"destPort": "3306", "database": "appdb"},
					MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "SELECT owner FROM configs WHERE id = 'warm'"}}}},
				},
			}
			if gotSQL, okSQL := integrations.MatchReadThrough(held, liveSQL); !okSQL || gotSQL == nil {
				t.Fatalf("YAML-reloaded MySQL read-through mock missed on exact match (KEPLOY_PROVEN=%q)", proven)
			}
			wrongDBSQL := liveSQL.DeepCopy()
			wrongDBSQL.Spec.Metadata["database"] = "other_tenant"
			if gotSQL, okSQL := integrations.MatchReadThrough(held, wrongDBSQL); okSQL || gotSQL != nil {
				t.Fatalf("MySQL wrong-tenant database must fail closed (KEPLOY_PROVEN=%q), got %v", proven, gotSQL)
			}
		})
	}
}
