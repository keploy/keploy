package mysql

import "testing"

func TestStatementIDNamesTheStatementOfEveryCommandThatNamesOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  interface{}
	}{
		{"execute", &StmtExecutePacket{StatementID: 7}},
		{"send long data", &StmtSendLongDataPacket{StatementID: 7}},
		{"reset", &StmtResetPacket{StatementID: 7}},
		{"close", &StmtClosePacket{StatementID: 7}},
		{"fetch", &StmtFetchPacket{StatementID: 7}},
	} {
		if id, ok := StatementID(tc.msg); !ok || id != 7 {
			t.Errorf("%s: StatementID = %d, %v; want 7, true", tc.name, id, ok)
		}
	}
	for _, msg := range []interface{}{
		&StmtPreparePacket{Query: "SELECT ?"},
		&QueryPacket{Query: "SELECT 1"},
		(*StmtExecutePacket)(nil),
		nil,
	} {
		if _, ok := StatementID(msg); ok {
			t.Errorf("StatementID(%T) reports a statement", msg)
		}
	}
}

func TestPreparedPairsThePrepareWithTheIDItsAnswerGave(t *testing.T) {
	id, q, ok := Prepared(&StmtPreparePacket{Query: "  INSERT INTO t VALUES (?)\n"}, &StmtPrepareOkPacket{StatementID: 9})
	if !ok || id != 9 || q != "INSERT INTO t VALUES (?)" {
		t.Fatalf("Prepared = %d, %q, %v; want 9, the trimmed query, true", id, q, ok)
	}
	for _, tc := range []struct {
		name      string
		req, resp interface{}
	}{
		{"an error answered it", &StmtPreparePacket{Query: "SELECT ?"}, &ERRPacket{}},
		{"not a prepare", &QueryPacket{Query: "SELECT 1"}, &StmtPrepareOkPacket{StatementID: 1}},
		{"no query", &StmtPreparePacket{Query: "  "}, &StmtPrepareOkPacket{StatementID: 1}},
		{"nil answer", &StmtPreparePacket{Query: "SELECT ?"}, (*StmtPrepareOkPacket)(nil)},
	} {
		if _, _, ok := Prepared(tc.req, tc.resp); ok {
			t.Errorf("%s: Prepared reports a statement", tc.name)
		}
	}
}

func TestSetStoredQueryLeavesASharedMetaAsItWas(t *testing.T) {
	shared := map[string]string{"auth operation": "public key response"}
	b := PacketBundle{Meta: shared}
	b.SetStoredQuery("  SELECT ?  ")
	if got := b.StoredQuery(); got != "SELECT ?" {
		t.Fatalf("StoredQuery = %q, want %q", got, "SELECT ?")
	}
	if b.Meta["auth operation"] != "public key response" {
		t.Errorf("SetStoredQuery dropped the bundle's other meta: %v", b.Meta)
	}
	if _, leaked := shared[MetaQuery]; leaked {
		t.Errorf("SetStoredQuery wrote into the map the bundle shared: %v", shared)
	}

	var empty PacketBundle
	empty.SetStoredQuery("   ")
	if empty.Meta != nil {
		t.Errorf("SetStoredQuery of no query wrote %v", empty.Meta)
	}
	if (*PacketBundle)(nil).StoredQuery() != "" {
		t.Error("a nil bundle has a stored query")
	}
}
