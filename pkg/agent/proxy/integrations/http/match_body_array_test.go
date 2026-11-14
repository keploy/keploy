package http

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// bodyMatches runs the shape step on a recorded body and a replayed one, with
// no pool to read redacted places from.
func bodyMatches(t *testing.T, recorded, replayed string) (bool, error) {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	mock := &models.Mock{Name: "recorded", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Body: recorded}}}
	return h.bodyMatch(mock, []byte(replayed), func(*models.Mock) redactedPlaces { return unreadPlaces() })
}

// A batch call, a JSON-RPC batch and a JSON Patch all send an array body. A
// value changing in it is what the fuzzy comparison after the shape step
// weighs, so the shape step has to read an array instead of refusing it.
func TestBodyMatch_AnArrayBodyMatchesWhenAValueChanged(t *testing.T) {
	recorded := `[{"id":1,"at":"2026-11-01T10:00:00Z"},{"id":2,"at":"2026-11-01T10:00:01Z"}]`
	replayed := `[{"id":1,"at":"2026-11-04T09:00:00Z"},{"id":2,"at":"2026-11-04T09:00:01Z"}]`

	ok, err := bodyMatches(t, recorded, replayed)
	require.NoError(t, err)
	assert.True(t, ok, "the values changed, the shape did not")
}

// The shape step still refuses an array that lost an element or a key, and one
// the request answers with something that is not an array.
func TestBodyMatch_ArrayShapesAreStillRefused(t *testing.T) {
	recorded := `[{"id":1,"at":"A"},{"id":2,"at":"B"}]`

	for _, replayed := range []string{
		`[{"id":1,"at":"A"}]`,
		`[{"id":1,"at":"A"},{"id":2}]`,
		`{"0":{"id":1,"at":"A"},"1":{"id":2,"at":"B"}}`,
		`"an array was expected"`,
	} {
		ok, err := bodyMatches(t, recorded, replayed)
		require.NoError(t, err)
		assert.False(t, ok, replayed)
	}
}

// An object body keeps the one-level key check it has always had: a key the
// record holds and the request does not refuses the mock, a value does not.
func TestBodyMatch_AnObjectBodyKeepsItsKeyCheck(t *testing.T) {
	ok, err := bodyMatches(t, `{"id":1,"at":"A"}`, `{"id":9,"at":"Z","extra":true}`)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = bodyMatches(t, `{"id":1,"at":"A"}`, `{"id":1}`)
	require.NoError(t, err)
	assert.False(t, ok)
}

// One mock whose body will not decode used to end the loop, so no mock after it
// was considered. It is skipped now.
func TestPerformBodyMatch_ACandidateThatWillNotDecodeDoesNotDiscardTheRest(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	mocks := []*models.Mock{
		{Name: "undecodable", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Body: `{not json`}}},
		{Name: "array", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Body: `[{"id":1,"at":"A"}]`}}},
	}

	got, err := h.PerformBodyMatch(context.Background(), mocks, []byte(`[{"id":1,"at":"Z"}]`))
	require.NoError(t, err)
	require.Len(t, got, 1, "the candidate after the undecodable one is still considered")
	assert.Equal(t, "array", got[0].Name)
}
