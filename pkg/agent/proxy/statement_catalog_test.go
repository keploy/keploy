package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The staged statement catalog is read by kind, through the manager and
// through every scoped view, and is in no tree: no read a parser serves from
// returns it, so a statement recorded inside a test is described at app start
// and never answered there. A new staging replaces it whole.
func TestTheStatementCatalogIsDescribeOnly(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	stmt := pgStatementMock("lists-in-a", "sha256:lists", 10)
	other := perTestMockAt("http", 11)
	mm.SeedStatementCatalog([]*models.Mock{stmt, other, nil})

	if got := mm.StatementCatalog(models.PostgresV3); len(got) != 1 || got[0] != stmt {
		t.Fatalf("the PostgreSQL catalog is %v, want the staged statement", got)
	}
	var view interface{} = &scopedMockDb{MockMemDb: mm}
	c, ok := view.(integrations.StatementCatalog)
	if !ok {
		t.Fatal("the statement catalog is erased by the scoped view")
	}
	if got := c.StatementCatalog(models.PostgresV3); len(got) != 1 || got[0] != stmt {
		t.Fatalf("the scoped view's catalog is %v, want the staged statement", got)
	}

	for name, read := range map[string]func() ([]*models.Mock, error){
		"filtered":   mm.GetFilteredMocks,
		"unfiltered": mm.GetUnFilteredMocks,
		"startup":    mm.GetStartupMocks,
		"session":    mm.GetSessionMocks,
	} {
		ms, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range ms {
			if m == stmt {
				t.Fatalf("the %s read serves a catalog mock", name)
			}
		}
	}
	if mm.InTestWindow(time.Unix(10, 0)) {
		t.Fatal("staging the catalog opened a test window")
	}

	mm.SeedStatementCatalog(nil)
	if got := mm.StatementCatalog(models.PostgresV3); len(got) != 0 {
		t.Fatalf("a new staging left the old catalog: %v", got)
	}
}
