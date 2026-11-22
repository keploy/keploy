package mockdb

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A replay's mapping names a mock under the test that consumed it, and a run
// of other tests leaves it out. A mock that is every test's by statement is
// kept whichever test's mapping names it: one its recorder stated session (a
// connection's setup), and a call stated per-test that keploy.yaml declares
// shared (sessionCalls). A mock recorded before lifetimes were stated is left
// out as it always was, declared or not.
func TestMappingPruneKeepsWhatIsEveryTestsByStatement(t *testing.T) {
	t.Cleanup(func() { models.SetSessionCalls(nil) })
	base := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	call := func(method, path string, stated string, sec int) *models.Mock {
		md := map[string]string{"type": string(models.HTTPClient)}
		if stated != "" {
			md[models.MetadataLifetimeKey] = stated
		}
		return &models.Mock{
			Version: models.GetVersion(), Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata:         md,
				HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: path, ProtoMajor: 1, ProtoMinor: 1, Header: map[string]string{"Host": "auth.example.com"}},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: `{"ok":true}`},
				ReqTimestampMock: base.Add(time.Duration(sec) * time.Second),
				ResTimestampMock: base.Add(time.Duration(sec)*time.Second + time.Millisecond),
			},
		}
	}
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	for _, m := range []*models.Mock{
		call("POST", "/oauth/token", models.MetadataLifetimePerTest, 1), // mock-0: the token call
		call("GET", "/profile", models.MetadataLifetimePerTest, 2),      // mock-1: the test's own data
		call("GET", "/config", models.MetadataLifetimeSession, 3),       // mock-2: stated session
		call("POST", "/oauth/token", "", 4),                             // mock-3: recorded before
	} {
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatal(err)
		}
	}
	// The mapping names every mock under test A; the run is of test B alone.
	mapped := map[string]bool{"mock-0": true, "mock-1": true, "mock-2": true, "mock-3": true}
	served := func() []string {
		t.Helper()
		pool, err := ys.GetUnFilteredMocks(context.Background(), "set-0", models.BaseTime, base.Add(time.Hour), mapped, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range pool {
			names = append(names, m.Name)
		}
		return names
	}

	if got := served(); len(got) != 1 || got[0] != "mock-2" {
		t.Fatalf("session pool of a run of other tests %v, want the stated session mock mock-2 only", got)
	}
	models.SetSessionCalls([]models.SessionCallRule{{Host: "auth.example.com", Method: "POST", PathPrefix: "/oauth/token"}})
	if got := served(); len(got) != 2 || got[0] != "mock-0" || got[1] != "mock-2" {
		t.Fatalf("with the token call declared, session pool %v, want mock-0 (declared) and mock-2; mock-3, recorded before, left out as always", got)
	}
}

// A MySQL prepared statement's lifecycle (PREPARE, CLOSE, RESET) is every
// test's whenever it was recorded: one recorded before lifetimes were stated
// is its connection's (models.DeriveLifetime), so a run of other tests keeps
// it, as it keeps a stated one. The statement's data (EXECUTE) recorded
// before is left out, as always.
func TestMappingPruneKeepsALegacyPreparedStatementsLifecycle(t *testing.T) {
	legacy := func(command string) *models.Mock {
		return &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{
			Metadata:      map[string]string{"type": "mocks", "connID": "c1"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: command}}}},
		}}
	}
	for _, command := range []string{"COM_STMT_PREPARE", "COM_STMT_CLOSE", "COM_STMT_RESET"} {
		if !sharedByStatement(legacy(command)) {
			t.Errorf("a legacy %s is not kept for a run of other tests", command)
		}
	}
	if sharedByStatement(legacy("COM_STMT_EXECUTE")) {
		t.Error("a legacy EXECUTE, the statement's data, is kept for a run of other tests")
	}
}
