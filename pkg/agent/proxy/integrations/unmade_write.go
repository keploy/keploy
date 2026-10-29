package integrations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

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
// The rule: a read an answer step would answer with a recording made after
// one of the open test's own writes the replay has not made yet, of a write
// that can change what the read reads, is answered with the newest recording
// of the same request made before that write and after the newest write the
// replay has made that can change it too: from earlier in the test, else
// from earlier in the recording (an earlier test, the start-up window, the
// background). Never with a recording made after a write the replay has not
// made, whoever recorded it: the open test (after the write, in recorded
// order), another test, or the background (a request time after the
// write's). Never with one made before a write the replay has made either
// (the open test's, an earlier test's, the background's): that is a state
// the replay has moved past, which the database could not answer. With none
// in between, it is a miss that says so (BeforeUnmadeWrite.Note), naming
// the unmade write, and the made one when there is one.
// Serving the earlier recording consumes nothing: the read the test recorded
// after the write still answers the call made after it. The read is still
// that recording's call, made at another point than the recording made it:
// when the test ends with no call made after the write having taken the
// recording, the read counts as its call (OwnRecordingStandIn), so one read
// recorded is one read replayed and the test's mock accounting holds.
//
// The parser says which of its recordings are writes that can change what
// the read reads (gates: Mongo's of the same collection, of its database as
// a whole, or of a collection an aggregate's stages read; a write to
// another collection never holds the read) and which recordings are the
// same request as the call (same), as it compares them when it serves
// exactly. A write it cannot tell the reach of holds every read. The store says which of the open test's writes
// the replay has not made (UnmadeWriteReader) and holds the recordings made
// before them (ExactConsumedReader, RecordedBeforeReader), and which
// recordings the replay has made the call of (UnmadeWriteReader.Made).

// UnmadeWriteReader is an optional MockMemDb capability under exact serving:
// whether a recording of the open test's own is recorded after a write of
// the test the replay has not made yet. The agent's MockManager implements
// it, and every MockMemDb wrapper that passes reads through to it must
// forward it: one that drops it answers a read before a write with the
// recording made after it.
type UnmadeWriteReader interface {
	// UnmadeWriteBefore is the first of the open test's own recordings of
	// mk's kind recorded before mk that has not answered and that gates
	// says is a write that can change what mk reads: before in recorded order when mk is one of the test's
	// own recordings that has not answered, at an earlier request time when
	// mk is any other recording (another test's, the background's, the
	// start-up window's). False when there is none, mk is one of the test's
	// own that answered already, or no test is open. gates is called
	// without the store's locks held.
	UnmadeWriteBefore(mk *models.Mock, gates func(*models.Mock) bool) (*models.Mock, bool)
	// Made reports whether the replay has made mk's call: for one of the
	// open test's own recordings, that it answered a call in the test's
	// view in force (this attempt of the test); for any other recording
	// (another test's, the background's, the start-up window's, one the
	// test shares), that a call of the staged set consumed it. A write
	// made is one whose state a later read is answered from or after,
	// never from before.
	Made(mk *models.Mock) bool
}

// OwnRecordingStandIn is an optional MockMemDb capability under exact
// serving: the read a step answers from before an unmade write is the call
// of the recording the step chose for it, when that recording is one of the
// open test's own. The agent's MockManager implements it, and every
// MockMemDb wrapper that passes reads through to it must forward it: one
// that drops it leaves the recording unconsumed when no call made after the
// write takes it, and the test fails the mock accounting for a call it made.
type OwnRecordingStandIn interface {
	// StandIn files mk, the recording a read answered from before an
	// unmade write was chosen for, as one the read stood in for, when it is
	// one of the open test's own that has not answered. It answers nothing
	// and consumes nothing now: a call made later still takes it, once.
	// When the test ends with none having taken it, it counts as answered
	// (the read was its call), flagged as used with nothing learned onto
	// it. Any other recording (another test's, the background's, one the
	// test shares) is no call the test owes, and nothing is filed.
	StandIn(mk *models.Mock)
}

// ViewEnd is an optional MockMemDb capability under exact serving: what a
// parser keeps for the view in force alone (the open test's, or the pool's
// between tests), such as a cursor an answer from before an unmade write
// opened, is forgotten when that view ends, so nothing of it reaches a later
// test, round or run the same agent serves. The agent's MockManager
// implements it, and every MockMemDb wrapper that passes reads through to it
// must forward it.
type ViewEnd interface {
	// AtViewEnd runs f once, when the view in force now ends: the next
	// view is installed (the next test, the pool between tests, a new
	// set), or the manager closes. False when no view is in force or it
	// has ended already: f never runs, and the caller keeps nothing for
	// the view. f is called without the store's locks held.
	AtViewEnd(f func()) bool
}

