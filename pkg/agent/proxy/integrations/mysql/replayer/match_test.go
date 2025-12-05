package replayer

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// ============================================================================
// PreparedStmtHistory Tests
// ============================================================================

func TestPreparedStmtHistory_NewPreparedStmtHistory(t *testing.T) {
	h := wire.NewPreparedStmtHistory()

	if h == nil {
		t.Fatal("NewPreparedStmtHistory returned nil")
	}
	if len(h.Entries) != 0 {
		t.Errorf("expected empty Entries, got %d", len(h.Entries))
	}
	if h.QueryIndex == nil {
		t.Error("QueryIndex should not be nil")
	}
	if h.CurrentCycle != 0 {
		t.Errorf("expected CurrentCycle=0, got %d", h.CurrentCycle)
	}
}

func TestPreparedStmtHistory_RecordPrepare(t *testing.T) {
	h := wire.NewPreparedStmtHistory()

	// Record first prepare
	h.RecordPrepare(1, "SELECT * FROM users WHERE id = ?")

	if len(h.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(h.Entries))
	}
	if h.CurrentCycle != 1 {
		t.Errorf("expected CurrentCycle=1, got %d", h.CurrentCycle)
	}
	if h.Entries[0].StmtID != 1 {
		t.Errorf("expected StmtID=1, got %d", h.Entries[0].StmtID)
	}
	if h.Entries[0].ClosedAt != -1 {
		t.Errorf("expected ClosedAt=-1 (active), got %d", h.Entries[0].ClosedAt)
	}

	// Record second prepare (same query)
	h.RecordPrepare(2, "SELECT * FROM users WHERE id = ?")

	if len(h.Entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(h.Entries))
	}
	if h.CurrentCycle != 2 {
		t.Errorf("expected CurrentCycle=2, got %d", h.CurrentCycle)
	}
	if h.GetPrepareCountForQuery("SELECT * FROM users WHERE id = ?") != 2 {
		t.Errorf("expected prepare count=2 for query")
	}
}

func TestPreparedStmtHistory_RecordClose(t *testing.T) {
	h := wire.NewPreparedStmtHistory()

	// Prepare and then close
	h.RecordPrepare(1, "SELECT * FROM users")
	h.RecordClose(1)

	if h.Entries[0].ClosedAt == -1 {
		t.Error("expected entry to be marked as closed")
	}
	if h.Entries[0].ClosedAt != int64(h.CurrentCycle) {
		t.Errorf("expected ClosedAt=%d, got %d", h.CurrentCycle, h.Entries[0].ClosedAt)
	}
}

func TestPreparedStmtHistory_GetActiveEntryByQuery(t *testing.T) {
	h := wire.NewPreparedStmtHistory()

	// Prepare, close, prepare again (same query)
	h.RecordPrepare(1, "SELECT * FROM products")
	h.RecordClose(1)
	h.RecordPrepare(2, "SELECT * FROM products")

	entry := h.GetActiveEntryByQuery("SELECT * FROM products")
	if entry == nil {
		t.Fatal("expected to find active entry")
	}
	if entry.StmtID != 2 {
		t.Errorf("expected StmtID=2 (active one), got %d", entry.StmtID)
	}

	// Close the second one too
	h.RecordClose(2)
	entry = h.GetActiveEntryByQuery("SELECT * FROM products")
	if entry != nil {
		t.Error("expected no active entry after closing all")
	}
}

func TestPreparedStmtHistory_GetPrepareCountForQuery(t *testing.T) {
	h := wire.NewPreparedStmtHistory()

	query := "INSERT INTO orders (user_id) VALUES (?)"

	if h.GetPrepareCountForQuery(query) != 0 {
		t.Error("expected count=0 for unprepared query")
	}

	h.RecordPrepare(1, query)
	if h.GetPrepareCountForQuery(query) != 1 {
		t.Error("expected count=1")
	}

	h.RecordPrepare(2, query)
	h.RecordPrepare(3, query)
	if h.GetPrepareCountForQuery(query) != 3 {
		t.Error("expected count=3")
	}
}

func TestPreparedStmtHistory_CaseInsensitiveQuery(t *testing.T) {
	h := wire.NewPreparedStmtHistory()

	h.RecordPrepare(1, "SELECT * FROM Users")

	// Should match case-insensitively
	entry := h.GetActiveEntryByQuery("select * from users")
	if entry == nil {
		t.Error("expected case-insensitive match")
	}

	count := h.GetPrepareCountForQuery("SELECT * FROM USERS")
	if count != 1 {
		t.Errorf("expected count=1 with case-insensitive match, got %d", count)
	}
}

// ============================================================================
// Edge Case Scenario Tests
// ============================================================================

