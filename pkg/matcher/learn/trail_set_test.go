package learn

import (
	"fmt"
	"testing"
	"time"
)

// A call's places read the list of the test it trails (Input.Trails) once
// (trailSet), however many places the call has: a call of a long test lists
// thousands of places, and the trailed test's list can hold thousands of
// recordings. Asked again at each place, the list was read up to places x
// recordings times a call. The count is of names read (trailReads), never
// of time. Each place is trailed exactly where the list names its
// recording, as before; a trail that names no test reads nothing and
// trails no place.
func TestACallsPlacesReadTheTrailedTestsListOnce(t *testing.T) {
	const exchanges, recordings, others = 300, 3000, 900
	in := oInput("body.user", "r", "f", nil, nil)
	listed := map[string]bool{}
	var trail []string
	for i := range exchanges {
		e := oSent("rAHcEraS7Qx1", "Zq81Lm0XpVb4")[0]
		e.Mock = fmt.Sprintf("mock-e%d", i)
		in.Earlier = append(in.Earlier, e)
		if i%3 == 0 {
			listed[e.Mock] = true
			trail = append(trail, e.Mock)
		}
	}
	for i := range recordings {
		name := fmt.Sprintf("mock-r%d", i)
		in.Raw = append(in.Raw, RawOf(getAnswer(name, "http://api/items", `{"v":"x"}`, oAt.Add(-time.Hour)), false))
		if i%3 == 0 {
			listed[name] = true
			trail = append(trail, name)
		}
	}
	for i := range others {
		trail = append(trail, fmt.Sprintf("mock-z%d", i)) // recordings of the trailed test the call has no place of
	}
	in.Trails = Trail{Test: in.Test, Mocks: trail}

	read := trailReads.Load()
	ps, _, _ := placesOf(in)
	read = trailReads.Load() - read
	if ps.len() != exchanges+recordings {
		t.Fatalf("placesOf listed %d places; want %d (an exchange's one leaf each, and each recording)", ps.len(), exchanges+recordings)
	}
	t.Logf("%d places, a trail of %d recordings: %d names read", ps.len(), len(trail), read)
	if read != int64(len(trail)) {
		t.Errorf("a call's %d places read %d names of the trailed test's %d; want the list read once (%d)", ps.len(), read, len(trail), len(trail))
	}
	trailed := 0
	for _, p := range ps.all() {
		if p.trailed != listed[p.mock] {
			t.Errorf("%s: trailed %v; want %v (the trailed test's list names it: %v)", p.name, p.trailed, listed[p.mock], listed[p.mock])
		}
		if p.trailed {
			trailed++
		}
	}
	if want := exchanges/3 + recordings/3; trailed != want {
		t.Errorf("%d places trailed; want %d, the ones the list names", trailed, want)
	}

	// A trail that names no test trails none, whatever its list holds, and
	// reads no name of it.
	in.Trails.Test = ""
	read = trailReads.Load()
	ps, _, _ = placesOf(in)
	if read = trailReads.Load() - read; read != 0 {
		t.Errorf("a trail that names no test read %d names; want none", read)
	}
	for _, p := range ps.all() {
		if p.trailed {
			t.Errorf("%s trailed by a trail that names no test", p.name)
		}
	}
}
