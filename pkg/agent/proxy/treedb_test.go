package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// helper to create a mock with the given name, sortOrder, and ID.
func mockWith(name string, sortOrder int64, id int) *models.Mock {
	return &models.Mock{
		Name: name,
		TestModeInfo: models.TestModeInfo{
			SortOrder:  sortOrder,
			ID:         id,
			IsFiltered: true,
		},
	}
}

func TestUpdateExactMatch(t *testing.T) {
	db := NewTreeDb(customComparator)
	m := mockWith("mock-5", 6, 5)
	db.insert(m.TestModeInfo, m)

	newMock := mockWith("mock-5", 99, 5)
	newMock.TestModeInfo.IsFiltered = false

	ok := db.update(m.TestModeInfo, newMock.TestModeInfo, newMock)
	if !ok {
		t.Fatal("expected update to succeed via exact key match")
	}

	// Old key should be gone
	if _, found := db.rbt.Get(m.TestModeInfo); found {
		t.Error("old key should have been removed")
	}
	// New key should exist
	val, found := db.rbt.Get(newMock.TestModeInfo)
	if !found {
		t.Fatal("new key should exist in tree")
	}
	if val.(*models.Mock).Name != "mock-5" {
		t.Errorf("expected mock-5, got %s", val.(*models.Mock).Name)
	}
}

func TestUpdateRejectsKeyCollision(t *testing.T) {
	// Simulate the bug scenario: after SetUnFilteredMocks re-indexes,
	// a stale mock's old key (SortOrder=8, ID=7) accidentally matches
	// a DIFFERENT mock that was re-inserted at that key position.
	db := NewTreeDb(customComparator)

	// "mock-10" now sits at SortOrder=8, ID=7 after re-indexing
	mock10 := mockWith("mock-10", 8, 7)
	db.insert(mock10.TestModeInfo, mock10)

	// A stale reference tries to consume "mock-7" using old key (SortOrder=8, ID=7)
	staleMock7 := mockWith("mock-7", 8, 7)
	newMock7 := mockWith("mock-7", 99, 7)
	newMock7.TestModeInfo.IsFiltered = false

	ok := db.update(staleMock7.TestModeInfo, newMock7.TestModeInfo, newMock7)

	// The update should NOT succeed via exact key match since mock-10 != mock-7.
	// It should fall through to name-based scan, but mock-7 doesn't exist in tree,
	// so it should return false.
	if ok {
		t.Fatal("update should have returned false: mock-7 is not in the tree")
	}

	// mock-10 should still be intact in the tree
	val, found := db.rbt.Get(mock10.TestModeInfo)
	if !found {
		t.Fatal("mock-10 should NOT have been removed by the stale update")
	}
	if val.(*models.Mock).Name != "mock-10" {
		t.Errorf("expected mock-10 to be intact, got %s", val.(*models.Mock).Name)
	}
}

func TestUpdateNameFallbackAfterReindex(t *testing.T) {
	// After SetUnFilteredMocks re-indexes, mock-7 is at a different key.
	// A stale reference with old key should still find it via name scan.
	db := NewTreeDb(customComparator)

	// mock-7 was re-indexed to SortOrder=11, ID=10
	mock7 := mockWith("mock-7", 11, 10)
	db.insert(mock7.TestModeInfo, mock7)

	// mock-10 at the old position of mock-7
	mock10 := mockWith("mock-10", 8, 7)
	db.insert(mock10.TestModeInfo, mock10)

	// Stale reference uses old key for mock-7
	staleKey := models.TestModeInfo{SortOrder: 8, ID: 7}
	newMock7 := mockWith("mock-7", 99, 7)
	newMock7.TestModeInfo.IsFiltered = false

	ok := db.update(staleKey, newMock7.TestModeInfo, newMock7)
	if !ok {
		t.Fatal("expected update to succeed via name-based fallback")
	}

	// mock-10 should still be intact (not accidentally removed)
	val, found := db.rbt.Get(mock10.TestModeInfo)
	if !found {
		t.Fatal("mock-10 should still be in the tree")
	}
	if val.(*models.Mock).Name != "mock-10" {
		t.Errorf("expected mock-10, got %s", val.(*models.Mock).Name)
	}

	// mock-7 at old position should be gone
	_, found = db.rbt.Get(models.TestModeInfo{SortOrder: 11, ID: 10})
	if found {
		t.Error("mock-7's old position should have been removed")
	}
}

func TestUpdateIDIndexRejectsCollision(t *testing.T) {
	// Test the ID-index fallback path also validates identity.
	db := NewTreeDb(customComparator)

	// mock-10 is in the tree with ID=5
	mock10 := mockWith("mock-10", 6, 5)
	db.insert(mock10.TestModeInfo, mock10)

	// Stale reference for mock-7 with ID=5 (key doesn't match exactly,
	// but ID index would find mock-10 at ID=5)
	staleKey := models.TestModeInfo{SortOrder: 999, ID: 5}
	newMock7 := mockWith("mock-7", 100, 5)
	newMock7.TestModeInfo.IsFiltered = false

	ok := db.update(staleKey, newMock7.TestModeInfo, newMock7)

	// Should fail: ID=5 in tree is mock-10, not mock-7. And mock-7 not in tree at all.
	if ok {
		t.Fatal("update should have returned false: mock-7 is not in the tree")
	}

	// mock-10 should still be intact
	val, found := db.rbt.Get(mock10.TestModeInfo)
	if !found {
		t.Fatal("mock-10 should NOT have been removed")
	}
	if val.(*models.Mock).Name != "mock-10" {
		t.Errorf("expected mock-10, got %s", val.(*models.Mock).Name)
	}
}