// BeforeUnmadeWrite is what ReadBeforeUnmadeWrite decided for a read.
type BeforeUnmadeWrite struct {
	// Write is the open test's write, recorded before the recording the
	// matcher chose, that the replay has not made and that can change what
	// the read reads: nil when the rule does not apply, and the parser
	// serves its choice as before.
	Write *models.Mock
	// WriteSays is the parser's account of Write, for the miss's note and
	// the report: what it is and what it changes (Mongo: "an insert into
	// db.coll"); "" names Write by its recording alone.
	WriteSays string
	// Made is, with Write set, the newest write by request time the replay
	// has made (UnmadeWriteReader.Made) that can change what the read reads:
	// the answer is made after it. nil when the replay has made none.
	Made *models.Mock
	// MadeSays is the parser's account of Made, as WriteSays is of Write.
	MadeSays string
	// Answer is, with Write set, the newest recording of the same request
	// made before Write and after Made, a copy, to answer the read with
	// without consuming anything: nil for none, a miss (Note).
	Answer *models.Mock
}

// Applies reports whether the read is one before a write the replay has not
// made: the parser answers it with Answer, or misses with Note.
func (b BeforeUnmadeWrite) Applies() bool { return b.Write != nil }

// Note is the reason of the miss when no recording of the request was made
// before the write (and after the made one, Made): "" when the rule does
// not apply or Answer answers.
func (b BeforeUnmadeWrite) Note() string {
	if b.Write == nil || b.Answer != nil {
		return ""
	}
	if b.Made != nil {
		return fmt.Sprintf("an extra read before %s, after %s: no read was recorded between %s and %s. "+
			"The first is a write the replay has made, the second one the test recorded before this read's recording that the replay has not made yet; both can change what this read reads. "+
			"Under exact serving (KEPLOY_PROVEN) a read is never answered with a recording made after a write the replay has not made, nor with one made before a write it has made",
			b.writeName(), b.madeName(), b.madeName(), b.writeName())
	}
	return fmt.Sprintf("an extra read before %s (a write the test recorded before this read's recording, which the replay has not made yet, and which can change what this read reads); "+
		"the recording has no answer from before it. Under exact serving (KEPLOY_PROVEN) a read is never answered with a recording made after a write the replay has not made",
		b.writeName())
}

// Answered is the sentence the run report names a read answered from
// before the write by (ReportAnswered): "" when the rule does not apply, or
// no recording answers (Note).
func (b BeforeUnmadeWrite) Answered() string {
	if b.Write == nil || b.Answer == nil {
		return ""
	}
	return fmt.Sprintf("an extra read before %s, answered from %s: the newest recording of this request made before that write, which the replay had not made yet. "+
		"The recording made after the write still answers the read made after it; when the test makes no such read, this read counts as that recording's call. Nothing is learned from this answer",
		b.writeName(), b.Answer.Name)
}

// writeName is Write's name, with the parser's account of it (WriteSays)
// when it gives one: "mock-7 (an insert into db.coll)".
func (b BeforeUnmadeWrite) writeName() string {
	if b.WriteSays == "" {
		return b.Write.Name
	}
	return b.Write.Name + " (" + b.WriteSays + ")"
}

// madeName is Made's name, with the parser's account of it (MadeSays).
func (b BeforeUnmadeWrite) madeName() string {
	if b.MadeSays == "" {
		return b.Made.Name
	}
	return b.Made.Name + " (" + b.MadeSays + ")"
}

// ReportAnswered reports a read b answers from before the write
// (models.AnswerBeforeUnmadeWrite) through the reporter misses take
// (models.ReportAnswer), so the run report names it, the write and the
// recording that answered (models.BeforeWriteLines). r is the parser's
// account of the call (its protocol, summary and destination, as
// mismatch.NewReport builds them), on which Answer, ClosestMock (the
// recording that answered) and NextSteps (Answered) are set. It reports
// nothing, and returns false, when b answers nothing (the rule does not
// apply, or the read misses: Note) or no reporter is installed.
func ReportAnswered(ctx context.Context, b BeforeUnmadeWrite, r *models.MockMismatchReport) bool {
	if b.Write == nil || b.Answer == nil || r == nil {
		return false
	}
	r.Answer, r.ClosestMock, r.NextSteps = models.AnswerBeforeUnmadeWrite, b.Answer.Name, b.Answered()
	return models.ReportAnswer(ctx, r)
}

