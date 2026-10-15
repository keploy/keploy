package models

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

func mysqlStmtMock(name, connID string, req interface{}, resp interface{}) *Mock {
	m := &Mock{Name: name, Kind: MySQL}
	m.Spec.Metadata = map[string]string{"connID": connID}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{}, Message: req}}}
	if resp != nil {
		m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{}, Message: resp}}}
	}
	return m
}

func TestMySQLDepsLinkAStatementsCommandsToItsPrepare(t *testing.T) {
	prepare := mysqlStmtMock("mock-1", "3", &mysql.StmtPreparePacket{Query: "INSERT INTO t VALUES (?)"}, &mysql.StmtPrepareOkPacket{StatementID: 77})
	execute := mysqlStmtMock("mock-2", "3", &mysql.StmtExecutePacket{StatementID: 77}, &mysql.ERRPacket{})
	closer := mysqlStmtMock("mock-3", "3", &mysql.StmtClosePacket{StatementID: 77}, nil)
	carried := mysqlStmtMock("mock-4", "3", &mysql.StmtExecutePacket{StatementID: 77}, &mysql.OKPacket{})
	carried.Spec.MySQLRequests[0].SetStoredQuery("INSERT INTO t VALUES (?)")
	query := mysqlStmtMock("mock-5", "3", &mysql.QueryPacket{Query: "SELECT 1"}, &mysql.OKPacket{})

	key, closeKey := MySQLStatementKey("3", 77), MySQLStatementCloseKey("3", 77)
	if got := DepsOf(prepare); !reflect.DeepEqual(got, MockDeps{Provides: []string{key}}) {
		t.Errorf("the PREPARE's deps = %+v, want it to provide %q", got, key)
	}
	// The EXECUTE needs its PREPARE to replay; it is kept with its CLOSE.
	want := MockDeps{Requires: []MockRequirement{{Key: key}, {Key: closeKey, Standalone: true}}}
	if got := DepsOf(execute); !reflect.DeepEqual(got, want) {
		t.Errorf("the EXECUTE's deps = %+v, want %+v", got, want)
	}
	want = MockDeps{Requires: []MockRequirement{{Key: key, Standalone: true}, {Key: closeKey, Standalone: true}}}
	if got := DepsOf(carried); !reflect.DeepEqual(got, want) {
		t.Errorf("the EXECUTE that kept its query: deps = %+v, want %+v", got, want)
	}
	want = MockDeps{Provides: []string{closeKey}, Requires: []MockRequirement{{Key: key, Standalone: true}}}
	if got := DepsOf(closer); !reflect.DeepEqual(got, want) {
		t.Errorf("the CLOSE's deps = %+v, want %+v", got, want)
	}
	if got := DepsOf(query); len(got.Provides)+len(got.Requires) != 0 {
		t.Errorf("a COM_QUERY's deps = %+v, want none", got)
	}
	// The same statement ID on another connection is another statement.
	if MySQLStatementKey("3", 77) == MySQLStatementKey("4", 77) || MySQLStatementKey("3", 77) == MySQLStatementKey("3", 78) ||
		MySQLStatementKey("3", 77) == MySQLStatementCloseKey("3", 77) {
		t.Error("statement keys do not tell connections or statements apart")
	}
}

func TestMockDepsCheckFindsTheRequirementsNoMockMeets(t *testing.T) {
	prepare := mysqlStmtMock("mock-1", "0", &mysql.StmtPreparePacket{Query: "SELECT ?"}, &mysql.StmtPrepareOkPacket{StatementID: 1})
	met := mysqlStmtMock("mock-2", "0", &mysql.StmtExecutePacket{StatementID: 1}, &mysql.OKPacket{})
	orphan := mysqlStmtMock("mock-3", "0", &mysql.StmtExecutePacket{StatementID: 2}, &mysql.OKPacket{})
	carried := mysqlStmtMock("mock-4", "0", &mysql.StmtExecutePacket{StatementID: 3}, &mysql.OKPacket{})
	carried.Spec.MySQLRequests[0].SetStoredQuery("SELECT ?")
	// A CLOSE replays without its PREPARE.
	closer := mysqlStmtMock("mock-7", "0", &mysql.StmtClosePacket{StatementID: 5}, nil)
	// A requirer added before its provider is met all the same.
	late := mysqlStmtMock("mock-5", "0", &mysql.StmtClosePacket{StatementID: 4}, nil)
	latePrepare := mysqlStmtMock("mock-6", "0", &mysql.StmtPreparePacket{Query: "SELECT ?"}, &mysql.StmtPrepareOkPacket{StatementID: 4})

	var c MockDepsCheck
	for _, m := range []*Mock{prepare, met, orphan, carried, closer, late, latePrepare} {
		c.Add(m)
	}
	want := []UnmetRequirement{{Mock: "mock-3", Kind: MySQL, Key: MySQLStatementKey("0", 2)}}
	if got := c.Unmet(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Unmet = %+v, want %+v", got, want)
	}
	var empty MockDepsCheck
	if got := empty.Unmet(); got != nil {
		t.Errorf("an empty check has unmet requirements: %+v", got)
	}
}

func TestRegisterMockDepsDeclaresAKindsLinks(t *testing.T) {
	const kind Kind = "Acme-Queue"
	m := &Mock{Name: "mock-1", Kind: kind}
	if got := DepsOf(m); len(got.Provides)+len(got.Requires) != 0 {
		t.Fatalf("an undeclared kind has deps: %+v", got)
	}
	RegisterMockDeps(kind, func(*Mock) MockDeps { return MockDeps{Provides: []string{"acme lease"}} })
	t.Cleanup(func() { RegisterMockDeps(kind, nil) })
	if got := DepsOf(m); !reflect.DeepEqual(got.Provides, []string{"acme lease"}) {
		t.Errorf("DepsOf a registered kind = %+v", got)
	}
	RegisterMockDeps(kind, nil)
	if got := DepsOf(m); len(got.Provides) != 0 {
		t.Errorf("a removed declaration still applies: %+v", got)
	}
	if got := DepsOf(nil); len(got.Provides)+len(got.Requires) != 0 {
		t.Errorf("DepsOf(nil) = %+v", got)
	}
}
