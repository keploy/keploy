package models

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
)

// The decoded wire messages of the database protocols are the parser's:
// DeepCopy shares them (see DeepCopy). The rows below neither edit them on a
// copy nor fill what sits behind an interface.
var sharedWirePayloads = map[reflect.Type]bool{
	reflect.TypeOf(MongoRequest{}):      true,
	reflect.TypeOf(MongoResponse{}):     true,
	reflect.TypeOf(mysql.Request{}):     true,
	reflect.TypeOf(mysql.Response{}):    true,
	reflect.TypeOf(postgres.Request{}):  true,
	reflect.TypeOf(postgres.Response{}): true,
	reflect.TypeOf(PostgresV3Spec{}):    true,
}

var timeType = reflect.TypeOf(time.Time{})

// fillAll sets every exported field reachable from v to a value other than
// its zero, the same for the same seed: a map gets one entry, a slice one
// element, a pointer a filled value. An interface is left nil.
func fillAll(v reflect.Value, seed string, depth int) {
	if depth > 16 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(seed)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(len(seed)%100 + 1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(uint64(len(seed)%100 + 1))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(len(seed)) + 0.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillAll(p.Elem(), seed, depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillAll(s.Index(0), seed+"[0]", depth+1)
		v.Set(s)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fillAll(v.Index(i), fmt.Sprintf("%s[%d]", seed, i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillAll(k, seed+".key", depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fillAll(e, seed+".value", depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, len(seed), time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				fillAll(v.Field(i), seed+"."+f.Name, depth+1)
			}
		}
	}
}

// editAll changes every exported value reachable from v, in place: each
// string, number, bool and time, each map entry (plus one new entry), each
// slice element, through each pointer. It does not edit sharedWirePayloads
// or what sits behind an interface.
func editAll(v reflect.Value, depth int) {
	if depth > 16 || sharedWirePayloads[v.Type()] {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-edited")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(v.Uint() + 7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 7)
	case reflect.Pointer:
		if !v.IsNil() {
			editAll(v.Elem(), depth+1)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			editAll(v.Index(i), depth+1)
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			editAll(e, depth+1)
			v.SetMapIndex(k, e)
		}
		k := reflect.New(v.Type().Key()).Elem()
		fillAll(k, "added-by-the-copy", 0)
		e := reflect.New(v.Type().Elem()).Elem()
		fillAll(e, "added-by-the-copy", 0)
		v.SetMapIndex(k, e)
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				editAll(v.Field(i), depth+1)
			}
		}
	}
}

// diffAll is the paths of the exported values reachable from a and b at
// which they differ.
func diffAll(a, b reflect.Value, path string, out *[]string) {
	if a.Kind() != b.Kind() {
		*out = append(*out, path)
		return
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				*out = append(*out, path)
			}
			return
		}
		diffAll(a.Elem(), b.Elem(), path, out)
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() || (a.Kind() == reflect.Slice && a.IsNil() != b.IsNil()) {
			*out = append(*out, path)
			return
		}
		for i := 0; i < a.Len(); i++ {
			diffAll(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.Map:
		if a.Len() != b.Len() || a.IsNil() != b.IsNil() {
			*out = append(*out, path)
			return
		}
		for _, k := range a.MapKeys() {
			bv := b.MapIndex(k)
			if !bv.IsValid() {
				*out = append(*out, fmt.Sprintf("%s[%v]", path, k))
				continue
			}
			diffAll(a.MapIndex(k), bv, fmt.Sprintf("%s[%v]", path, k), out)
		}
	case reflect.Struct:
		if a.Type() == timeType {
			if !a.Interface().(time.Time).Equal(b.Interface().(time.Time)) {
				*out = append(*out, path)
			}
			return
		}
		for i := 0; i < a.NumField(); i++ {
			if f := a.Type().Field(i); f.IsExported() {
				diffAll(a.Field(i), b.Field(i), path+"."+f.Name, out)
			}
		}
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
	default:
		if a.Interface() != b.Interface() {
			*out = append(*out, path)
		}
	}
}

// filledMock is a mock with every exported field filled (fillAll), and the
// variables its learn step learned (SetLearned): the same each call.
func filledMock() *Mock {
	m := &Mock{}
	fillAll(reflect.ValueOf(m).Elem(), "mock", 0)
	var vars MockVariables
	fillAll(reflect.ValueOf(&vars).Elem(), "learned", 0)
	binds := map[string]MockVariables{}
	fillAll(reflect.ValueOf(&binds).Elem(), "binds", 0)
	m.Spec.SetLearned(vars, binds)
	return m
}

// A deep copy of a mock holds every field the mock holds: none is left
// behind. Start, StartRef and SourcePID were: a mock recorded with
// record.mockFormat gob (whose writer encodes a copy) lost its start.
func TestDeepCopyCarriesEveryField(t *testing.T) {
	m := filledMock()
	c := m.DeepCopy()
	var diff []string
	diffAll(reflect.ValueOf(c).Elem(), reflect.ValueOf(m).Elem(), "Mock", &diff)
	diffAll(reflect.ValueOf(c.Spec.Learned()), reflect.ValueOf(m.Spec.Learned()), "Mock.Spec.Learned()", &diff)
	diffAll(reflect.ValueOf(c.Spec.LearnedBinds()), reflect.ValueOf(m.Spec.LearnedBinds()), "Mock.Spec.LearnedBinds()", &diff)
	if len(diff) > 0 {
		t.Fatalf("the copy differs from the mock at %d places: %v", len(diff), diff)
	}
}

// A deep copy of a mock shares no map, slice or pointer with it outside the
// database protocols' decoded wire messages: editing every value of the copy
// (each header, form value, stream chunk, gRPC header and message, DNS
// record, generic packet, learned variable...) leaves the mock as it was.
func TestDeepCopySharesNothingEditable(t *testing.T) {
	m, want := filledMock(), filledMock()
	c := m.DeepCopy()
	editAll(reflect.ValueOf(c).Elem(), 0)
	editAll(reflect.ValueOf(c.Spec.Learned()), 0)
	editAll(reflect.ValueOf(c.Spec.LearnedBinds()), 0)

	var diff []string
	diffAll(reflect.ValueOf(m).Elem(), reflect.ValueOf(want).Elem(), "Mock", &diff)
	diffAll(reflect.ValueOf(m.Spec.Learned()), reflect.ValueOf(want.Spec.Learned()), "Mock.Spec.Learned()", &diff)
	diffAll(reflect.ValueOf(m.Spec.LearnedBinds()), reflect.ValueOf(want.Spec.LearnedBinds()), "Mock.Spec.LearnedBinds()", &diff)
	if len(diff) > 0 {
		t.Fatalf("editing the copy changed the mock at %d places: %v", len(diff), diff)
	}

	// The edit reached every place: a row that edits nothing proves nothing.
	var edited []string
	diffAll(reflect.ValueOf(c).Elem(), reflect.ValueOf(want).Elem(), "Mock", &edited)
	for _, p := range []string{"Mock.Spec.HTTPResp.Header", "Mock.Start", "Mock.Spec.HTTPReq.Form[0].Values[0]"} {
		found := false
		for _, e := range edited {
			found = found || e == p
		}
		if !found {
			t.Errorf("the copy's edit did not reach %s: edited %v", p, edited)
		}
	}
}

// The answer's headers are the copy's own: the case that showed the sharing.
// A derived mock that sets a header on its copy of the recorded answer must
// leave the recorded mock's answer as it was recorded.
func TestDeepCopyAnswerHeadersAreTheCopys(t *testing.T) {
	m := &Mock{Kind: HTTP, Spec: MockSpec{
		HTTPReq:  &HTTPReq{Header: map[string]string{"Accept": "application/json"}},
		HTTPResp: &HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}},
	}}
	c := m.DeepCopy()
	c.Spec.HTTPResp.Header["X-Decline-Code"] = "insufficient_funds"
	delete(c.Spec.HTTPResp.Header, "Content-Type")
	c.Spec.HTTPReq.Header["Accept"] = "text/plain"
	if got := m.Spec.HTTPResp.Header; len(got) != 1 || got["Content-Type"] != "application/json" {
		t.Fatalf("recorded answer headers = %v, want only Content-Type: application/json", got)
	}
	if got := m.Spec.HTTPReq.Header["Accept"]; got != "application/json" {
		t.Fatalf("recorded request Accept = %q, want application/json", got)
	}
}

