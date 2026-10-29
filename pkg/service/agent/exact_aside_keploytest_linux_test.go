package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// asideConsumes is what the manager reports consumed that a set-aside test's
// recording answered: name -> the test it is from.
func asideConsumes(p *servingProxy) map[string]string {
	out := map[string]string{}
	for _, m := range p.mm.GetConsumedMocks() {
		if m.SetAsideFrom != "" {
			out[m.Name] = m.SetAsideFrom
		}
	}
	return out
}

// keploy test under the switch, TestNonExistentApp's shape in api-server's
// acceptance run 3: the quarantined test before the kept one filled a cache
// at record (vault's PAT read), so at replay, with it set aside, the kept
// test makes that call in its own window. Its opening names the set-aside
// test (models.ExactServing.Aside), and the set-aside recording answers the
// call, unchecked: the kept test owes nothing, and the consume says whose
// recording it is (MockState.SetAsideFrom), so the replay names it on its
// result and counts it as none of its own. The recording answers once: a
// later test that makes the call again misses. A call that is not exactly
// the recording still misses. An opening that names no set-aside test (an
// older keploy test) serves none, as before.
func TestKeployTestServesASetAsideTestsRecordingToTheTestAfterIt(t *testing.T) {
	mocks := func() []*models.Mock {
		return []*models.Mock{
			pingMock("mock-1", "/pat", "pat", t0),
			pingMock("mock-2", "/k", "k", t0.Add(time.Second)),
			pingMock("mock-3", "/l", "l", t0.Add(2*time.Second)),
		}
	}
	aside := []models.AsideTest{{Test: "TestQ", Owned: []string{"mock-1"}}}
	open := func(t *testing.T, a *Agent, name string, owned []string, aside []models.AsideTest) {
		t.Helper()
		require.NoError(t, a.UpdateMockParams(context.Background(), models.MockFilterParams{Exact: &models.ExactServing{Test: name, Owned: owned, Aside: aside}}))
	}
	call := func(t *testing.T, p *servingProxy, paths ...string) []int {
		t.Helper()
		var codes []int
		for _, path := range paths {
			code, _ := p.call(t, path)
			codes = append(codes, code)
		}
		return codes
	}
	pool := func(t *testing.T, a *Agent) {
		t.Helper()
		require.NoError(t, a.UpdateMockParams(context.Background(), models.MockFilterParams{Exact: &models.ExactServing{}}))
	}

	t.Run("the same call is served, unchecked, once", func(t *testing.T) {
		a, p := keployTestAgent(t, mocks(), nil)
		open(t, a, "TestK", []string{"mock-2"}, aside)
		code, body := p.call(t, "/k")
		require.Equal(t, 200, code)
		require.Equal(t, "k", body)
		code, body = p.call(t, "/pat")
		require.Equal(t, 200, code, "the set-aside test's recording answers the kept test's call that is it exactly")
		require.Equal(t, "pat", body)
		misses, err := p.GetMockErrors(context.Background())
		require.NoError(t, err)
		require.Empty(t, misses)
		_, _, unconsumed, ok := p.mm.ExactTestOutcome()
		require.True(t, ok)
		require.Empty(t, unconsumed, "the kept test owes only its own")
		require.Equal(t, map[string]string{"mock-1": "TestQ"}, asideConsumes(p), "the consume names the set-aside test whose recording it is")
		pool(t, a)

		// The recording answered once: a later test's copy of the call
		// misses, and the later opening needs no Aside of its own.
		open(t, a, "TestL", []string{"mock-3"}, nil)
		require.Equal(t, []int{200}, call(t, p, "/l"))
		require.NotEqual(t, []int{200}, call(t, p, "/pat"), "a set-aside recording answers at most once in the staging")
		misses, err = p.GetMockErrors(context.Background())
		require.NoError(t, err)
		require.Len(t, misses, 1)
		pool(t, a)
	})

	t.Run("a later test is served it when the first after it does not call", func(t *testing.T) {
		a, p := keployTestAgent(t, mocks(), nil)
		open(t, a, "TestK", []string{"mock-2"}, aside)
		require.Equal(t, []int{200}, call(t, p, "/k"))
		pool(t, a)
		open(t, a, "TestL", []string{"mock-3"}, nil)
		require.Equal(t, []int{200, 200}, call(t, p, "/l", "/pat"), "the set-aside recordings stay served for the staging")
		require.Equal(t, map[string]string{"mock-1": "TestQ"}, asideConsumes(p))
		pool(t, a)
	})

	t.Run("a call that is not exactly it still misses", func(t *testing.T) {
		a, p := keployTestAgent(t, mocks(), nil)
		open(t, a, "TestK", []string{"mock-2"}, aside)
		require.Equal(t, 200, call(t, p, "/k")[0])
		require.NotEqual(t, 200, call(t, p, "/pat?fresh=1")[0])
		misses, err := p.GetMockErrors(context.Background())
		require.NoError(t, err)
		require.Len(t, misses, 1)
		require.Empty(t, asideConsumes(p))
		pool(t, a)
	})

	t.Run("between two tests it gets no answer", func(t *testing.T) {
		a, p := keployTestAgent(t, mocks(), nil)
		open(t, a, "TestK", []string{"mock-2"}, aside)
		require.Equal(t, 200, call(t, p, "/k")[0])
		pool(t, a)
		require.NotEqual(t, 200, call(t, p, "/pat")[0], "the pool serves no set-aside recording")
		require.Empty(t, asideConsumes(p))
	})

	t.Run("an opening that names none serves none, as before", func(t *testing.T) {
		a, p := keployTestAgent(t, mocks(), nil)
		open(t, a, "TestK", []string{"mock-2"}, nil)
		require.Equal(t, 200, call(t, p, "/k")[0])
		require.NotEqual(t, 200, call(t, p, "/pat")[0])
		require.Empty(t, asideConsumes(p))
		pool(t, a)
	})
}
