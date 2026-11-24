package manager

import (
	"fmt"
	"reflect"
	"testing"

	"go.uber.org/zap"
)

// A session's tally counts the requests over the size limit noted while it is
// open, in all and by call, and what came before it or after its Close is not
// its own. Each tally open beside it counts each request too. NoteOverSizeLimit
// says how many the manager has counted over its life, so its caller can warn
// of the first as it comes.
func TestOverSizeTallyCountsItsSessionsRequestsByCall(t *testing.T) {
	m := New(zap.NewNop())
	if got := m.NoteOverSizeLimit("POST /{} [path tag #1]"); got != 1 {
		t.Fatalf("the first request over the size limit is counted as %d so far, want 1", got)
	}
	tally := m.OpenOverSizeTally()
	other := m.OpenOverSizeTally()
	for i, call := range []string{"POST /{} [path tag #1]", "GET /{}/{} [path tag #2]", "POST /{} [path tag #1]"} {
		if got := m.NoteOverSizeLimit(call); got != uint64(i+2) {
			t.Fatalf("request %d over the size limit is counted as %d so far, want %d", i+2, got, i+2)
		}
	}
	tally.Close()
	tally.Close() // idempotent
	m.NoteOverSizeLimit("PUT /{} [path tag #3]")
	want := OverSizeSkips{Total: 3, ByCall: map[string]uint64{"POST /{} [path tag #1]": 2, "GET /{}/{} [path tag #2]": 1}}
	if got := tally.Read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the session's requests over the size limit = %+v, want %+v", got, want)
	}
	if most := tally.Read().Most(10); !reflect.DeepEqual(most, []string{"POST /{} [path tag #1] (2)", "GET /{}/{} [path tag #2] (1)"}) {
		t.Fatalf("the calls with the most = %q", most)
	}
	if got := other.Read(); got.Total != 4 || got.ByCall["PUT /{} [path tag #3]"] != 1 {
		t.Fatalf("the tally open beside it counted %+v, want the 4 requests since it opened", got)
	}
	if n := m.OpenOverSizeTallies(); n != 1 {
		t.Fatalf("%d tallies open, want the one not closed yet", n)
	}
	other.Close()
	if n := m.OpenOverSizeTallies(); n != 0 {
		t.Fatalf("%d tallies open after both closed", n)
	}
	if n := m.OverSizeLimitCount(); n != 5 {
		t.Fatalf("the manager's life counts %d requests over the size limit, want 5", n)
	}
}

// A session's calls kept apart are bounded: past maxOverSizeLimitCalls a call
// not seen yet is counted in the total and in Other only, and the summary's
// list says how many requests the calls it does not name hold, so the list's
// counts add up to the total. The bound is the session's: a later session
// names its own calls, however many came before it.
func TestOverSizeTallyKeepsABoundedSetOfCallsPerSession(t *testing.T) {
	m := New(zap.NewNop())
	first := m.OpenOverSizeTally()
	for i := 0; i < maxOverSizeLimitCalls+5; i++ {
		m.NoteOverSizeLimit(fmt.Sprintf("GET /{} [path tag #%d]", i))
	}
	m.NoteOverSizeLimit("GET /{} [path tag #0]") // kept apart: counted on its call
	first.Close()
	s := first.Read()
	if s.Total != maxOverSizeLimitCalls+6 || len(s.ByCall) != maxOverSizeLimitCalls || s.Other != 5 {
		t.Fatalf("total %d, calls kept %d, other %d; want %d, %d, 5", s.Total, len(s.ByCall), s.Other, maxOverSizeLimitCalls+6, maxOverSizeLimitCalls)
	}
	most := s.Most(2)
	if len(most) != 3 || most[0] != "GET /{} [path tag #0] (2)" {
		t.Fatalf("the calls with the most = %q, want #0's 2 first, one more, then the rest counted", most)
	}
	if want := fmt.Sprintf("%d more requests of other calls", maxOverSizeLimitCalls+6-2-1); most[2] != want {
		t.Fatalf("the list ends %q, want %q", most[2], want)
	}
	next := m.OpenOverSizeTally()
	defer next.Close()
	m.NoteOverSizeLimit("POST /{}/{} [path tag #new]")
	if got := next.Read(); got.Total != 1 || got.ByCall["POST /{}/{} [path tag #new]"] != 1 || got.Other != 0 {
		t.Fatalf("a later session counted %+v, want its one call named", got)
	}
}

// A nil manager counts nothing and says it counted none, and a nil tally reads
// nothing.
func TestOverSizeLimitOfANilManager(t *testing.T) {
	var m *SyncMockManager
	if got := m.NoteOverSizeLimit("GET /{}"); got != 0 {
		t.Fatalf("a nil manager counted %d", got)
	}
	if n := m.OverSizeLimitCount(); n != 0 {
		t.Fatalf("a nil manager reads %d", n)
	}
	tally := m.OpenOverSizeTally()
	tally.Close()
	if n := m.OpenOverSizeTallies(); n != 0 {
		t.Fatalf("a nil manager has %d tallies open", n)
	}
	if s := tally.Read(); s.Total != 0 || s.ByCall != nil || s.Other != 0 {
		t.Fatalf("a nil tally reads %+v", s)
	}
}