func TestPreparedStmtHistory_PrepareClosePrepareCycle(t *testing.T) {
	// Scenario 1: Prepare→Close→Prepare→Close→Prepare→Execute
	// Same query gets prepared multiple times with different IDs
	h := wire.NewPreparedStmtHistory()
	query := "SELECT * FROM users WHERE id = ?"

	// First prepare cycle
	h.RecordPrepare(1, query)
	if h.GetCurrentCycle() != 1 {
		t.Errorf("expected cycle=1, got %d", h.GetCurrentCycle())
	}

	// Close first
	h.RecordClose(1)

	// Second prepare cycle
	h.RecordPrepare(2, query)
	if h.GetCurrentCycle() != 2 {
		t.Errorf("expected cycle=2, got %d", h.GetCurrentCycle())
	}

	// Close second
	h.RecordClose(2)

	// Third prepare cycle (this is the one that will be executed)
	h.RecordPrepare(3, query)
	if h.GetCurrentCycle() != 3 {
		t.Errorf("expected cycle=3, got %d", h.GetCurrentCycle())
	}

	// Verify the active entry is the third one
	active := h.GetActiveEntryByQuery(query)
	if active == nil {
		t.Fatal("expected active entry")
	}
	if active.StmtID != 3 {
		t.Errorf("expected active StmtID=3, got %d", active.StmtID)
	}

	// Verify we have 3 entries total
	if len(h.Entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(h.Entries))
	}

	// Verify prepare count
	if h.GetPrepareCountForQuery(query) != 3 {
		t.Errorf("expected prepare count=3, got %d", h.GetPrepareCountForQuery(query))
	}
}

func TestPreparedStmtHistory_MultiplePrepareWithoutClose(t *testing.T) {
	// Scenario 2: Multiple prepares without closing
	// Same query gets multiple IDs, all active
	h := wire.NewPreparedStmtHistory()
	query := "UPDATE users SET name = ? WHERE id = ?"

	h.RecordPrepare(1, query)
	h.RecordPrepare(2, query)
	h.RecordPrepare(3, query)

	// All should be active
	if len(h.Entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(h.Entries))
	}

	for i, entry := range h.Entries {
		if entry.ClosedAt != -1 {
			t.Errorf("entry %d should be active (ClosedAt=-1), got %d", i, entry.ClosedAt)
		}
	}

	// GetActiveEntryByQuery returns the most recent
	active := h.GetActiveEntryByQuery(query)
	if active.StmtID != 3 {
		t.Errorf("expected most recent active StmtID=3, got %d", active.StmtID)
	}
}

// ============================================================================
// prepEntry and buildRecordedPrepIndex Tests
// ============================================================================

func TestBuildRecordedPrepIndex_BasicFunctionality(t *testing.T) {
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM users"),
		createPrepMock("mock-2", "conn-1", 2, "INSERT INTO users (name) VALUES (?)"),
		createPrepMock("mock-3", "conn-2", 1, "DELETE FROM users WHERE id = ?"),
	}

	index := buildRecordedPrepIndex(mocks)

	// Check conn-1 has 2 entries
	if len(index["conn-1"]) != 2 {
		t.Errorf("expected 2 entries for conn-1, got %d", len(index["conn-1"]))
	}

	// Check conn-2 has 1 entry
	if len(index["conn-2"]) != 1 {
		t.Errorf("expected 1 entry for conn-2, got %d", len(index["conn-2"]))
	}
}

func TestBuildRecordedPrepIndex_PrepareOrder(t *testing.T) {
	// Same query prepared multiple times on same connection
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM products"),
		createPrepMock("mock-2", "conn-1", 2, "SELECT * FROM products"),
		createPrepMock("mock-3", "conn-1", 3, "SELECT * FROM products"),
	}

	index := buildRecordedPrepIndex(mocks)

	entries := index["conn-1"]
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify prepare order
	for i, entry := range entries {
		expectedOrder := i + 1
		if entry.prepareOrder != expectedOrder {
			t.Errorf("entry %d: expected prepareOrder=%d, got %d", i, expectedOrder, entry.prepareOrder)
		}
	}
}

func TestBuildRecordedPrepIndex_SkipsConfigMocks(t *testing.T) {
	mocks := []*models.Mock{
		createConfigMock("config-mock", "conn-1"),
		createPrepMock("data-mock", "conn-1", 1, "SELECT 1"),
	}

	index := buildRecordedPrepIndex(mocks)

	if len(index["conn-1"]) != 1 {
		t.Errorf("expected 1 entry (config mock should be skipped), got %d", len(index["conn-1"]))
	}
}

// ============================================================================
// lookupRecordedQuery Tests
// ============================================================================

