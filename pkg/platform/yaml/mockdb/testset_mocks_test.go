package mockdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

// GetTestSetMocks reads a test set's mock file once for every pool a replay
// loads. It replaces GetFilteredMocks + GetUnFilteredMocks + GetUnFilteredMocks
// with no mapping maps, three reads of the whole file, so it must return what
// they returned, pool by pool: the same mocks, in the same order, pruned the
// same way, from every format and every kind of file. The single-pool readers
// are now views of the same pass, so agreeing with them is not enough; the
// pools are also pinned to what the two-pass readers returned (wantPools).

// testSetMocksCorpus covers each routing the readers perform: per-test,
// session (tagged, and untagged through the legacy kind fallback), connection,
// session mocks without timestamps (ordered apart by FilterConfigMocks, and
// kept by the per-test window filter), and PostgresV2 mocks, which the gob
// reader puts in both pools. InsertMock names them mock-0, mock-1, ... in
// this order.
func testSetMocksCorpus() []*models.Mock {
	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	stamp := func(m *models.Mock, sec int) *models.Mock {
		if sec >= 0 {
			m.Spec.ReqTimestampMock = base.Add(time.Duration(sec) * time.Second)
			m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(50 * time.Millisecond)
		}
		return m
	}
	meta := func(kv ...string) map[string]string {
		md := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			md[kv[i]] = kv[i+1]
		}
		return md
	}
	httpMock := func(url string, md map[string]string, sec int) *models.Mock {
		return stamp(&models.Mock{
			Version: models.GetVersion(), Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata: md,
				HTTPReq:  &models.HTTPReq{Method: "GET", URL: url, ProtoMajor: 1, ProtoMinor: 1, Header: map[string]string{"Accept": "*/*"}},
				HTTPResp: &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Header: map[string]string{"Content-Type": "application/json"}, Body: `{"ok":true}`},
			},
		}, sec)
	}
	mongoMock := func(find string, md map[string]string, sec int) *models.Mock {
		return stamp(&models.Mock{
			Version: models.GetVersion(), Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata: md,
				MongoRequests: []models.MongoRequest{{
					Header:  &models.MongoHeader{Length: 50, RequestID: 1, Opcode: wiremessage.OpMsg},
					Message: &models.MongoOpMessage{Sections: []string{fmt.Sprintf(`{ SectionSingle msg: {"find":%q} }`, find)}},
				}},
				MongoResponses: []models.MongoResponse{{
					Header:  &models.MongoHeader{Length: 60, RequestID: 2, ResponseTo: 1, Opcode: wiremessage.OpMsg},
					Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":1} }`}},
				}},
			},
		}, sec)
	}
	postgresMock := func(query string, md map[string]string, sec int) *models.Mock {
		return stamp(&models.Mock{
			Version: models.GetVersion(), Kind: models.PostgresV2,
			Spec: models.MockSpec{
				Metadata: md,
				PostgresRequestsV2: []postgres.Request{{PacketBundle: postgres.PacketBundle{
					Packets: []postgres.Packet{{
						Header:  &postgres.PacketInfo{Type: "Query", Header: &postgres.Header{PayloadLength: 9, PacketID: "Q"}},
						Message: map[string]interface{}{"query": query},
					}},
				}}},
			},
		}, sec)
	}
	return []*models.Mock{
		httpMock("http://svc/config", meta("type", "config"), 5),                   // mock-0: session
		mongoMock("orders", meta("type", "mocks"), 1),                              // mock-1: per-test
		httpMock("http://svc/untagged", nil, 3),                                    // mock-2: session (kind fallback)
		httpMock("http://svc/conn", meta("type", "connection", "connID", "c1"), 2), // mock-3: connection
		httpMock("http://svc/no-timestamps", meta("type", "config"), -1),           // mock-4: session, no timestamps
		mongoMock("customers", meta("type", "mocks"), 4),                           // mock-5: per-test
		mongoMock("handshake", meta("type", "connection", "connID", "c2"), 0),      // mock-6: connection
		postgresMock("SELECT 1", meta("type", "config"), 7),                        // mock-7: session
		postgresMock("SELECT 2", meta("type", "mocks"), 6),                         // mock-8: session (lax kind fallback)
		postgresMock("SELECT 3", meta("type", "config"), -1),                       // mock-9: session, no timestamps
	}
}

