package yaml_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// fill sets every exported field reachable from v to a value other than its
// zero, the same each call: a map gets one entry, a slice one element, a
// pointer a filled value. An interface is left nil.
func fill(v reflect.Value, seed string, depth int) {
	if depth > 12 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(seed)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(len(seed)%100 + 1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(len(seed)%100 + 1))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(len(seed)) + 0.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), seed, depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), seed+"[0]", depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, seed+".key", depth+1)
		fill(e, seed+".value", depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, len(seed), time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				fill(v.Field(i), seed+"."+f.Name, depth+1)
			}
		}
	}
}

// roundTrip writes doc in format and reads it back, as the store does.
func roundTrip(t *testing.T, format yaml.Format, doc *yaml.NetworkTrafficDoc) *yaml.NetworkTrafficDoc {
	t.Helper()
	data, err := yaml.MarshalDoc(format, doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := yaml.UnmarshalDoc(format, data)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A test case read back from a JSON file is the one read back from a YAML
// file, field by field: the JSON codec keeps all the YAML codec keeps. Its
// real: list comes back from both; the JSON codec dropped it, so a test case
// stored as JSON lost the destinations its replay must reach.
func TestTestCaseReadsTheSameFromEitherCodec(t *testing.T) {
	t.Setenv(provenswitch.Env, "1") // a test case's real: is read only under it
	logger := zap.NewNop()
	for _, kind := range []models.Kind{models.HTTP, models.GRPC_EXPORT} {
		t.Run(string(kind), func(t *testing.T) {
			var tc models.TestCase
			fill(reflect.ValueOf(&tc).Elem(), "tc", 0)
			tc.Kind, tc.Version, tc.Mocks = kind, models.GetVersion(), nil
			read := map[yaml.Format]*models.TestCase{}
			for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
				doc, err := testdb.EncodeTestcase(tc, logger)
				if err != nil {
					t.Fatal(err)
				}
				if read[format], err = testdb.Decode(roundTrip(t, format, doc), logger); err != nil {
					t.Fatal(err)
				}
				if got := read[format].Real; !reflect.DeepEqual(got, tc.Real) {
					t.Errorf("%s: Real = %#v, want %#v", format, got, tc.Real)
				}
			}
			if !reflect.DeepEqual(read[yaml.FormatJSON], read[yaml.FormatYAML]) {
				t.Errorf("the test case read from JSON differs from the one read from YAML at %v",
					diffPaths(reflect.ValueOf(read[yaml.FormatJSON]).Elem(), reflect.ValueOf(read[yaml.FormatYAML]).Elem(), "TestCase"))
			}
		})
	}
}

// A mock read back from a JSON file is the one read back from a YAML file,
// field by field, for each kind whose spec is plain data. (The database
// protocols' specs hold decoded wire messages behind interfaces, which a
// reflective fill leaves empty; their own round-trip rows cover them.)
func TestMockReadsTheSameFromEitherCodec(t *testing.T) {
	logger := zap.NewNop()
	for _, kind := range []models.Kind{models.HTTP, models.HTTP2, models.GRPC_EXPORT, models.GENERIC, models.DNS, models.ConnectionFailure} {
		t.Run(string(kind), func(t *testing.T) {
			var m models.Mock
			fill(reflect.ValueOf(&m).Elem(), "mock", 0)
			m.Kind, m.Version = kind, models.GetVersion()
			// A failure the reader accepts: it refuses a phase it does not know.
			*m.Spec.ConnFailure = models.ConnFailureSpec{Address: "[2001:db8::10]:443", Host: "api.example.com",
				Phase: models.ConnFailurePhaseConnect, Outcome: models.ConnFailureRefused, Cause: "connect: network is unreachable"}
			read := map[yaml.Format]*models.Mock{}
			for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
				doc, err := mockdb.EncodeMock(m.DeepCopy(), logger)
				if err != nil {
					t.Fatal(err)
				}
				mocks, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{roundTrip(t, format, doc)}, logger)
				if err != nil {
					t.Fatal(err)
				}
				if len(mocks) != 1 {
					t.Fatalf("%s: read %d mocks, want 1", format, len(mocks))
				}
				read[format] = mocks[0]
			}
			if !reflect.DeepEqual(read[yaml.FormatJSON], read[yaml.FormatYAML]) {
				t.Errorf("the mock read from JSON differs from the one read from YAML at %v",
					diffPaths(reflect.ValueOf(read[yaml.FormatJSON]).Elem(), reflect.ValueOf(read[yaml.FormatYAML]).Elem(), "Mock"))
			}
		})
	}
}

// diffPaths is the paths of the values reachable from a and b at which they
// differ, unexported fields included.
func diffPaths(a, b reflect.Value, path string) []string {
	if a.Kind() != b.Kind() || a.Type() != b.Type() {
		return []string{path}
	}
	var out []string
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				out = append(out, path)
			}
			return out
		}
		return diffPaths(a.Elem(), b.Elem(), path)
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() || (a.Kind() == reflect.Slice && a.IsNil() != b.IsNil()) {
			return []string{path}
		}
		for i := 0; i < a.Len(); i++ {
			out = append(out, diffPaths(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	case reflect.Map:
		if a.Len() != b.Len() || a.IsNil() != b.IsNil() {
			return []string{path}
		}
		for _, k := range a.MapKeys() {
			if bv := b.MapIndex(k); !bv.IsValid() {
				out = append(out, fmt.Sprintf("%s[%v]", path, k))
			} else {
				out = append(out, diffPaths(a.MapIndex(k), bv, fmt.Sprintf("%s[%v]", path, k))...)
			}
		}
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			out = append(out, diffPaths(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)...)
		}
	default:
		if !reflect.DeepEqual(valueOf(a), valueOf(b)) {
			out = append(out, path)
		}
	}
	return out
}

// valueOf is v's value as text, readable for an unexported field too.
func valueOf(v reflect.Value) string {
	return fmt.Sprint(v)
}