func TestLookupRecordedQuery_Found(t *testing.T) {
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM users"),
		createPrepMock("mock-2", "conn-1", 2, "SELECT * FROM orders"),
	}

	index := buildRecordedPrepIndex(mocks)

	query := lookupRecordedQuery(index, "conn-1", 1)
	if query != "SELECT * FROM users" {
		t.Errorf("expected 'SELECT * FROM users', got '%s'", query)
	}

	query = lookupRecordedQuery(index, "conn-1", 2)
	if query != "SELECT * FROM orders" {
		t.Errorf("expected 'SELECT * FROM orders', got '%s'", query)
	}
}

func TestLookupRecordedQuery_NotFound(t *testing.T) {
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM users"),
	}

	index := buildRecordedPrepIndex(mocks)

	// Non-existent statement ID
	query := lookupRecordedQuery(index, "conn-1", 999)
	if query != "" {
		t.Errorf("expected empty string for non-existent stmtID, got '%s'", query)
	}

	// Non-existent connection
	query = lookupRecordedQuery(index, "conn-999", 1)
	if query != "" {
		t.Errorf("expected empty string for non-existent connID, got '%s'", query)
	}
}

// ============================================================================
// lookupRecordedQueryByContent Tests
// ============================================================================

func TestLookupRecordedQueryByContent_BasicFunctionality(t *testing.T) {
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM users"),
		createPrepMock("mock-2", "conn-1", 2, "SELECT * FROM users"),
		createPrepMock("mock-3", "conn-1", 3, "SELECT * FROM users"),
	}

	index := buildRecordedPrepIndex(mocks)

	// Find first prepare
	entry := lookupRecordedQueryByContent(index, "conn-1", "SELECT * FROM users", 1)
	if entry == nil {
		t.Fatal("expected to find entry")
	}
	if entry.statementID != 1 {
		t.Errorf("expected stmtID=1, got %d", entry.statementID)
	}

	// Find second prepare
	entry = lookupRecordedQueryByContent(index, "conn-1", "SELECT * FROM users", 2)
	if entry == nil {
		t.Fatal("expected to find entry")
	}
	if entry.statementID != 2 {
		t.Errorf("expected stmtID=2, got %d", entry.statementID)
	}

	// Find third prepare
	entry = lookupRecordedQueryByContent(index, "conn-1", "SELECT * FROM users", 3)
	if entry == nil {
		t.Fatal("expected to find entry")
	}
	if entry.statementID != 3 {
		t.Errorf("expected stmtID=3, got %d", entry.statementID)
	}
}

func TestLookupRecordedQueryByContent_CaseInsensitive(t *testing.T) {
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM Users"),
	}

	index := buildRecordedPrepIndex(mocks)

	// Should match case-insensitively
	entry := lookupRecordedQueryByContent(index, "conn-1", "select * from users", 1)
	if entry == nil {
		t.Error("expected case-insensitive match")
	}
}

func TestLookupRecordedQueryByContent_NotFound(t *testing.T) {
	mocks := []*models.Mock{
		createPrepMock("mock-1", "conn-1", 1, "SELECT * FROM users"),
	}

	index := buildRecordedPrepIndex(mocks)

	// Query doesn't exist
	entry := lookupRecordedQueryByContent(index, "conn-1", "SELECT * FROM orders", 1)
	if entry != nil {
		t.Error("expected nil for non-existent query")
	}

	// Prepare order too high
	entry = lookupRecordedQueryByContent(index, "conn-1", "SELECT * FROM users", 2)
	if entry != nil {
		t.Error("expected nil for non-existent prepare order")
	}
}

// ============================================================================
// markClosedEntries Tests
// ============================================================================

func TestMarkClosedEntries(t *testing.T) {
	// Create mocks with PREP and CLOSE
	prepMock := createPrepMock("prep-mock", "conn-1", 1, "SELECT * FROM users")
	closeMock := createCloseMock("close-mock", "conn-1", 1)

	mocks := []*models.Mock{prepMock, closeMock}

	index := buildRecordedPrepIndex(mocks)

	// The entry should be marked as closed
	entries := index["conn-1"]
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if !entries[0].wasClosed {
		t.Error("expected entry to be marked as wasClosed=true")
	}
}

func TestMarkClosedEntries_PartialClose(t *testing.T) {
	// Multiple preps, only one closed
	mocks := []*models.Mock{
		createPrepMock("prep-1", "conn-1", 1, "SELECT 1"),
		createPrepMock("prep-2", "conn-1", 2, "SELECT 2"),
		createCloseMock("close-1", "conn-1", 1),
	}

	index := buildRecordedPrepIndex(mocks)

	entries := index["conn-1"]
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// First should be closed
	if !entries[0].wasClosed {
		t.Error("first entry should be marked as closed")
	}

	// Second should NOT be closed
	if entries[1].wasClosed {
		t.Error("second entry should NOT be marked as closed")
	}
}

