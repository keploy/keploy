package models

import "testing"

// A test's repeated reads mark copies of its recordings, never the store's.
// The recordings of one read share one mark, two reads named alike carry two
// marks, and a recording two reads name carries the later one's.
func TestRepeatedReadsMarkCopiesOfTheTestsRecordingsOneMarkPerRead(t *testing.T) {
	m1, m2, m3, m4, m5 := &Mock{Name: "mock-1"}, &Mock{Name: "mock-2"}, &Mock{Name: "mock-3"}, &Mock{Name: "mock-4"}, &Mock{Name: "mock-5"}
	owned := []*Mock{m1, nil, m2, m3, m4, m5}
	var got []*Mock
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("MarkRepeatReads panicked: %v", r)
			}
		}()
		got = MarkRepeatReads(owned, []RepeatRead{
			{Name: "onboarding", Mocks: []string{"mock-1", "mock-3"}},
			{Name: "list", Mocks: []string{"mock-3", "mock-9"}},
			{Name: "list", Mocks: []string{"mock-4", "mock-5"}},
		})
	}()
	if len(got) != 6 || got[1] != nil || got[2] != m2 {
		t.Fatalf("an unnamed recording is not served as it is: %v", got)
	}
	read := func(i int) RepeatReadMark { return got[i].TestModeInfo.ReadMark() }
	if got[0] == m1 || read(0) != (RepeatReadMark{Name: "onboarding", At: 1}) || got[3] == m3 || read(3) != (RepeatReadMark{Name: "list", At: 2}) {
		t.Fatalf("named recordings are not marked copies: %+v %+v", got[0].TestModeInfo, got[3].TestModeInfo)
	}
	if read(4) != (RepeatReadMark{Name: "list", At: 3}) || read(5) != read(4) {
		t.Fatalf("the recordings of one read carry other marks: %+v %+v", read(4), read(5))
	}
	if m1.TestModeInfo.RepeatRead.Marked() || m3.TestModeInfo.RepeatRead.Marked() || got[2].TestModeInfo.RepeatRead.Marked() {
		t.Fatal("the store's recordings were marked")
	}
	if same := MarkRepeatReads(owned, nil); &same[0] != &owned[0] {
		t.Fatal("a test with no repeated read is not served its recordings as they are")
	}
	if (&RepeatReadMark{Name: "list"}).Marked() || !(&RepeatReadMark{At: 1}).Marked() || (*RepeatReadMark)(nil).Marked() {
		t.Fatal("a mark is told by its place, not its name, and a nil mark marks none")
	}
}

// A recording no read names carries no mark and reads as the zero mark,
// which marks no read. The recordings of one read share its one mark, so
// a Mock carries a read at the cost of a pointer.
func TestARecordingNoReadNamesReadsAsTheZeroMark(t *testing.T) {
	var info TestModeInfo
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("reading a recording no read names panicked: %v", r)
			}
		}()
		if r := info.ReadMark(); r != (RepeatReadMark{}) || r.Marked() || info.RepeatRead.Marked() {
			t.Fatalf("a recording no read names reads as %+v", r)
		}
	}()
	got := MarkRepeatReads([]*Mock{{Name: "m1"}, {Name: "m2"}}, []RepeatRead{{Name: "list", Mocks: []string{"m1", "m2"}}})
	if a, b := got[0].TestModeInfo.RepeatRead, got[1].TestModeInfo.RepeatRead; a == nil || a != b || got[0].TestModeInfo.ReadMark() != (RepeatReadMark{Name: "list", At: 1}) {
		t.Fatalf("the recordings of one read carry %p and %p, read as %+v", a, b, got[0].TestModeInfo.ReadMark())
	}
}
