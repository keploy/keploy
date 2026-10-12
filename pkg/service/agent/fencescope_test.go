package agent

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Only the staged test's OWN GraphQL mutations are fences: a suite write, a
// spec-shared write, or another scope's write the replay may never repeat would
// otherwise hold every later read back.
func TestMarkFenceScope(t *testing.T) {
	const mutation = `{"query":"mutation($id: String!) { requestBranchReview(branchId: $id) }"}`
	const query = `{"operationName":"GetBranches","query":"query GetBranches { branches { id } }"}`
	ownWrite := httpMock("own-w", "POST", "/query", mutation, 200, "", 1)
	ownRead := httpMock("own-r", "POST", "/query", query, 200, "", 2)
	ownShared := httpMock("own-shared-w", "POST", "/query", mutation, 200, "", 3)
	ownShared.TestModeInfo.SharedInSpec = true
	suiteWrite := httpMock("suite-w", "POST", "/query", mutation, 200, "", 4)
	all := []*models.Mock{ownWrite, ownRead, ownShared, suiteWrite}

	a := &Agent{logger: zap.NewNop()}
	a.setFenceScope("spec > t", []string{"own-w", "own-r", "own-shared-w"})
	a.markFenceScope(all)

	for _, c := range []struct {
		m     *models.Mock
		scope string
		write bool
	}{
		{ownWrite, "spec > t", true},
		{ownRead, "spec > t", false},
		{ownShared, "spec > t", false},
		{suiteWrite, "spec > t", false}, // staged with the test, but not its own write
	} {
		if c.m.TestModeInfo.FenceScope != c.scope || c.m.TestModeInfo.FenceWrite != c.write {
			t.Errorf("%s: scope=%q write=%v, want scope=%q write=%v", c.m.Name,
				c.m.TestModeInfo.FenceScope, c.m.TestModeInfo.FenceWrite, c.scope, c.write)
		}
	}

	// Staging a suite/setup pool clears every tag: none may outlive its test.
	a.clearFenceScope()
	a.markFenceScope(all)
	for _, m := range all {
		if m.TestModeInfo.FenceScope != "" || m.TestModeInfo.FenceWrite {
			t.Errorf("%s still tagged after the fence scope was cleared", m.Name)
		}
	}
}

// A suite copy captured after the test's own last recorded mock belongs to a
// later hook; one captured before (setup the test depends on) does not.
func TestMarkFenceScopeTagsLaterSuiteCopies(t *testing.T) {
	const query = `{"operationName":"GetSuites","query":"query GetSuites { suites { id } }"}`
	own := httpMock("own", "POST", "/query", query, 200, "", 10)
	setup := httpMock("suite-setup", "POST", "/query", query, 200, "", 5)
	later := httpMock("suite-later", "POST", "/query", query, 200, "", 20)
	shared := httpMock("shared-later", "POST", "/query", query, 200, "", 30)
	shared.TestModeInfo.SharedInSpec = true

	a := &Agent{logger: zap.NewNop()}
	a.setFenceScope("spec > t", []string{"own"})
	a.markFenceScope([]*models.Mock{own, setup, later, shared})

	if own.TestModeInfo.FenceLater || setup.TestModeInfo.FenceLater {
		t.Fatal("the test's own copy and setup recorded before it must stay usable")
	}
	if !later.TestModeInfo.FenceLater {
		t.Fatal("a suite copy recorded after the test belongs to a later hook")
	}
	if shared.TestModeInfo.FenceLater {
		t.Fatal("a spec-shared copy answers identically at every moment and is never held")
	}
}
