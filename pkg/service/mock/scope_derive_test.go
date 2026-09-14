package mock

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func mk(name, owner string) *models.Mock {
	return &models.Mock{Name: name, Owner: owner}
}

// The table is a regrouping of the mocks we already hold, so a set whose mocks
// carry owners needs no mappings.yaml to be scoped.
func TestDeriveScopeTableGroupsByOwner(t *testing.T) {
	table := deriveScopeTable(
		[]*models.Mock{mk("a-0", "testA"), mk("b-0", "testB"), mk("a-1", "testA")},
		[]*models.Mock{mk("s-0", "__suite__:spec.ts")},
	)
	if len(table) != 3 {
		t.Fatalf("table has %d owners, want 3: %v", len(table), table)
	}
	got := table["testA"]
	if len(got) != 2 || got[0] != "a-0" || got[1] != "a-1" {
		t.Fatalf("testA = %v, want [a-0 a-1] in recorded order", got)
	}
	if len(table["__suite__:spec.ts"]) != 1 {
		t.Fatalf("suite owner missing: %v", table)
	}
}

// An unowned mock must NOT appear in the table. Absence is exactly what makes
// it shared overflow, reachable by every test; adding it would bind it to one.
func TestDeriveScopeTableExcludesUnowned(t *testing.T) {
	table := deriveScopeTable([]*models.Mock{mk("mock-0", ""), mk("a-0", "testA")})
	if _, present := table[""]; present {
		t.Fatalf(`empty owner must not be a key: %v`, table)
	}
	for owner, names := range table {
		for _, n := range names {
			if n == "mock-0" {
				t.Fatalf("unowned mock-0 was bound to owner %q", owner)
			}
		}
	}
	if len(table) != 1 {
		t.Fatalf("table = %v, want only testA", table)
	}
}

// No owners at all (a pre-Owner set) yields an empty table, which is the signal
// to fall back to mappings.yaml rather than push an empty table.
func TestDeriveScopeTableEmptyWhenNoOwners(t *testing.T) {
	if table := deriveScopeTable([]*models.Mock{mk("mock-0", ""), mk("mock-1", "")}); len(table) != 0 {
		t.Fatalf("table = %v, want empty so the caller falls back", table)
	}
	if table := deriveScopeTable(); len(table) != 0 {
		t.Fatalf("no input should yield empty, got %v", table)
	}
}
