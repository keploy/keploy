package manager

import (
	"reflect"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// mocksize.go sizes a mock for the hold's byte budget (MaxHeldBytes).
//
// A mock is whatever its parser built: an HTTP exchange, a MySQL result set of
// typed rows, a BSON document, opaque payloads. So its size is taken from the
// value itself, by walking it, rather than from one sum per kind that would
// read 0 for the kinds it does not know: the budget has to hold for the mocks
// that are large, and those are the database ones.

// mockSizeNodes bounds the values one walk visits. A mock past it (about a
// million strings, cells or map entries: tens of megabytes) is sized as the
// hold's whole budget, so the walk's cost is bounded and the mock is never
// held as if it were small.
const mockSizeNodes = 1 << 20

// mockSizeDepth bounds the walk's depth: no mock nests deeper, and a cycle
// (none is known) ends there instead of running to mockSizeNodes.
const mockSizeDepth = 64

// mapEntryOverhead is about what a Go map costs per entry beyond its key and
// value: the control byte, the group's padding and the table's spare capacity.
const mapEntryOverhead = 16

var (
	timeType      = reflect.TypeOf(time.Time{})
	locationType  = reflect.TypeOf((*time.Location)(nil))
	stringMapType = reflect.TypeOf(map[string]string(nil))
)

// mockSize is about how many bytes of heap mk holds: the Mock and everything
// only it references. It is an estimate (allocator size classes and a map's
// spare capacity are not known here) that open_window_size_test.go holds
// within a factor of the heap really used, for small HTTP mocks and for large
// database ones. A nil mock is 0.
func mockSize(mk *models.Mock) int64 {
	if mk == nil {
		return 0
	}
	v := reflect.ValueOf(mk).Elem()
	s := sizer{left: mockSizeNodes}
	n := int64(v.Type().Size()) + s.refs(v, 0)
	if s.left <= 0 {
		return max(n, MaxHeldBytes)
	}
	return n
}

// sizer walks one value; left is how many more values it may visit.
type sizer struct{ left int }

// refs is the bytes v references beyond its own inline size: what its strings,
// slices, maps, pointers and interfaces point at, all the way down.
func (s *sizer) refs(v reflect.Value, depth int) int64 {
	if s.left--; s.left <= 0 || depth > mockSizeDepth {
		return 0
	}
	switch v.Kind() {
	case reflect.String:
		return int64(v.Len())
	case reflect.Slice:
		if v.IsNil() {
			return 0
		}
		// Its backing array, as far as it is the slice's own: one built by
		// appending has up to twice its length in capacity, and that is heap
		// the mock holds. One that reaches further is a view into a buffer
		// something else built (a read buffer, a pooled one) and other views
		// share, so it is not charged the buffer once per view.
		et := v.Type().Elem()
		n := int64(min(v.Cap(), 2*v.Len())) * int64(et.Size())
		if !flat(et.Kind()) {
			for i := 0; i < v.Len() && s.left > 0; i++ {
				n += s.refs(v.Index(i), depth+1)
			}
		}
		return n
	case reflect.Array:
		var n int64
		if !flat(v.Type().Elem().Kind()) {
			for i := 0; i < v.Len() && s.left > 0; i++ {
				n += s.refs(v.Index(i), depth+1)
			}
		}
		return n
	case reflect.Pointer:
		// A time's Location is shared by every time of the process.
		if v.IsNil() || v.Type() == locationType {
			return 0
		}
		return int64(v.Type().Elem().Size()) + s.refs(v.Elem(), depth+1)
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		// The boxed value, and what it references.
		e := v.Elem()
		return int64(e.Type().Size()) + s.refs(e, depth+1)
	case reflect.Map:
		if v.IsNil() {
			return 0
		}
		t := v.Type()
		n := int64(v.Len()) * (int64(t.Key().Size()) + int64(t.Elem().Size()) + mapEntryOverhead)
		if flat(t.Key().Kind()) && flat(t.Elem().Kind()) {
			return n
		}
		// Headers and metadata, the maps every mock has: read as they are,
		// without a reflected iterator's allocations.
		if t == stringMapType && v.CanInterface() {
			m := v.Interface().(map[string]string)
			s.left -= 2 * len(m)
			for k, e := range m {
				n += int64(len(k) + len(e))
			}
			return n
		}
		for it := v.MapRange(); it.Next() && s.left > 0; {
			n += s.refs(it.Key(), depth+1) + s.refs(it.Value(), depth+1)
		}
		return n
	case reflect.Struct:
		if v.Type() == timeType {
			return 0
		}
		var n int64
		for i := 0; i < v.NumField(); i++ {
			n += s.refs(v.Field(i), depth+1)
		}
		return n
	}
	return 0
}

// flat reports whether a value of kind k references nothing: its inline size
// is all it takes, so a slice of them is not walked element by element.
func flat(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}