// writeTestSetMocks records the corpus into set-0 in the given format, then
// two more mocks named after earlier ones, as a recording appended to with a
// reset counter carries.
func writeTestSetMocks(t *testing.T, dir string, format string) *MockYaml {
	t.Helper()
	var ys *MockYaml
	switch format {
	case "gob":
		ys = New(zap.NewNop(), dir, "mocks")
		ys.MockFormat = mockFormatGob
	case "json":
		ys = NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON)
	default:
		ys = New(zap.NewNop(), dir, "mocks")
	}
	for _, m := range testSetMocksCorpus() {
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatalf("InsertMock(%s, %s): %v", format, m.Kind, err)
		}
	}
	ys.ResetCounterID()
	again := testSetMocksCorpus()[:2]
	again[0].Spec.HTTPReq.URL = "http://svc/config-again"
	again[1].Spec.ReqTimestampMock = again[1].Spec.ReqTimestampMock.Add(time.Hour)
	again[1].Spec.ResTimestampMock = again[1].Spec.ResTimestampMock.Add(time.Hour)
	for _, m := range again {
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatalf("InsertMock(%s): %v", format, err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return ys
}

// poolString renders a pool in order as name:kind/lifetime, with "/f" for a
// mock the window filter marked filtered; nil and empty differ.
func poolString(mocks []*models.Mock) string {
	if mocks == nil {
		return "<nil>"
	}
	parts := make([]string, 0, len(mocks))
	for _, m := range mocks {
		p := fmt.Sprintf("%s:%s/%s", m.Name, m.Kind, m.TestModeInfo.Lifetime)
		if m.TestModeInfo.IsFiltered {
			p += "/f"
		}
		parts = append(parts, p)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

type testSetMocksCase struct {
	window, maps   string
	after, before  time.Time
	mapped, needed map[string]bool
	prunes         bool
}

func testSetMocksCases() []testSetMocksCase {
	// mock-0 and mock-2 are session mocks, and mock-7 a PostgresV2 one, mapped
	// to a test this run does not need, mock-5 a per-test one; mock-1 is
	// mapped and needed.
	pruneMapped := map[string]bool{"mock-0": true, "mock-1": true, "mock-2": true, "mock-5": true, "mock-7": true}
	pruneNeeded := map[string]bool{"mock-1": true}
	every := map[string]bool{}
	for i := 0; i < 10; i++ {
		every[fmt.Sprintf("mock-%d", i)] = true
	}
	var cases []testSetMocksCase
	for _, w := range []struct {
		name          string
		after, before time.Time
	}{
		{"replay window", models.BaseTime, time.Now()},
		{"no window", time.Time{}, time.Time{}},
	} {
		for _, m := range []struct {
			name           string
			mapped, needed map[string]bool
			prunes         bool
		}{
			{"no mappings", nil, nil, false},
			{"mapping prune", pruneMapped, pruneNeeded, true},
			{"every mock needed", every, every, false},
		} {
			cases = append(cases, testSetMocksCase{w.name, m.name, w.after, w.before, m.mapped, m.needed, m.prunes})
		}
	}
	return cases
}

// wantPools is what the two-pass readers this change replaced (HEAD
// 013d0dce4's GetFilteredMocks, GetUnFilteredMocks, and GetUnFilteredMocks
// with no mapping maps) returned for the corpus: format -> window -> mapping
// -> {Filtered, Unfiltered, AllSession}.
var wantPools = map[string]map[string]map[string][3]string{
	"yaml": {
		"replay window": {
			"no mappings": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"mapping prune": {
				"[mock-1:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-8:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"every mock needed": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
		},
		"no window": {
			"no mappings": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"mapping prune": {
				"[mock-1:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-8:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"every mock needed": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
		},
	},
	"json": {
		"replay window": {
			"no mappings": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"mapping prune": {
				"[mock-1:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-8:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"every mock needed": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
		},
		"no window": {
			"no mappings": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"mapping prune": {
				"[mock-1:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-8:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"every mock needed": {
				"[mock-1:Mongo/per-test mock-5:Mongo/per-test mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
		},
	},
	"gob": {
		"replay window": {
			"no mappings": {
				"[mock-9:PostgresV2/session/f mock-1:Mongo/per-test/f mock-5:Mongo/per-test/f mock-1:Mongo/per-test/f]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"mapping prune": {
				"[mock-9:PostgresV2/session/f mock-1:Mongo/per-test/f mock-1:Mongo/per-test/f]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-8:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"every mock needed": {
				"[mock-9:PostgresV2/session/f mock-1:Mongo/per-test/f mock-5:Mongo/per-test/f mock-1:Mongo/per-test/f]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session/f mock-9:PostgresV2/session/f mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
		},
		"no window": {
			"no mappings": {
				"[mock-9:PostgresV2/session mock-1:Mongo/per-test mock-5:Mongo/per-test mock-8:PostgresV2/session mock-7:PostgresV2/session mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"mapping prune": {
				"[mock-9:PostgresV2/session mock-1:Mongo/per-test mock-8:PostgresV2/session mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-8:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
			"every mock needed": {
				"[mock-9:PostgresV2/session mock-1:Mongo/per-test mock-5:Mongo/per-test mock-8:PostgresV2/session mock-7:PostgresV2/session mock-1:Mongo/per-test]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
				"[mock-4:Http/session mock-9:PostgresV2/session mock-6:Mongo/connection mock-3:Http/connection mock-2:Http/session mock-0:Http/session mock-0:Http/session mock-8:PostgresV2/session mock-7:PostgresV2/session]",
			},
		},
	},
}

// readersAgree asserts that GetTestSetMocks returns, pool by pool, what the
// single-pool readers return for the same arguments, and returns its result.
func readersAgree(t *testing.T, ys *MockYaml, c testSetMocksCase) models.TestSetMocks {
	t.Helper()
	ctx := context.Background()
	got, err := ys.GetTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
	if err != nil {
		t.Fatalf("GetTestSetMocks: %v", err)
	}
	filtered, err := ys.GetFilteredMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
	if err != nil {
		t.Fatalf("GetFilteredMocks: %v", err)
	}
	unfiltered, err := ys.GetUnFilteredMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
	if err != nil {
		t.Fatalf("GetUnFilteredMocks: %v", err)
	}
	allSession, err := ys.GetUnFilteredMocks(ctx, "set-0", c.after, c.before, nil, nil)
	if err != nil {
		t.Fatalf("GetUnFilteredMocks(nil, nil): %v", err)
	}
	// No window filters nothing, so the per-test reader with no window and
	// no mapping maps returns every per-test candidate, sorted by request
	// time; AllPerTest keeps them in file order.
	allPerTest, err := ys.GetFilteredMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("GetFilteredMocks(no window, nil, nil): %v", err)
	}
	if g, w := sortedPoolString(got.AllPerTest), sortedPoolString(allPerTest); g != w {
		t.Errorf("AllPerTest is not every per-test candidate:\n got %s\nwant %s", g, w)
	}
	for _, pool := range []struct {
		name      string
		got, want []*models.Mock
	}{
		{"Filtered", got.Filtered, filtered},
		{"Unfiltered", got.Unfiltered, unfiltered},
		{"AllSession", got.AllSession, allSession},
	} {
		if !reflect.DeepEqual(pool.got, pool.want) {
			t.Errorf("%s differs from the single-pool read:\n got %s\nwant %s", pool.name, poolString(pool.got), poolString(pool.want))
		}
	}
	return got
}

func TestGetTestSetMocksMatchesTheSinglePoolReaders(t *testing.T) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW"))) {
	case "1", "true", "yes", "on":
		// It turns off the lax kind fallback, so mock-8 is per-test, and it
		// is read once at start-up, so it cannot be unset here.
		t.Skip("wantPools is lax routing; KEPLOY_STRICT_MOCK_WINDOW is on")
	}
	t.Setenv("KEPLOY_MOCK_FORMAT", "") // it would override the format each subtest writes
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), format)
			for _, c := range testSetMocksCases() {
				t.Run(c.window+"/"+c.maps, func(t *testing.T) {
					got := readersAgree(t, ys, c)
					want := wantPools[format][c.window][c.maps]
					for i, pool := range [][]*models.Mock{got.Filtered, got.Unfiltered, got.AllSession} {
						if s := poolString(pool); s != want[i] {
							t.Errorf("%s is not what the two-pass readers returned:\n got %s\nwant %s",
								[]string{"Filtered", "Unfiltered", "AllSession"}[i], s, want[i])
						}
					}
					if c.prunes != (len(got.AllSession) > len(got.Unfiltered)) {
						t.Fatalf("the prune should drop session mocks: %v; AllSession %s, Unfiltered %s", c.prunes, poolString(got.AllSession), poolString(got.Unfiltered))
					}
					if format == "gob" {
						// The window filter keeps a session PostgresV2 mock in
						// the per-test pool only without a window or without
						// timestamps (mock-9), so in every case some mock is
						// in both pools.
						poolsShareNoMock(t, got)
					}
				})
			}
		})
	}
}

// sortedPoolString is poolString of the pool's mocks sorted by name, kind and
// URL or request time, for comparing pools whose order differs.
func sortedPoolString(pool []*models.Mock) string {
	sorted := append([]*models.Mock(nil), pool...)
	key := func(m *models.Mock) string {
		k := m.Name + "/" + string(m.Kind) + "/" + m.Spec.ReqTimestampMock.String()
		if m.Spec.HTTPReq != nil {
			k += "/" + m.Spec.HTTPReq.URL
		}
		return k
	}
	sort.SliceStable(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	return poolString(sorted)
}

// poolsShareNoMock: the gob reader puts PostgresV2 mocks in both pools. Read
// separately, each pool had its own; read once, each pool must still get its
// own, down to the payloads, so a change to one pool's mock, as a MockMutator
// makes in place, cannot reach the other's.
func poolsShareNoMock(t *testing.T, got models.TestSetMocks) {
	t.Helper()
	inBoth := 0
	for _, f := range got.Filtered {
		for _, u := range got.Unfiltered {
			if f == u {
				t.Fatalf("%s is the same *Mock in both pools", f.Name)
			}
			if f.Name != u.Name || f.Kind != models.PostgresV2 {
				continue
			}
			inBoth++
			fb, ub := f.Spec.PostgresRequestsV2[0].PacketBundle, u.Spec.PostgresRequestsV2[0].PacketBundle
			if &fb.Packets[0] == &ub.Packets[0] || fb.Packets[0].Header == ub.Packets[0].Header ||
				reflect.ValueOf(fb.Packets[0].Message).Pointer() == reflect.ValueOf(ub.Packets[0].Message).Pointer() {
				t.Fatalf("%s shares its request payload between the pools", f.Name)
			}
			if reflect.ValueOf(f.Spec.Metadata).Pointer() == reflect.ValueOf(u.Spec.Metadata).Pointer() {
				t.Fatalf("%s shares its metadata between the pools", f.Name)
			}
		}
	}
	if inBoth == 0 {
		t.Fatalf("no PostgresV2 mock is in both pools, so the copy is untested: %s / %s", poolString(got.Filtered), poolString(got.Unfiltered))
	}
}

// A mock file whose tail does not parse fails the read, with the same error
// whichever reader meets it.
func TestGetTestSetMocksFailsACorruptTailAsTheSinglePoolReadersDo(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	dir := t.TempDir()
	ys := writeTestSetMocks(t, dir, "yaml")
	f, err := os.OpenFile(filepath.Join(dir, "set-0", "mocks.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("---\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-9\nspec:\n    metadata: {type: [\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	set, errSet := ys.GetTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
	_, errFiltered := ys.GetFilteredMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
	_, errUnfiltered := ys.GetUnFilteredMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
	if errSet == nil || errFiltered == nil || errUnfiltered == nil {
		t.Fatalf("a corrupt tail must fail every read: GetTestSetMocks %v, GetFilteredMocks %v, GetUnFilteredMocks %v", errSet, errFiltered, errUnfiltered)
	}
	if errSet.Error() != errFiltered.Error() || errSet.Error() != errUnfiltered.Error() {
		t.Fatalf("the readers fail differently:\n GetTestSetMocks:    %v\n GetFilteredMocks:   %v\n GetUnFilteredMocks: %v", errSet, errFiltered, errUnfiltered)
	}
	if !reflect.DeepEqual(set, models.TestSetMocks{}) {
		t.Fatalf("a failed read returned mocks: %s %s %s", poolString(set.Filtered), poolString(set.Unfiltered), poolString(set.AllSession))
	}
}

// A mock file cut off mid-write ends inside a document, and YAML cannot tell
// most such documents from complete ones. One that stops after a packet's
// "- header:" line holds a packet with no header, which the MySQL and Mongo
// decoders dereferenced: the replay's read panicked. Cut at every line, a real
// MySQL recording and a file of every kind the corpus writes (Mongo among
// them) must never panic GetTestSetMocks. Cut after a packet's "- header:",
// every reader of the file fails it as a corrupt tail is failed, naming the
// file and the mock. Cut at a document boundary, the file reads as the whole
// documents before the cut. The other cuts are only held to not panicking:
// the format has no end-of-document marker, so most of them decode into a
// partial mock, and nothing here asserts that they should.
func TestReadersFailAMockFileCutMidDocumentInsteadOfPanicking(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	mysqlRecording, err := os.ReadFile(filepath.Join("testdata", "mysql_capture_starved_mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	corpusDir := t.TempDir()
	writeTestSetMocks(t, corpusDir, "yaml")
	corpus, err := os.ReadFile(filepath.Join(corpusDir, "set-0", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		data []byte
		// headerCuts is how many cuts end after a packet's "- header:".
		headerCuts int
	}{
		{name: "mysql recording", data: mysqlRecording, headerCuts: 8},
		{name: "every kind", data: corpus, headerCuts: 8},
	} {
		t.Run(file.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "set-0"), 0o755); err != nil {
				t.Fatal(err)
			}
			mockFile := filepath.Join(dir, "set-0", "mocks.yaml")
			ys := New(zap.NewNop(), dir, "mocks")
			ctx := context.Background()
			lines := strings.SplitAfter(string(file.data), "\n")
			// complete holds the names of the documents a "---" has closed;
			// current is the name of the open document, "" between documents.
			var complete []string
			current, inDoc := "", false
			cut, headerCuts, boundaryCuts := 0, 0, 0
			for n := 0; n <= len(lines); n++ {
				last := ""
				if n > 0 {
					last = lines[n-1]
					cut += len(last)
					switch {
					case last == "---\n":
						complete = append(complete, current)
						current, inDoc = "", false
					case strings.HasPrefix(last, "#") && !inDoc:
					default:
						inDoc = true
						if strings.HasPrefix(last, "name: ") {
							current = strings.TrimSpace(strings.TrimPrefix(last, "name: "))
						}
					}
				}
				if err := os.WriteFile(mockFile, file.data[:cut], 0o644); err != nil {
					t.Fatal(err)
				}
				var got models.TestSetMocks
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("cut after line %d (%q), inside %s: GetTestSetMocks panicked: %v", n, last, current, r)
						}
					}()
					got, err = ys.GetTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
				}()

				atBoundary := !inDoc || n == len(lines) || lines[n] == "---\n"
				switch {
				case last == "        - header:\n":
					headerCuts++
					if !errors.Is(err, errPacketWithoutHeader) {
						t.Fatalf("cut after line %d, inside %s: want a packet-without-header error, got %v", n, current, err)
					}
					for _, want := range []string{mockFile, fmt.Sprintf("mock %q", current)} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("cut after line %d: the error does not name %s: %v", n, want, err)
						}
					}
					if !reflect.DeepEqual(got, models.TestSetMocks{}) {
						t.Fatalf("cut after line %d: a failed read returned mocks: %s %s %s", n, poolString(got.Filtered), poolString(got.Unfiltered), poolString(got.AllSession))
					}
					_, errFiltered := ys.GetFilteredMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
					_, errUnfiltered := ys.GetUnFilteredMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
					if errFiltered == nil || errUnfiltered == nil || errFiltered.Error() != err.Error() || errUnfiltered.Error() != err.Error() {
						t.Fatalf("cut after line %d: the readers fail differently:\n GetTestSetMocks:    %v\n GetFilteredMocks:   %v\n GetUnFilteredMocks: %v", n, err, errFiltered, errUnfiltered)
					}
					// The prune and the noise persistence decode the whole file
					// too; neither may panic or rewrite it.
					errPrune := ys.UpdateMocks(ctx, "set-0", map[string]models.MockState{}, time.Now(), time.Time{})
					errNoise := ys.PersistMockNoise(ctx, "set-0", map[string]models.MockState{"mock-0": {ReqBodyNoise: map[string][]string{"body": {"id"}}}})
					for _, e := range []error{errPrune, errNoise} {
						if !errors.Is(e, errPacketWithoutHeader) || !strings.Contains(e.Error(), mockFile) || !strings.Contains(e.Error(), fmt.Sprintf("mock %q", current)) {
							t.Fatalf("cut after line %d: want the prune and the noise persistence to fail naming %s and %s, got %v and %v", n, mockFile, current, errPrune, errNoise)
						}
					}
					if onDisk, err := os.ReadFile(mockFile); err != nil || string(onDisk) != string(file.data[:cut]) {
						t.Fatalf("cut after line %d: a failed read rewrote the file (err %v)", n, err)
					}
				case atBoundary:
					boundaryCuts++
					if err != nil {
						t.Fatalf("cut after line %d, at a document boundary: %v", n, err)
					}
					want := append([]string(nil), complete...)
					if inDoc {
						want = append(want, current)
					}
					var names []string
					for _, m := range append(append([]*models.Mock(nil), got.Filtered...), got.AllSession...) {
						names = append(names, m.Name)
					}
					sort.Strings(want)
					sort.Strings(names)
					if strings.Join(names, " ") != strings.Join(want, " ") {
						t.Fatalf("cut after line %d, at a document boundary: want the mocks %v, got %v", n, want, names)
					}
				}
			}
			if headerCuts != file.headerCuts || boundaryCuts < 3 {
				t.Fatalf("want %d cuts after a packet's header line and a few at document boundaries, got %d and %d", file.headerCuts, headerCuts, boundaryCuts)
			}
		})
	}
}

// A file with no documents is zero mocks, not an error: the per-test pool is
// nil and the session pools empty, as the single-pool readers return them.
// No file at all is zero mocks too.
func TestGetTestSetMocksReadsAnEmptyOrMissingFileAsTheSinglePoolReadersDo(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	all := testSetMocksCase{after: models.BaseTime, before: time.Now()}
	t.Run("header only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "set-0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "set-0", "mocks.yaml"), []byte("# Generated by Keploy (3-dev)\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := readersAgree(t, New(zap.NewNop(), dir, "mocks"), all)
		if got.Filtered != nil || got.Unfiltered == nil || len(got.Unfiltered) != 0 {
			t.Fatalf("want a nil per-test pool and an empty session pool, got %s and %s", poolString(got.Filtered), poolString(got.Unfiltered))
		}
	})
	t.Run("no file", func(t *testing.T) {
		got := readersAgree(t, New(zap.NewNop(), t.TempDir(), "mocks"), all)
		if len(got.Filtered)+len(got.Unfiltered)+len(got.AllSession) != 0 {
			t.Fatalf("want no mocks, got %s %s %s", poolString(got.Filtered), poolString(got.Unfiltered), poolString(got.AllSession))
		}
	})
}
