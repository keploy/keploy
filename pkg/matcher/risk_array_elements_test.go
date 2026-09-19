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

	assert.NotEmpty(t, assess.RemovedFields)
	assert.NotEqual(t, models.None, assess.Risk)
}

func TestChangedJSONFieldPaths_ArrayElementsReportOnePath(t *testing.T) {
	paths := ChangedJSONFieldPaths(
		`{"items":["a","b"]}`,
		`{"items":["y","z"]}`, nil, nil)

	assert.Equal(t, []string{"items[]"}, paths)
}
