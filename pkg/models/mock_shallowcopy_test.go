package models

import (
	"reflect"
	"testing"
	"time"
)

// ShallowCopy is how the replay mock manager stages a mock again once a pool
// holds it: the copy must carry everything but the pooled mark, which belongs
// to the object a pool stored.
func TestShallowCopyIsAnUnpooledCopy(t *testing.T) {
	m := &Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "m",
		Kind:    MySQL,
		Spec: MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: time.Unix(1, 0),
		},
		TestModeInfo: TestModeInfo{ID: 3, IsFiltered: true, SortOrder: 7, Lifetime: LifetimeSession,
			LifetimeDerived: true, IsStartup: true},
		ConnectionID: "c1",
		SourcePID:    42,
		Noise:        []string{"x"},
	}
	m.SetResponseHydrator(func() (*HTTPResp, []MongoResponse, error) { return nil, nil, nil })
	m.MarkPooled()

	c := m.ShallowCopy()
	if c == m {
		t.Fatal("ShallowCopy returned the same object")
	}
	if c.Pooled() {
		t.Error("the copy is marked pooled; it is a new object no pool holds")
	}
	if !m.Pooled() {
		t.Error("copying cleared the original's pooled mark")
	}
	if !c.HasSpilledResponse() {
		t.Error("the response hydrator was not carried over")
	}
	cv, mv := reflect.ValueOf(*c), reflect.ValueOf(*m)
	for i := 0; i < cv.NumField(); i++ {
		switch name := cv.Type().Field(i).Name; name {
		case "pooled", "responseHydrator": // checked above
		default:
			if !reflect.DeepEqual(cv.Field(i).Interface(), mv.Field(i).Interface()) {
				t.Errorf("field %s was not copied", name)
			}
		}
	}
	c.TestModeInfo.ID = 99
	if m.TestModeInfo.ID != 3 {
		t.Error("the copy shares TestModeInfo with the original")
	}
}
