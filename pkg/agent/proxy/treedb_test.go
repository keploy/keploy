package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// count returns how many entries the tree holds.
func count(tree *TreeDb) int {
	n := 0
	tree.rangeValues(func(interface{}) bool { n++; return true })
	return n
}

// TestDeleteMockRejectsCrossTreeKeyCollision guards the identity check in
// deleteMock.
//
// customComparator keys the tree on (SortOrder, ID) alone — not Name, not Kind —
// while SetFilteredMocks and SetUnFilteredMocks each stamp those fields from
// their OWN 0-based slice index. The first mock of every pool therefore lands on
// (SortOrder=1, ID=0), so the same key exists in more than one tree.
//
// Without an identity check a delete aimed at the wrong tree does not miss: it
// removes whichever unrelated mock occupies that coordinate. This test builds
// exactly that collision and asserts the bystander survives.
func TestDeleteMockRejectsCrossTreeKeyCollision(t *testing.T) {
	key := models.TestModeInfo{SortOrder: 1, ID: 0}

	resident := &models.Mock{Name: "per-test-mock", Kind: models.HTTP, TestModeInfo: key}
	intruder := &models.Mock{Name: "session-mock", Kind: models.HTTP, TestModeInfo: key}

	tree := NewTreeDb(customComparator)
	tree.insert(resident.TestModeInfo, resident)

	// Same key, different mock: must be refused.
	if tree.deleteMock(intruder) {
		t.Errorf("deleteMock removed an entry for a DIFFERENT mock sharing (SortOrder, ID); "+
			"want a clean miss. resident=%q intruder=%q", resident.Name, intruder.Name)
	}
	if got := count(tree); got != 1 {
		t.Errorf("bystander was evicted: tree holds %d entries, want 1", got)
	}

	// The genuine owner must still be removable.
	if !tree.deleteMock(resident) {
		t.Errorf("deleteMock refused the mock that actually owns the key (%q)", resident.Name)
	}
	if got := count(tree); got != 0 {
		t.Errorf("owner was not removed: tree holds %d entries, want 0", got)
	}
}

// TestDeleteMockDistinguishesKind covers two mocks that share both the key and
// the name but differ in Kind, which the per-kind trees make reachable.
func TestDeleteMockDistinguishesKind(t *testing.T) {
	key := models.TestModeInfo{SortOrder: 2, ID: 5}

	httpMock := &models.Mock{Name: "same-name", Kind: models.HTTP, TestModeInfo: key}
	redisMock := &models.Mock{Name: "same-name", Kind: models.REDIS, TestModeInfo: key}

	tree := NewTreeDb(customComparator)
	tree.insert(httpMock.TestModeInfo, httpMock)

	if tree.deleteMock(redisMock) {
		t.Errorf("deleteMock removed a %s entry when asked to delete a %s mock", models.HTTP, models.REDIS)
	}
	if got := count(tree); got != 1 {
		t.Errorf("entry was evicted across Kind: tree holds %d entries, want 1", got)
	}
}

// TestDeleteMockNilSafe documents that a nil mock is a no-op rather than a panic.
func TestDeleteMockNilSafe(t *testing.T) {
	tree := NewTreeDb(customComparator)
	if tree.deleteMock(nil) {
		t.Errorf("deleteMock(nil) reported a deletion")
	}
}
