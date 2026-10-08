package models

import (
	"errors"
	"reflect"
	"strings"
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

// WithResponse hands back a mock that carries its response without ever
// writing the mock it was asked about: that one is pooled, and other
// connections may be matching or copying it.
func TestWithResponseLoadsOntoACopy(t *testing.T) {
	resident := &Mock{Name: "resident", Spec: MockSpec{HTTPResp: &HTTPResp{StatusCode: 200}}}
	if got, err := resident.WithResponse(); got != resident || err != nil {
		t.Fatalf("a mock that holds its response is returned as it is: %v, %v", got, err)
	}
	if got, err := (*Mock)(nil).WithResponse(); got != nil || err != nil {
		t.Fatalf("no mock, no response: %v, %v", got, err)
	}

	loads := 0
	spilled := &Mock{Name: "spilled"}
	spilled.SetResponseHydrator(func() (*HTTPResp, []MongoResponse, error) {
		loads++
		return &HTTPResp{StatusCode: 201, Body: "loaded"}, nil, nil
	})
	spilled.MarkPooled()
	for i := 1; i <= 2; i++ {
		got, err := spilled.WithResponse()
		if err != nil || got == spilled || got.Spec.HTTPResp == nil || got.Spec.HTTPResp.Body != "loaded" {
			t.Fatalf("call %d: want a copy with the response loaded: %+v, %v", i, got, err)
		}
		if spilled.Spec.HTTPResp != nil || !spilled.HasSpilledResponse() {
			t.Fatalf("call %d wrote the response into the pooled mock", i)
		}
		if got.HasSpilledResponse() || got.Pooled() {
			t.Fatalf("call %d: the copy is loaded, and no pool holds it", i)
		}
		if loads != i {
			t.Fatalf("call %d loaded the response %d times", i, loads)
		}
	}

	broken := &Mock{Name: "broken"}
	broken.SetResponseHydrator(func() (*HTTPResp, []MongoResponse, error) { return nil, nil, errors.New("store closed") })
	got, err := broken.WithResponse()
	if got != nil || err == nil || !strings.Contains(err.Error(), `"broken"`) || !strings.Contains(err.Error(), "store closed") {
		t.Fatalf("a response that cannot be loaded is an error naming the mock: %v, %v", got, err)
	}
	if !broken.HasSpilledResponse() {
		t.Fatal("a failed load must leave the mock as it was")
	}
}
