package matcher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func TestComputeFailureAssessmentJSON_ArrayElementValueChange(t *testing.T) {
	assess, err := ComputeFailureAssessmentJSON(
		`{"items":["a","b"],"x":1}`,
		`{"items":["z","b"],"x":1,"newField":2}`, nil, false)
	require.NoError(t, err)
	require.NotNil(t, assess)

	assert.Equal(t, []string{"items[]"}, assess.ValueChanges)
	assert.Equal(t, []string{"newField"}, assess.AddedFields)
	assert.NotEqual(t, models.Low, assess.Risk)
}

func TestComputeFailureAssessmentJSON_ArrayTruncation(t *testing.T) {
	assess, err := ComputeFailureAssessmentJSON(
		`{"items":["a","b","c"]}`,
		`{"items":["c"]}`, nil, false)
	require.NoError(t, err)
	require.NotNil(t, assess)

	assert.Empty(t, assess.RemovedFields)
	assert.Equal(t, []string{"items[]"}, assess.ValueChanges)
	assert.NotEqual(t, models.None, assess.Risk)
}

func TestComputeFailureAssessmentJSON_ArrayLengthChangeIsNotAddedField(t *testing.T) {
	for _, tc := range []struct{ exp, act, path string }{
		{`{"items":["a"]}`, `{"items":["a","b"]}`, "items[]"},
		{`{"users":[{"id":1},{"id":2}]}`, `{"users":[{"id":1},{"id":2},{"id":3}]}`, "users[].id"},
		{`{"users":[{"id":1},{"id":2}]}`, `{"users":[{"id":1}]}`, "users[].id"},
	} {
		assess, err := ComputeFailureAssessmentJSON(tc.exp, tc.act, nil, false)
		require.NoError(t, err)
		require.NotNil(t, assess)

		assert.Empty(t, assess.AddedFields, tc.act)
		assert.Empty(t, assess.RemovedFields, tc.act)
		assert.Equal(t, []string{tc.path}, assess.ValueChanges, tc.act)
		assert.NotEqual(t, models.Low, assess.Risk, tc.act)
	}
}

func TestComputeFailureAssessmentJSON_FieldAddedInArrayElements(t *testing.T) {
	assess, err := ComputeFailureAssessmentJSON(
		`{"users":[{"id":1},{"id":2}]}`,
		`{"users":[{"id":1,"n":1},{"id":2,"n":2}]}`, nil, false)
	require.NoError(t, err)
	require.NotNil(t, assess)

	assert.Equal(t, []string{"users[].n"}, assess.AddedFields)
	assert.Empty(t, assess.ValueChanges)
}

func TestChangedJSONFieldPaths_ArrayElementsReportOnePath(t *testing.T) {
	paths := ChangedJSONFieldPaths(
		`{"items":["a","b"]}`,
		`{"items":["y","z"]}`, nil, nil)

	assert.Equal(t, []string{"items[]"}, paths)
}
