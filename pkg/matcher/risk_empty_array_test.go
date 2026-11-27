package matcher

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// AN EMPTY ARRAY RECORDS NO ELEMENT PATH, SO GAINING ELEMENTS IS DECIDED BY
// THE CONTAINMENT CHECK, NOT BY THE PATH MAPS. These pin what that comes to,
// for every element type: an element showing up where the recording had none
// is MEDIUM (the containment check finds the recording is not held element for
// element, so it no longer takes the additive auto-pass of #5370), losing the
// elements an array held is HIGH, and two empty arrays are identical.
func TestComputeFailureAssessmentJSON_AnArrayThatWasEmpty(t *testing.T) {
	for _, c := range []struct {
		name     string
		exp, act string
		want     models.RiskLevel
		removed  []string
	}{
		{
			name: "an empty array gains an object element",
			exp:  `{"items":[]}`,
			act:  `{"items":[{"id":1}]}`,
			want: models.Medium,
		},
		{
			name: "an empty array gains a scalar element",
			exp:  `{"items":[]}`,
			act:  `{"items":["a"]}`,
			want: models.Medium,
		},
		{
			name: "an array loses its last scalar element",
			exp:  `{"items":["a"]}`,
			act:  `{"items":[]}`,
			want: models.Medium,
		},
		{
			name:    "an array loses its last object element",
			exp:     `{"items":[{"id":1}]}`,
			act:     `{"items":[]}`,
			want:    models.High,
			removed: []string{"items[].id"},
		},
		{
			name: "two empty arrays are identical",
			exp:  `{"items":[]}`,
			act:  `{"items":[]}`,
			want: models.None,
		},
		{
			name: "an empty array beside an unchanged field gains an element",
			exp:  `{"items":[],"k":1}`,
			act:  `{"items":[{"id":1}],"k":1}`,
			want: models.Medium,
		},
		{
			name: "an empty array of arrays gains an element",
			exp:  `{"items":[]}`,
			act:  `{"items":[["a"]]}`,
			want: models.Medium,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := ComputeFailureAssessmentJSON(c.exp, c.act, nil, false)
			if err != nil || a == nil {
				t.Fatalf("assessment: %v, %v", a, err)
			}
			if a.Risk != c.want {
				t.Errorf("risk %s, want %s (reasons %v, category %v, added %v, removed %v, changed %v)",
					a.Risk, c.want, a.Reasons, a.Category, a.AddedFields, a.RemovedFields, a.ValueChanges)
			}
			if c.removed != nil {
				if len(a.RemovedFields) != len(c.removed) || (len(c.removed) > 0 && a.RemovedFields[0] != c.removed[0]) {
					t.Errorf("removed %v, want %v", a.RemovedFields, c.removed)
				}
			}
		})
	}
}