// errNoEarlierRecordings is ReadBeforeUnmadeWrite's answer over a store that
// says a write is unmade but cannot read the recordings made before it.
var errNoEarlierRecordings = errors.New("the mock store cannot read the recordings made before a write")

// everRecorded is a request time after every recording's: RangeRecordedBefore
// up to it visits the whole staged set. It is the latest instant whose Unix
// time in nanoseconds an int64 holds (in 2262), as a store that keeps request
// times in nanoseconds compares them: a later one has no such value.
var everRecorded = time.Unix(0, math.MaxInt64)

// ReadBeforeUnmadeWrite decides, under exact serving, whether chosen, the
// recording an answer step would answer a read with, was recorded after a write
// of the open test the replay has not made (UnmadeWriteReader) that can
// change what chosen reads (gates), and if so which recording answers the
// read instead: the newest one that same accepts, of chosen's kind, made
// before that write and after the newest write the replay has made that
// gates says can change what chosen reads (UnmadeWriteReader.Made, at any
// request time, of the whole staged set: Made), of the test's own that
// answered (ExactConsumedReader), else of the whole recording
// (RecordedBeforeReader). gates is the parser's, built for chosen: whether a
// recording of the kind is a write that can change what chosen reads; nil
// when chosen reads nothing a write changes (a write is no read), and chosen
// keeps its choice. With exact serving off, for a store that cannot tell,
// and for a recording no unmade write it gates on precedes, the zero value:
// the parser serves chosen as before. A store that tells of an unmade write
// but cannot read the staged set, or fails to, fails the read: it cannot
// tell which writes the replay has made.
//
// A parser asks it of the recording a step will answer the read with, and
// answers the read with Answer when it is set: chosen is then filed with the
// store as the recording the read stood in for (OwnRecordingStandIn).
func ReadBeforeUnmadeWrite(db MockMemDb, chosen *models.Mock, gates, same func(*models.Mock) bool) (BeforeUnmadeWrite, error) {
	if chosen == nil || gates == nil || same == nil || !ExactServing(db) {
		return BeforeUnmadeWrite{}, nil
	}
	ur, ok := db.(UnmadeWriteReader)
	if !ok {
		return BeforeUnmadeWrite{}, nil
	}
	w, ok := ur.UnmadeWriteBefore(chosen, gates)
	if !ok || w == nil {
		return BeforeUnmadeWrite{}, nil
	}
	out := BeforeUnmadeWrite{Write: w}
	before := w.Spec.ReqTimestampMock
	rb, ok := db.(RecordedBeforeReader)
	if !ok {
		return out, errNoEarlierRecordings
	}
	// newestOf keeps, in *kept, the newest recording made before the write
	// that is the read's request; of equal request times, the first it is
	// given: the first recorded for the set's recordings, the first to have
	// answered for the test's own.
	newestOf := func(kept **models.Mock, mk *models.Mock) {
		if mk == nil || mk.Kind != chosen.Kind || !mk.Spec.ReqTimestampMock.Before(before) || !same(mk) {
			return
		}
		if *kept == nil || mk.Spec.ReqTimestampMock.After((*kept).Spec.ReqTimestampMock) {
			*kept = mk
		}
	}
	// The whole set: the newest write the replay has made that can change
	// what chosen reads (Made is asked first: it is the store's map, gates
	// the parser's decode), and the newest recording of the request before
	// the write.
	var recorded *models.Mock
	err := rb.RangeRecordedBefore(everRecorded, func(mk *models.Mock) bool {
		if mk == nil || mk.Kind != chosen.Kind {
			return true
		}
		if ur.Made(mk) && gates(mk) && (out.Made == nil || mk.Spec.ReqTimestampMock.After(out.Made.Spec.ReqTimestampMock)) {
			out.Made = mk
		}
		newestOf(&recorded, mk)
		return true
	})
	if err != nil {
		return out, err
	}
	// after says whether mk was made after the newest write the replay has
	// made: a state that write is in.
	after := func(mk *models.Mock) bool {
		return mk != nil && (out.Made == nil || mk.Spec.ReqTimestampMock.After(out.Made.Spec.ReqTimestampMock))
	}
	var own *models.Mock
	if er, ok := db.(ExactConsumedReader); ok {
		for _, mk := range er.ConsumedTestMocks() {
			newestOf(&own, mk)
		}
	}
	answer := own
	if !after(answer) {
		answer = recorded
	}
	if after(answer) {
		out.Answer = answer.DeepCopy()
		if si, ok := db.(OwnRecordingStandIn); ok {
			si.StandIn(chosen)
		}
	}
	return out, nil
}
