package integrations

import (
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
)

// A READ BEFORE A WRITE THE REPLAY HAS NOT MADE (under exact serving: the
// proven switch).
//
// An application that keeps a copy of what it read for a span of time (a
// cache with a time to live) reads its dependency again only when the copy
// has expired. A replay that runs slower than its recording finds the copy
// expired where the recording found it fresh, and makes a read the
// recording did not make at that point. When the test recorded that read
// only after a write (the test blocks a client, then reads the block list),
// the first recording of the read the test holds is the answer from after
// the write. Answering the earlier read with it hands the application the
// state the write made before the replay made the write: the application
// acts on it (refuses the client the test has not blocked yet), and the
// test fails, or passes on a state it never reached.
//
// The rule: a read the matcher would answer with one of the open test's own
// recordings, that the test recorded after one of its own writes of the
// same kind the replay has not made yet, is answered with the newest
// recording of the same request made before that write: from earlier in
// the test, else from earlier in the recording (an earlier test, the
// start-up window). Never with a recording made after a write the replay
// has not made. With none, it is a miss that says so (BeforeUnmadeWrite.Note).
// Serving the earlier recording consumes nothing: the read the test recorded
// after the write still answers the call made after it.
//
// The parser says which of its calls are writes (isWrite) and which
// recordings are the same request as the call (same), as it compares them
// when it serves exactly. The store says which of the open test's writes
// the replay has not made (UnmadeWriteReader) and holds the recordings made
// before them (ExactConsumedReader, RecordedBeforeReader).

// UnmadeWriteReader is an optional MockMemDb capability under exact serving:
// whether a recording of the open test's own is recorded after a write of
// the test the replay has not made yet. The agent's MockManager implements
// it, and every MockMemDb wrapper that passes reads through to it must
// forward it: one that drops it answers a read before a write with the
// recording made after it.
type UnmadeWriteReader interface {
	// UnmadeWriteBefore is, when mk is one of the open test's own
	// recordings that has not answered, the first of the test's own
	// recordings of mk's kind recorded before it that has not answered
	// either and that isWrite says is a write; false when there is none, mk
	// is not one of the open test's own unanswered recordings, or no test is
	// open. isWrite is called without the store's locks held.
	UnmadeWriteBefore(mk *models.Mock, isWrite func(*models.Mock) bool) (*models.Mock, bool)
}

// BeforeUnmadeWrite is what ReadBeforeUnmadeWrite decided for a read.
type BeforeUnmadeWrite struct {
	// Write is the open test's write, recorded before the recording the
	// matcher chose, that the replay has not made: nil when the rule does
	// not apply, and the parser serves its choice as before.
	Write *models.Mock
	// Answer is, with Write set, the newest recording of the same request
	// made before Write, a copy, to answer the read with without consuming
	// anything: nil for none, a miss (Note).
	Answer *models.Mock
}

// Applies reports whether the read is one before a write the replay has not
// made: the parser answers it with Answer, or misses with Note.
func (b BeforeUnmadeWrite) Applies() bool { return b.Write != nil }

// Note is the reason of the miss when no recording of the request was made
// before the write: "" when the rule does not apply or Answer answers.
func (b BeforeUnmadeWrite) Note() string {
	if b.Write == nil || b.Answer != nil {
		return ""
	}
	return fmt.Sprintf("an extra read before %s (a write the test recorded before this read's recording, which the replay has not made yet); "+
		"the recording has no answer from before it. Under exact serving (KEPLOY_PROVEN) a read is never answered with a recording made after a write the replay has not made",
		b.Write.Name)
}

// errNoEarlierRecordings is ReadBeforeUnmadeWrite's answer over a store that
// says a write is unmade but cannot read the recordings made before it.
var errNoEarlierRecordings = errors.New("the mock store cannot read the recordings made before a write")

// ReadBeforeUnmadeWrite decides, under exact serving, whether chosen, the
// recording the matcher would answer a read with, was recorded after a write
// of the open test the replay has not made (UnmadeWriteReader), and if so
// which recording answers the read instead: the newest one made before
// that write that same accepts, of chosen's kind, of the test's own that
// answered (ExactConsumedReader), else of the whole recording
// (RecordedBeforeReader). isWrite says which of the kind's recordings are
// writes; a write chosen is no read and keeps its choice. With exact serving
// off, for a store that cannot tell, and for a recording no unmade write
// precedes, the zero value: the parser serves chosen as before.
func ReadBeforeUnmadeWrite(db MockMemDb, chosen *models.Mock, isWrite, same func(*models.Mock) bool) (BeforeUnmadeWrite, error) {
	if chosen == nil || isWrite == nil || same == nil || !ExactServing(db) || isWrite(chosen) {
		return BeforeUnmadeWrite{}, nil
	}
	ur, ok := db.(UnmadeWriteReader)
	if !ok {
		return BeforeUnmadeWrite{}, nil
	}
	w, ok := ur.UnmadeWriteBefore(chosen, isWrite)
	if !ok || w == nil {
		return BeforeUnmadeWrite{}, nil
	}
	out := BeforeUnmadeWrite{Write: w}
	before := w.Spec.ReqTimestampMock
	// earlier keeps the newest recording made before the write that is the
	// read's request; the first of equal request times, as recorded order
	// files them.
	var answer *models.Mock
	earlier := func(mk *models.Mock) bool {
		if mk == nil || mk.Kind != chosen.Kind || !mk.Spec.ReqTimestampMock.Before(before) || !same(mk) {
			return true
		}
		if answer == nil || mk.Spec.ReqTimestampMock.After(answer.Spec.ReqTimestampMock) {
			answer = mk
		}
		return true
	}
	if er, ok := db.(ExactConsumedReader); ok {
		for _, mk := range er.ConsumedTestMocks() {
			earlier(mk)
		}
	}
	if answer == nil {
		rb, ok := db.(RecordedBeforeReader)
		if !ok {
			return out, errNoEarlierRecordings
		}
		if err := rb.RangeRecordedBefore(before, earlier); err != nil {
			return out, err
		}
	}
	if answer != nil {
		out.Answer = answer.DeepCopy()
	}
	return out, nil
}