// ============================================================================
// Integration Test: Full Scenario
// ============================================================================

func TestIntegration_PrepareClosePrepareCycle(t *testing.T) {
	// Simulates: Prepare→Close→Prepare→Close→Prepare→Execute
	// This is the main edge case we're solving

	// Create recorded mocks
	mocks := []*models.Mock{
		createPrepMock("prep-1", "conn-1", 1, "SELECT * FROM users WHERE id = ?"),
		createCloseMock("close-1", "conn-1", 1),
		createPrepMock("prep-2", "conn-1", 2, "SELECT * FROM users WHERE id = ?"),
		createCloseMock("close-2", "conn-1", 2),
		createPrepMock("prep-3", "conn-1", 3, "SELECT * FROM users WHERE id = ?"),
	}

	index := buildRecordedPrepIndex(mocks)

	entries := index["conn-1"]
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify prepare orders
	for i, entry := range entries {
		if entry.prepareOrder != i+1 {
			t.Errorf("entry %d: expected prepareOrder=%d, got %d", i, i+1, entry.prepareOrder)
		}
	}

	// First two should be closed, third should not
	if !entries[0].wasClosed {
		t.Error("first entry should be closed")
	}
	if !entries[1].wasClosed {
		t.Error("second entry should be closed")
	}
	if entries[2].wasClosed {
		t.Error("third entry should NOT be closed (it's the active one)")
	}

	// Now simulate runtime with PreparedStmtHistory
	h := wire.NewPreparedStmtHistory()

	// Runtime: prepare, close, prepare, close, prepare
	h.RecordPrepare(100, "SELECT * FROM users WHERE id = ?") // runtime ID=100
	h.RecordClose(100)
	h.RecordPrepare(101, "SELECT * FROM users WHERE id = ?") // runtime ID=101
	h.RecordClose(101)
	h.RecordPrepare(102, "SELECT * FROM users WHERE id = ?") // runtime ID=102

	// The active entry should be the one with runtime ID=102
	active := h.GetActiveEntryByQuery("SELECT * FROM users WHERE id = ?")
	if active == nil {
		t.Fatal("expected active entry")
	}
	if active.StmtID != 102 {
		t.Errorf("expected active StmtID=102, got %d", active.StmtID)
	}

	// We can correlate: 3rd prepare in recorded (stmtID=3) matches 3rd prepare at runtime (stmtID=102)
	recordedEntry := lookupRecordedQueryByContent(index, "conn-1", "SELECT * FROM users WHERE id = ?", 3)
	if recordedEntry == nil {
		t.Fatal("expected to find 3rd recorded entry")
	}
	if recordedEntry.statementID != 3 {
		t.Errorf("expected recorded stmtID=3, got %d", recordedEntry.statementID)
	}
}

// ============================================================================
// Helper Functions for Creating Test Mocks
// ============================================================================

func createPrepMock(name, connID string, stmtID uint32, query string) *models.Mock {
	return &models.Mock{
		Kind: models.MySQL,
		Name: name,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"connID": connID,
				"type":   "data",
			},
			MySQLRequests: []mysql.Request{
				{
					PacketBundle: mysql.PacketBundle{
						Header: &mysql.PacketInfo{
							Type: "COM_STMT_PREPARE",
						},
						Message: &mysql.StmtPreparePacket{
							Query: query,
						},
					},
				},
			},
			MySQLResponses: []mysql.Response{
				{
					PacketBundle: mysql.PacketBundle{
						Header: &mysql.PacketInfo{
							Type: "StmtPrepareOk",
						},
						Message: &mysql.StmtPrepareOkPacket{
							StatementID: stmtID,
						},
					},
				},
			},
		},
	}
}

func createCloseMock(name, connID string, stmtID uint32) *models.Mock {
	return &models.Mock{
		Kind: models.MySQL,
		Name: name,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"connID": connID,
				"type":   "data",
			},
			MySQLRequests: []mysql.Request{
				{
					PacketBundle: mysql.PacketBundle{
						Header: &mysql.PacketInfo{
							Type: "COM_STMT_CLOSE",
						},
						Message: &mysql.StmtClosePacket{
							StatementID: stmtID,
						},
					},
				},
			},
			MySQLResponses: []mysql.Response{}, // CLOSE has no response
		},
	}
}

func createConfigMock(name, connID string) *models.Mock {
	return &models.Mock{
		Kind: models.MySQL,
		Name: name,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"connID": connID,
				"type":   "config",
			},
		},
	}
}
