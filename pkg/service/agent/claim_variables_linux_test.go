//go:build linux

package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// claimVarProxy is a servingProxy that keeps the test variables on its mock
// manager, as the proxy does: a test's begin starts the test's, and a new
// claim the claim key's (coreAgent.ClaimVariables).
type claimVarProxy struct {
	*servingProxy
	claims []string
}

func (p *claimVarProxy) ResetVariables() { p.mm.Variables().Reset() }

// OpenExactTest opens a test with its variables in one step, as keploy's proxy
// does (proxy.Proxy.OpenExactTest): the claim key's stay bound.
func (p *claimVarProxy) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	p.mm.OpenExactTest(test, owned, given)
	return nil
}
func (p *claimVarProxy) BeginClaimVariables(key string) {
	p.claims = append(p.claims, key)
	p.mm.Variables().StartOutside()
}

// A value a Playwright beforeAll seeds outside every test is the claim key's
// variable: the pool call binds it, the key's test is answered with it, and
// a new claim (the test process run again) starts it afresh: its beforeAll
// binds it anew, and a test it did not run before has nothing bound.
func TestAClaimKeysVariableIsBoundOutsideItsTestsUntilTheNextClaim(t *testing.T) {
	create := pingMock("mock-0", "/roles/new/e2e-r-1111", `{"ok":true}`, t0)
	create.Spec.Variables = models.MockVariables{"role": {Recorded: "e2e-r-1111", Bind: "url.path[2]", Outside: true}}
	list := pingMock("mock-1", "/roles", `{"roles":["admin","e2e-r-1111"]}`, t0.Add(2*time.Second))
	list.Spec.Variables = models.MockVariables{"role": {Recorded: "e2e-r-1111", Answer: []string{"body.roles[1]"}, Outside: true}}
	a, sp := unclaimedExactAgent(t, []*models.Mock{create, list},
		map[string][]string{"o::TestRoles": {"mock-1"}}, map[string][]string{"o": {"mock-0"}})
	p := &claimVarProxy{servingProxy: sp}
	a.Proxy = p
	claim := func(key string) {
		t.Helper()
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: key}, os.Getpid())
		require.Nil(t, serr)
	}

	claim("o")
	code, _ := p.call(t, "/roles/new/e2e-r-7777")
	require.Equal(t, 200, code, "the beforeAll's call binds the key's variable")
	got, e := runTest(t, a, sp, "TestRoles", "/roles")
	require.Equal(t, []string{`200 {"roles":["admin","e2e-r-7777"]}`}, got)
	require.Empty(t, e.Action, e.Reason)

	// The test process runs again: a new claim, whose beforeAll mints anew.
	claim("x")
	claim("o")
	code, _ = p.call(t, "/roles/new/e2e-r-8888")
	require.Equal(t, 200, code)
	got, e = runTest(t, a, sp, "TestRoles", "/roles")
	require.Equal(t, []string{`200 {"roles":["admin","e2e-r-8888"]}`}, got)
	require.Empty(t, e.Action, e.Reason)

	// A new claim whose beforeAll has not run: the key's variable is not
	// bound, and the test's call is a miss, never answered with a previous
	// claim's value.
	claim("x")
	claim("o")
	got, _ = runTest(t, a, sp, "TestRoles", "/roles")
	require.Len(t, got, 1)
	require.Equal(t, "502", got[0][:3], "answered with a previous claim's value: %s", got[0])
	require.Equal(t, []string{"o", "x", "o", "x", "o"}, p.claims)
}
