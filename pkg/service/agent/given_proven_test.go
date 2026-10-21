package agent

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// givenProxy is a wireProxy that keeps test variables: it records each given
// binding, as the proxy binds it (proxy.Proxy.BindGiven).
type givenProxy struct {
	*wireProxy
	bound []models.VariableBinding
}

func (p *givenProxy) BindGiven(b models.VariableBinding) (models.VariableBinding, bool, bool) {
	p.bound = append(p.bound, b)
	return b, true, true
}

// In a record, what the open test's harness gives is kept, in order, with the
// test's window, for the CLI to write into the test's mapping. A value given
// before any claim belongs to nothing, and is refused.
func TestARecordKeepsWhatTheHarnessGaveWithTheTestsWindow(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	serr := a.Give(ctx, models.GivenReq{Name: "cookie-jwt_token_staging-1", Value: "tok-before"})
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	claimSelf(t, a)

	_, serr = a.ProvenBegin(ctx, begin("TestReview"))
	require.Nil(t, serr)
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "cookie-jwt_token_staging-1", Value: "tok-1"}))
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "cookie-jwt_token_staging-2", Value: "tok-2"}))
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestReview", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []models.GivenValue{
		{Name: "cookie-jwt_token_staging-1", Value: "tok-1"},
		{Name: "cookie-jwt_token_staging-2", Value: "tok-2"},
	}, windowOf(t, a, "o::TestReview").Given)
}

// In a replay, a value the open test's harness gives is bound as the test's
// given variable of its name, recorded as the recording's value given under
// that name. A name its recording gave none under binds nothing.
func TestAReplayBindsWhatTheHarnessGivesAsTheRecordingsValue(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &givenProxy{wireProxy: wp}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.SetTestGiven(ctx, map[string][]models.GivenValue{
		"o::TestReview": {{Name: "cookie-jwt_token_staging-1", Value: "tok-recorded"}},
	}, nil))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestReview"))
	require.Nil(t, serr)
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "cookie-jwt_token_staging-1", Value: "tok-live"}))
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "cookie-jwt_token_staging-9", Value: "tok-other"}))
	require.Equal(t, []models.VariableBinding{{
		Name: "cookie-jwt_token_staging-1", Value: "tok-live", Recorded: "tok-recorded",
		Mock: "the test's harness", Place: "given", Given: true,
	}}, p.bound)
}

// A value the harness gives while no test is open (a group's beforeAll that
// signs a session) is the claim key's: a record keeps it with the key's claim
// window, for the CLI to write into the mapping by key.
func TestARecordKeepsWhatTheHarnessGaveOutsideEveryTestWithTheClaim(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "outside-cookie-jwt_token_staging-1", Value: "tok-beforeall"}))
	_, serr := a.ProvenBegin(ctx, begin("TestSeeded"))
	require.Nil(t, serr)
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "cookie-jwt_token_staging-1", Value: "tok-test"}))
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestSeeded", Outcome: "passed"})
	require.Nil(t, serr)
	ws, err := a.FinalScopeWindows(ctx)
	require.NoError(t, err)
	var claim *models.ScopeWindow
	for i := range ws {
		if ws[i].Claim == "o" {
			claim = &ws[i]
		}
	}
	require.NotNil(t, claim, "the claim's window")
	require.Equal(t, []models.GivenValue{{Name: "outside-cookie-jwt_token_staging-1", Value: "tok-beforeall"}}, claim.Given)
	require.Equal(t, []models.GivenValue{{Name: "cookie-jwt_token_staging-1", Value: "tok-test"}}, windowOf(t, a, "o::TestSeeded").Given)
}

// In a replay, a value given while no test is open is bound in the claim
// key's scope (Outside), recorded as the key's recording gave it.
func TestAReplayBindsWhatTheHarnessGivesOutsideEveryTestAsTheKeys(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &givenProxy{wireProxy: wp}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.SetTestGiven(ctx, nil, map[string][]models.GivenValue{
		"o": {{Name: "outside-cookie-jwt_token_staging-1", Value: "tok-recorded"}},
	}))
	claimSelf(t, a)
	require.Nil(t, a.Give(ctx, models.GivenReq{Name: "outside-cookie-jwt_token_staging-1", Value: "tok-live"}))
	require.Equal(t, []models.VariableBinding{{
		Name: "outside-cookie-jwt_token_staging-1", Value: "tok-live", Recorded: "tok-recorded",
		Mock: "the claim key's harness", Place: "given", Given: true, Outside: true,
	}}, p.bound)
}