// A deep copy of a mock that leaves its parts' maps and slices nil, or has
// no HTTP or gRPC parts at all, equals it: each nil stays nil, so a copy
// compares equal to what it copied, and a missing part stays missing.
func TestDeepCopyOfASparseMockEqualsIt(t *testing.T) {
	for name, m := range map[string]*Mock{
		"parts with nothing set": {Kind: HTTP, Spec: MockSpec{
			HTTPReq:  &HTTPReq{Method: "POST", Form: []FormData{{Key: "file"}}},
			HTTPResp: &HTTPResp{StatusCode: 204},
			GRPCReq:  &GrpcReq{},
			GRPCResp: &GrpcResp{},
		}},
		"a request without a form": {Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET"}}},
		"no parts":                 {Kind: GENERIC, Spec: MockSpec{GenericRequests: []Payload{{}}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := m.DeepCopy()
			// DeepCopy has always made each protocol's list of requests and
			// responses a list, empty for none, where the mock has none:
			// no encoder tells the two apart (each is omitempty).
			for _, list := range []string{"GenericRequests", "GenericResponses", "MongoRequests", "MongoResponses",
				"PostgresRequestsV2", "PostgresResponsesV2", "MySQLRequests", "MySQLResponses"} {
				cv, mv := reflect.ValueOf(&c.Spec).Elem().FieldByName(list), reflect.ValueOf(m.Spec).FieldByName(list)
				if mv.IsNil() && !cv.IsNil() && cv.Len() == 0 {
					cv.Set(mv)
				}
			}
			var diff []string
			diffAll(reflect.ValueOf(c).Elem(), reflect.ValueOf(m).Elem(), "Mock", &diff)
			diffAll(reflect.ValueOf(c.Spec.Learned()), reflect.ValueOf(m.Spec.Learned()), "Mock.Spec.Learned()", &diff)
			diffAll(reflect.ValueOf(c.Spec.LearnedBinds()), reflect.ValueOf(m.Spec.LearnedBinds()), "Mock.Spec.LearnedBinds()", &diff)
			if len(diff) > 0 {
				t.Fatalf("the copy differs from the mock at %v", diff)
			}
		})
	}
}
