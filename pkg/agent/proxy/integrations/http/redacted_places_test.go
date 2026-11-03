package http

import "testing"

// A body field is redacted when the recording redacted it, a field inside
// it, or the whole body.
func TestRedactedPlacesFieldIsARedactedFieldOrInsideOne(t *testing.T) {
	p := redactedPlaces{fields: map[string]bool{"a": true}}
	for name, want := range map[string]bool{"a": true, "a.b": true, "a[]": true, "a[].b": true, "ab": false, "b": false} {
		if got := p.field(name); got != want {
			t.Errorf("field(%q) = %v, want %v", name, got, want)
		}
	}
	if !(redactedPlaces{body: true}).field("x") {
		t.Error("a field of a body the recording redacted whole is not redacted")
	}
}

// No mock is no place redacted, not a crash.
func TestPlacesOfNoMockIsNoPlaces(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("placesOf(nil) panicked: %v", r)
		}
	}()
	p := placesOf(nil)
	if p.body || len(p.fields) > 0 || p.decodedPath("/x/y") != "/x/y" {
		t.Errorf("placesOf(nil) = %+v, want no places", p)
	}
}
