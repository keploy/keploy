package models

import (
	"reflect"
	"testing"
	"time"
)

// answerPlacedPoll registers answer placement for the HTTP mocks whose
// metadata names them a poll, for the test's life.
func answerPlacedPoll(t *testing.T) {
	t.Helper()
	t.Cleanup(RegisterAnswerPlaced(HTTP, func(m *Mock) bool { return m.Spec.Metadata["poll"] == "yes" }))
}

// placeMock is a per-test HTTP mock asked at req ms and answered at res ms
// (wmAt), a poll when poll is set.
func placeMock(name string, req, res int, poll bool) *Mock {
	meta := map[string]string{"type": "mocks"}
	if poll {
		meta["poll"] = "yes"
	}
	return &Mock{Name: name, Kind: HTTP, Spec: MockSpec{Metadata: meta, ReqTimestampMock: wmAt(req), ResTimestampMock: wmAt(res)},
		TestModeInfo: TestModeInfo{Lifetime: LifetimePerTest, LifetimeDerived: true}}
}

// A per-test mock a registration accepts is placed at its answer; every other
// is placed at its request: one no registration accepts, a session or
// connection mock, a carry-over one (released by its request time), a
// window-bound kind's, one with no answer recorded, and one whose answer is
// stamped before its request. The registration
// comes off with its unregister, and a nil match panics.
func TestPlaceOfIsTheAnswerForAMockPlacedAtItsAnswer(t *testing.T) {
	poll := placeMock("poll", 1050, 2050, true)
	if IsAnswerPlaced(poll) || !PlaceOf(poll).Equal(wmAt(1050)) {
		t.Fatal("a poll was placed at its answer with nothing registered")
	}
	unregister := RegisterAnswerPlaced(HTTP, func(m *Mock) bool { return m.Spec.Metadata["poll"] == "yes" })
	if !IsAnswerPlaced(poll) || !PlaceOf(poll).Equal(wmAt(2050)) {
		t.Fatalf("a registered poll is placed at %v; want its answer, %v", PlaceOf(poll), wmAt(2050))
	}
	session := placeMock("session", 1050, 2050, true)
	session.TestModeInfo.Lifetime = LifetimeSession
	unanswered := placeMock("unanswered", 1050, 0, true)
	unanswered.Spec.ResTimestampMock = time.Time{}
	carried := placeMock("carried", 1050, 2050, true)
	removeCarry := RegisterCarryOver(HTTP, func(m *Mock) bool { return m.Name == "carried" })
	backwards := placeMock("backwards", 1050, 1040, true) // answered before it was asked
	for _, m := range []*Mock{placeMock("call", 1050, 2050, false), session, unanswered, carried, backwards} {
		if IsAnswerPlaced(m) || !PlaceOf(m).Equal(m.Spec.ReqTimestampMock) {
			t.Errorf("%s is placed at %v; want its request, %v", m.Name, PlaceOf(m), m.Spec.ReqTimestampMock)
		}
	}
	removeCarry()
	// Released by a StagingRelease need, or staged with the startup by a
	// StagingWithStartup one: placed at its request.
	for _, use := range []StagingUse{StagingRelease, StagingWithStartup} {
		needed := placeMock("needed", 1050, 2050, true)
		remove := RegisterStagingNeed(HTTP, StagingNeed{Use: use, Match: func(m *Mock) bool { return m.Name == "needed" }})
		if IsAnswerPlaced(needed) {
			t.Errorf("a mock a staging need (use %d) declares is placed at its answer", use)
		}
		remove()
	}
	// A mock not derived yet is looked at through a copy: a config one is a
	// session mock, every test's.
	underived := placeMock("underived", 1050, 2050, true)
	underived.Spec.Metadata["type"] = "config"
	underived.TestModeInfo = TestModeInfo{}
	if IsAnswerPlaced(underived) {
		t.Error("a config mock not derived yet is placed at its answer")
	}
	unbound := RegisterAnswerPlaced(ConnectionFailure, func(*Mock) bool { return true })
	failure := placeMock("refused", 1050, 2050, false)
	failure.Kind = ConnectionFailure
	if IsAnswerPlaced(failure) {
		t.Error("a connection failure was placed at its answer")
	}
	unbound()
	unregister()
	if IsAnswerPlaced(poll) {
		t.Fatal("the poll is still placed at its answer after its registration came off")
	}
	if AnswerPlacedRegistered() {
		t.Fatal("a kind is still registered after every registration came off")
	}
	if !PlaceOf(nil).IsZero() || IsAnswerPlaced(nil) {
		t.Fatal("a nil mock has a place")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterAnswerPlaced took a nil match")
		}
	}()
	RegisterAnswerPlaced(HTTP, nil)
}

// The in-flight rule does not apply to a mock placed at its answer: its place
// says which test it is, wherever it was sent.
func TestInFlightEligibleLeavesOutAMockPlacedAtItsAnswer(t *testing.T) {
	answerPlacedPoll(t)
	if InFlightEligible(placeMock("poll", 1500, 2050, true)) {
		t.Fatal("the in-flight rule applies to a mock placed at its answer")
	}
	if !InFlightEligible(placeMock("call", 1500, 2050, false)) {
		t.Fatal("the in-flight rule no longer applies to a call placed at its request")
	}
}

// A poll placed at its answer is owned by the test whose window holds its
// answer: (b) one sent inside test-1 and answered in test-2 is test-2's, not
// test-1's; (e) one sent as the app boots and answered in test-1 is test-1's,
// not start-up traffic; one sent in the gap before test-2 and answered in
// test-3 is test-3's, where the in-flight rule gave it to test-2; one answered
// in a gap is no test's. A call placed at its request is placed as before.
func TestDeriveWindowMappingsGivesAPollToTheTestItsAnswerIsIn(t *testing.T) {
	answerPlacedPoll(t)
	windows := []TestWindow{wmWindow("test-1", 1000, 1100), wmWindow("test-2", 2000, 2100), wmWindow("test-3", 3000, 3100)}
	recorded := []*Mock{
		placeMock("boot-poll", 500, 1050, true), // (e)
		placeMock("poll-b", 1060, 2050, true),   // (b)
		placeMock("gap-poll-late", 1500, 3050, true),
		placeMock("poll-to-gap", 2050, 2500, true), // answered in the gap after test-2
		placeMock("call-b", 1070, 2060, false),     // a call, placed at its request
	}
	mocks := make([]WindowedMock, 0, len(recorded))
	for _, m := range recorded {
		mocks = append(mocks, NewWindowedMock(m))
	}
	got := mappedNames(DeriveWindowMappings(windows, mocks))
	want := map[string][]string{"test-1": {"boot-poll", "call-b"}, "test-2": {"poll-b"}, "test-3": {"gap-poll-late"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapped %v; want %v", got, want)
	}
	// The entry keeps the mock's recorded times.
	for _, tc := range DeriveWindowMappings(windows, mocks).Tests {
		for _, e := range tc.Mocks {
			if e.Name == "poll-b" && (e.ReqTimestampMock != FormatMockTimestamp(wmAt(1060)) || e.ResTimestampMock != FormatMockTimestamp(wmAt(2050))) {
				t.Fatalf("poll-b's entry reads %s .. %s; want its recorded request and answer", e.ReqTimestampMock, e.ResTimestampMock)
			}
		}
	}
}
