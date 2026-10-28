package learn

import (
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

// Side is the half of an exchange a value is in.
type Side string

// The two sides.
const (
	Request  Side = "request"
	Response Side = "response"
)

// Seen is one value of an exchange F served earlier in the test: the leaf
// the recording holds there, the value F saw there (the live request's, or
// the answer keploy served, with its variables' values), and the leaf's
// variable place ("" when no variable place names it: VarPlace).
type Seen struct {
	Side  Side
	Leaf  Leaf
	Live  string
	Place string
	// Written: the request stores the value at Place (a body it creates or
	// updates a record with), rather than only naming or comparing with it
	// (ReadBack).
	Written bool
	// Record names the record a Written place stores its value in, as the
	// adapter names it (its collection, and the identity the record has in
	// this run); "" when the adapter names none. A read of that record links
	// its answer to this write (ReadBack): the adapter states the link, keploy
	// never guesses it from the value.
	Record string
	// Name is the variable the learn step put at Place (bound or used
	// there), "" for none (Named).
	Name string
	// Stamp: the leaf is its answer's own clock stamp by its protocol (an
	// HTTP answer's Date header within the span keploy stamped the exchange
	// in: AnswerStamp, instants.go rule 3), which no value came from. Its
	// words are searched for nothing; the echo reads it as it stands.
	Stamp bool
	// Undeclared: the leaf is a test case's request body, or a leaf of it,
	// one side of which declares no media type (no Content-Type) and is
	// read as text by its bytes (plainText, GivenRequest); both sides are
	// then searched so, the declared one too. Bytes that frame its
	// words (a protobuf's tag and length) may be printable and glued to a
	// word ("BAs17"), and keploy cannot tell them from the words: a word of
	// what changed found in it glued to the bytes around it is refused
	// (unaccounted), as one found there as a word is, never learned as
	// noise. A body that declares a text media type is read by its words.
	Undeclared bool
}

// Earlier is an HTTP mock the learn step served earlier in the same test,
// leaf by leaf: a variable found there has a place a runtime can bind it at.
type Earlier struct {
	Mock string
	// At is when the mock's request was recorded. One recorded after the
	// call gave the recorded application nothing before the call: it is read
	// as a recording made after the call is, which only what F served there
	// counts for, never by its leaves (leafedAfterCall); one recorded at the
	// call's own instant is read both ways (leafedAtCall, classifyGiven).
	At time.Time
	// Own: the mock is one of the test's own, not the pool's. Only a test's
	// own mocks carry its variables.
	Own bool
	// Run: served while no test was open (Tracker.Served): a variable bound
	// at it is the run's, which every later test uses (Tracker.RunName).
	Run bool
	// StartUp: served before the set's first test began (Tracker.Served),
	// so the call it is an earlier place of was made then too: no test was
	// running, and a reason names the place as earlier in the recording,
	// never earlier in the test. Only the wording reads it.
	StartUp bool
	// Given: the place is no mock but the running keploy test case's own
	// request (GivenRequest, Tracker.Give), each part as recorded (Leaf) and
	// as keploy test sent it (Live): what the application was given before
	// any call of the test (given.go). It is read as any earlier place is: a
	// value that drifted with a part sent with a template value, a word of
	// what changed it holds, and its instants the echo reads (a pick by the
	// application's clock, the next of the slots it lists). Nothing binds a
	// variable there, and nothing is read back from it.
	Given bool
	// Unread, of a Given place, says why the test case's request could not
	// be read (none was sent with the test case, or keploy cannot read its
	// kind): it has no Seen, and every value that changed is refused
	// (ClassifyIn), never learned as noise or an echo.
	Unread string
	Seen   []Seen
	// noTime: the exchange's recorded time keploy cannot read (At was the
	// zero time), read as one made at the call's own instant
	// (timedEarlier), and named so.
	noTime bool
	// Payload is the exchange's recording as a payload is read (RawOf), its
	// answer read back, where the adapter that served it leaf by leaf gives
	// it (the zero Raw where it does not): the answer's fields name the same
	// places as another recording's of its kind, which the offset rule reads
	// (offsetFrom). Its answer's recorded time and its connection position
	// (Raw.SentOn) are the exchange's, which the order the recording gave the
	// application its earlier answers in reads (recordedOrder). Nothing else
	// reads it: the leaves are the exchange's.
	Payload Raw
}

// Raw is any other mock of the test's recording, of any kind: served earlier
// in the test (Served), or recorded before the request L classifies (At), or
// at its own instant (earlierThan).
// Its text (Text) is its recorded request and answer, the payload only, as
// one text: a value found there flowed from that mock, at a place no leaf
// names.
//
// A Raw is read from its mock only (RawOf, ReadRaw, and their DriftCache
// forms), or made from another Raw (WithServed): its fields are its own, so
// its text and its answer's clock reads (AnswerClock) are read together from
// the mock and travel together, and no copy keeps the one and drops the
// other. A Raw that was not read from its mock (the zero Raw, or ReadRaw's
// of an answer it could not read back) is refused wherever it is handed to
// the classifier (ClassifyIn): what it held cannot be told, and nothing is
// learned on the strength of its absence.
type Raw struct {
	// mock is Mock, at At and served Served.
	mock   string
	at     time.Time
	served bool
	// answered is when the mock's answer was recorded (RawOf: its
	// ResTimestampMock), the zero time where keploy cannot read it; conn and
	// pos are where the recording carried its request (SentOn): "" and 0
	// where its adapter does not say. recordedOrder tells by them the order
	// the recording gave the application its earlier answers in.
	answered time.Time
	conn     string
	pos      uint64
	// first and last are the first and the last of F's serves of the mock,
	// in one order across the set's start-up, its tests and the gaps
	// between them (ServedIn: 1 the first serve; learn.Order.Of), 0 where
	// it was not served or that order is not known: the offset rule
	// (offsetFrom) tells by them whether F gave the application two
	// recordings both runs gave it in the order the recording did.
	first, last int
	// call is the call the mock answers, as one recording of it stands in
	// for another (callOf): only an answer to the same call is given in
	// another's place (the offset rule, offsetFrom).
	call string
	// elsewhere: a recording F gave the application before the call that
	// is no earlier place of the call's own test (OfAnotherTest): another
	// recorded test's, or one F served in an earlier test than the
	// previous one. Only the offset rule (offsetPlacesOf), the word rule
	// (givenWhy) and the echo of what F gave there (echoedElsewhere) read
	// it, never a variable's search or the call's own echo, so that a call
	// of one test binds no value of another's, while a value built on what
	// F gave there by an offset, or copied from its answer word for word or
	// as a moment, is refused, naming it.
	elsewhere bool
	// late: an earlier place of the call's own test recorded at the call's
	// own instant, read here as one made at or after the call
	// (givenAtCall): a reason naming it says it is read both ways
	// (givenWhose), never that it is no place of the call's own test.
	late bool
	// noTime: the recording's recorded time keploy cannot read, read as one
	// made at the call's own instant (timedEarlier): a reason naming it says
	// so (atNote, givenWhose).
	noTime bool
	// read: the text was read from the mock (RawOf).
	read bool
	// startUp: the mock is a connection's recorded start-up (isStartUp).
	// A word it carries in a field a swap of start-ups can change
	// (swapHolds: its backend pid, its secret key, a field another
	// start-up recorded otherwise) never varies: a drifted value one of
	// whose recorded words stands there is refused, never learned as
	// varying or bound there (appearedIn, startUpWhy).
	startUp bool
	// asRecorded: F served r unchanged (ServedAsRecorded), so what F sent
	// and gave the application there is r's recorded text: what the rule a
	// copy follows its source reads of what F gave at r (follows.go).
	asRecorded bool
	// live is, of a recording F served changed, what F's application sent
	// there and F answered it, read as RawOf reads a mock (ServedLive): the
	// value F gave the application at each field of the payload, by the
	// field's path. nil where F gave no such view (not served, served
	// unchanged, or an adapter that does not say), and keploy then reads that
	// F gave nothing there (fail closed).
	live *Raw
	// unread, of a Raw that was not read, says why where the place it stands
	// for says more than rawUnread can (Earlier.lateOf: payloadUnread).
	unread string
	// text is Text; answerClock is AnswerClock.
	text        string
	answerClock []time.Time
	// fields are where each scalar and key of text stands in it, and the
	// field of the mock's spec it is (rawField): what a reason names where a
	// value is found.
	fields []rawField
	// stamps are the fields of text that are its answer's own clock stamp
	// by its protocol (instants.go, rule 3: an HTTP answer's Date header
	// within the span keploy stamped the exchange in): no value came from
	// one, and only the echo reads them.
	stamps []rawField
	// moves are, of what F served at the leaves of an exchange's answer
	// (servedAnswer), the leaves F served with another value than recorded
	// (a template value), each where the exchange's payload holds it
	// (servedMove): the offset rule reads them as places F gave the
	// application, keyed as the payload's own are (movePlaces), never as
	// places the recording gave it.
	moves []servedMove
}

// Mock is the name of r's mock.
func (r Raw) Mock() string { return r.mock }

// recordedAt is when r's request was recorded, the zero time where keploy
// cannot read it (noTime: at then stands for the call's own instant).
func (r Raw) recordedAt() time.Time {
	if r.noTime {
		return time.Time{}
	}
	return r.at
}

// recordedAt is when e's request was recorded, the zero time where keploy
// cannot read it (noTime: At then stands for the call's own instant).
func (e Earlier) recordedAt() time.Time {
	if e.noTime {
		return time.Time{}
	}
	return e.At
}

// atNote is what a reason naming r, an earlier place of a call recorded at
// at, says of when r was recorded: that keploy cannot read it (noTimeNote),
// or that it was the call's own instant (atCallNote); "" otherwise.
func (r Raw) atNote(at time.Time) string {
	switch {
	case r.noTime:
		return noTimeNote
	case r.at.Equal(at):
		return atCallNote
	}
	return ""
}

// earlierThan reports whether r is an earlier place of a call recorded at
// at: served earlier in the test, or recorded before the call or at its own
// instant. A recording made at that instant (a fixture's whole second) may
// have been made before the call, so it is read as one, the fail-closed
// side: a value it holds is refused or bound, never learned as noise by
// default, whether F served it before the call or not. The caller hands the
// paired recording as no Raw, and a recording made at the call's own instant
// as one, served or not, as it hands one made a moment before the call: one
// F served there is no place F gave the application alone (GivenEarlier),
// whose word rule reads only what F served.
func (r Raw) earlierThan(at time.Time) bool {
	return r.served || !r.at.After(at)
}

// At is when r's mock's request was recorded.
func (r Raw) At() time.Time { return r.at }

// Served reports whether F gave the application r's mock before the call:
// served earlier in the test, or before it in the running set (ServedAs).
func (r Raw) Served() bool { return r.served }

// Read reports whether r was read from its mock (RawOf): a Raw that was not
// holds no text, and the classifier refuses it.
func (r Raw) Read() bool { return r.read }

// rawField is one scalar or key of a Raw's text: [start, end) of it in the
// text, and its field of the mock's spec, by the spec's JSON names
// ("Res.header.Date", "MongoResponses[0].Message.Sections[1]"); a key's is
// its own (key: the field's name, not its value).
type rawField struct {
	start, end int
	path       string
	key        bool
	// conn: of a connection's recorded start-up, a field that is the
	// connection's own, whatever the server: its backend pid or secret key
	// (BackendKeyData; RawOf, connFields).
	conn bool
}

// Text is r's recorded request and answer, the payload only, as one text.
func (r Raw) Text() string { return r.text }

// AnswerClock are, of a protocol-phase mock (a driver's handshake, heartbeat
// or auth), the moments of its answer's own clock reads (RawOf, answerClock):
// the instants its answer holds, between its request and its answer as
// keploy stamped them, that its request does not, which the dependency read
// from its clock while it answered. No value came from one of them
// (instants.go, rule 2). nil for a data-plane mock, or one whose answer or
// span is not known: then every instant of it is a place a value may have
// come from. A copy; r keeps its own.
func (r Raw) AnswerClock() []time.Time { return append([]time.Time(nil), r.answerClock...) }

// WithServed is r as served, or not, earlier in the test: the same text,
// fields and answer clock reads.
func (r Raw) WithServed(served bool) Raw {
	r.served = served
	if !served {
		r.first, r.last, r.asRecorded, r.live = 0, 0, false, nil
	}
	return r
}

// ServedAsRecorded is r, which F served (ServedIn, WithServed), as served
// unchanged: the application sent its request byte for byte as recorded (no
// part of it learned as noise or bound as a variable in F) and F gave it the
// recorded answer byte for byte (no variable, template value or clock stamp
// of F's own in it). An adapter says it only where it knows both. Under the
// rule a copy follows its source (ClassifyFollowing) only such a recording
// shows what F sent and gave the application there: a word of it F gave
// (follows.excusedRaw), and a source the call's value moved away from while
// it did not move (appearedIn). Of a Raw F did not serve it says nothing,
// and keploy then reads that F gave nothing there (fail closed).
func (r Raw) ServedAsRecorded() Raw {
	r.asRecorded = r.served
	return r
}

// AsRecorded reports whether F served r unchanged (ServedAsRecorded).
func (r Raw) AsRecorded() bool { return r.served && r.asRecorded }

// ServedLive is r, which F served (ServedIn, WithServed) changed, with F's
// view of it: live is RawOf of the mock as F's application sent its request
// and F answered it. Its fields stand for r's by their paths (rawField.path):
// what F gave the application at a field of r's payload is live's text at
// the field of the same path. Under the rule a copy follows its source
// (ClassifyFollowing) a word of the value at a field F gave another value
// at, which the value took none of the words of, is no word the value copied
// there (follows.excusedRaw, appearedIn's refute: movedAway); where they
// drifted together, or the view lacks the field, it stays where the value
// came from. A word the value holds now that stands in the view and not in
// the recorded text is refused, under ClassifyGiven too (unaccounted): the
// recording did not give it there. A live that was not read from its mock,
// or of another mock or call than r's, or a Raw F did not serve, gives no
// view: keploy then reads that F gave nothing there (fail closed).
// ServedAsRecorded, where an adapter also says it, reads first.
func (r Raw) ServedLive(live Raw) Raw {
	if !r.served || !live.read || live.mock != r.mock || live.call != r.call {
		r.live = nil
		return r
	}
	live.live = nil
	r.live = &live
	return r
}

// Live reports whether r carries F's view of what F served there
// (ServedLive; WithServed(false) drops it).
func (r Raw) Live() bool { return r.live != nil }

// ServedAs is r as served once, the n-th serve of F's in the running set
// (ServedIn(n, n)).
func (r Raw) ServedAs(n int) Raw { return r.ServedIn(n, n) }

// ServedIn is r as served by F before the call, in the test or before it,
// first at its first-th serve in the running set and last at its last-th, in
// the one order F served the set's mocks in across its start-up, its tests
// and the gaps between them (learn.Order.Of; 1 the first serve), with the
// same text, fields and answer clock reads. A Raw served in an order not
// known (WithServed, RawOf, ServedIn(0, 0), or last before first) stands in
// any other's place the offset rule reads.
func (r Raw) ServedIn(first, last int) Raw {
	r.served, r.first, r.last = true, max(first, 0), max(last, 0)
	if r.first == 0 || r.last < r.first {
		r.first, r.last = 0, 0
	}
	return r
}

// fromMock reports whether r was read from a mock (RawOf, ReadRaw), whether
// its text could be read or not: the zero Raw, an exchange's payload its
// adapter gave none of, was not.
func (r Raw) fromMock() bool {
	return r.read || r.mock != "" || r.served || r.call != "" || !r.at.IsZero()
}

// SentOn is r as its recording carried its request: on connection conn, the
// pos-th request its client sent there (1 the first; 0 unknown), with the
// same text, fields and answer clock reads. An adapter says it only where
// its protocol answers a connection's requests in the order they were sent
// (a Postgres connection: the invocation id's sequence), so that the
// recording gave the application the answers to two requests in flight
// together on one connection in the order of their positions
// (recordedOrder): a client that sends a block of statements in one write
// gives them one recorded instant. "" or 0 says nothing, and keploy then
// cannot tell the order of two such requests (fail closed).
func (r Raw) SentOn(conn string, pos uint64) Raw {
	r.conn, r.pos = conn, pos
	return r
}

// Nth is where F first served r's mock (ServedIn), 0 where it was not served
// or that order is not known.
func (r Raw) Nth() int { return r.first }

// Span is where F first and last served r's mock (ServedIn), 0, 0 where it
// was not served or that order is not known.
func (r Raw) Span() (first, last int) { return r.first, r.last }

// OfAnotherTest is r as a recording F gave the application before the call
// that is no earlier place of the call's own test (another recorded test's,
// or one served in an earlier test than the previous one): a place the
// offset rule, the word rule and the echo of what F gave there read, and no
// other rule does (Raw.elsewhere).
func (r Raw) OfAnotherTest() Raw {
	r.elsewhere = true
	return r
}

// AnotherTests reports whether r is a place the offset rule and the word
// rule alone read (OfAnotherTest).
func (r Raw) AnotherTests() bool { return r.elsewhere }

// ownPlaces splits raw into the places every rule reads and those the offset
// rule and the word rule alone read (OfAnotherTest), each in raw's order.
func ownPlaces(raw []Raw) (own, elsewhere []Raw) {
	for _, x := range raw {
		if x.elsewhere {
			elsewhere = append(elsewhere, x)
			continue
		}
		own = append(own, x)
	}
	return own, elsewhere
}

// Kind is what a drifted leaf is learned as.
type Kind string

// The three kinds (design §7.2 step 6; an echo is the design's
// correlation). They never collapse into each other: noise is not compared,
// an echo is bound at the request and answered, a variable is asserted equal
// to the value at its earlier place.
const (
	KindNoise    Kind = "noise"
	KindEcho     Kind = "echo"
	KindVariable Kind = "variable"
	// KindClockOffset is an instant its protocol types that is a read of
	// the application's clock plus a constant offset (an expiry thirty
	// minutes after now): in its call's window in the recording and in
	// record's full round, at one offset (Learned.Offset). It is compared,
	// never noise: a replay checks the value it sees is a read in its own
	// call's window plus that offset (models.ClockOffset.Holds). The learn
	// step learns it only for a caller whose replay so compares it
	// (Window.ComparesOffsets); for any other it is unaccounted.
	KindClockOffset Kind = "clock-offset"
	// KindClockRead is a drifted leaf that is a plain read of the
	// application's clock whole (Learned.Read): compared, never noise. A
	// replay checks the value it sees is a read made in its own call's span
	// on the application's clock (models.ClockOffset.Holds at offset zero).
	// It is its own kind, never KindNoise with a read beside it, so that a
	// caller whose replay checks no clock read, and switches on the kind
	// alone, refuses it with its Why (clockReadUncheckedWhy, or the reason
	// keploy refused such a value before it learned it as a read) rather
	// than learn it as noise that lets any value pass at every replay. A
	// caller whose replay checks it writes it to models.MockSpec.Clocks
	// (Learned.Clock).
	KindClockRead Kind = "clock-read"
	// KindUnaccounted is a drift keploy can learn as none of the three:
	// no earlier place is its source, and yet its readings cannot show it
	// came from nowhere (unaccounted). The request is refused.
	KindUnaccounted Kind = "unaccounted"
)

// Place is where a value is: a mock, and, when a leaf names it, a side and
// the leaf. A place found in a mock's raw text has no side and no path.
type Place struct {
	Mock  string
	Side  Side
	Path  string
	Noise string
	// Var is the leaf's variable place, "" when it has none (Seen.Place).
	Var string
	// Live is the value F saw at the leaf.
	Live string
	// Whole: the leaf held the value whole (not as a word of a longer
	// value), so the value can be bound there.
	Whole bool
	// Own: the mock is one of the test's own (Earlier.Own).
	Own bool
	// Run: the mock was served while no test was open (Earlier.Run).
	Run bool
	// Given: the place is the test case's own request (Earlier.Given), no
	// call of the test: nothing binds a variable there, and a value found
	// there is refused.
	Given bool
	// Name is the variable already at Var (Seen.Name), "" for none.
	Name string
	// Field, of a place found in a raw mock's payload, is the field of its
	// spec that holds what was found, as the mock file names it
	// (rawField.describe; "" when it stands across two); Held is what was
	// found there: the value whole, or a part of it. Echo: what was found is
	// an instant of the value's, in another layout or at another resolution,
	// that the earlier places hold both ways (echoedInstants). They are the
	// evidence a reason names (evidence), which names Held by where it
	// stands in the value, never by its text.
	Field, Held string
	Echo        bool
	// AtCall, of a place found in a raw mock's payload: the mock was
	// recorded at the call's own instant (a fixture's whole second), which
	// keploy reads as one the recording may have given the application
	// before the call, the fail-closed side (Raw.earlierThan). A reason
	// naming the place says so (placeAt).
	AtCall bool
	// noTime: that mock's recorded time keploy cannot read, read as the
	// call's own instant (Raw.noTime): a reason says so (placeAt).
	noTime bool
	// startUp, of a place found in a raw mock's payload: the mock is a
	// connection's recorded start-up (Raw.startUp) and the value's word
	// stands there in a field a swap of start-ups can change (swapHolds),
	// whose words never vary, so a drifted value found there is refused
	// (startUpWhy).
	startUp bool
}

// atCallNote is what a reason says of a recording it names that was made at
// the call's own instant: keploy reads it as given to the application before
// the call, the fail-closed side, as every rule reads the equal instant.
//
// noTimeNote is what it says of one whose recorded time keploy cannot read
// (the zero time): read both ways, as one made at the call's own instant is.
const (
	atCallWords = "recorded at the call's own instant, so keploy reads it as given to the application before the call"
	atCallNote  = " (" + atCallWords + ")"
	noTimeWords = "recorded at a time keploy cannot read, so keploy reads it as given to the application before the call"
	noTimeNote  = " (" + noTimeWords + ")"
)

// Learned is what L learned about one drifted leaf of the paired mock.
type Learned struct {
	Kind     Kind
	Recorded Leaf
	Live     Leaf
	// From is a variable's earlier place, where it is bound.
	From Place
	// Echo are an echo's places in the paired mock's recorded answer.
	Echo []Leaf
	// Offset is, of a KindClockOffset, the offset its call's windows put
	// the value at from the application's clock in both runs; nil for any
	// other kind.
	Offset *models.ClockOffset
	// Read is, of a leaf that is a read of the application's clock whole
	// (KindClockRead), that read as a replay checks it: a zero
	// offset (models.ClockOffset.Plain) at the resolution the learn step
	// read it at, the coarsest of the two values' encodings and the
	// granularity they share. The leaf is an instant its protocol types that
	// names its moment or is a wall clock of no zone, or a text or a number
	// that is one such instant whole (readClockText), every word of it that
	// changed a read of the clock; of a wall clock of no zone, it carries the
	// zone the learn step read it in (models.ClockOffset.Zone, wallZone),
	// which a replay reads it in alone. A
	// caller whose replay checks it writes it to models.MockSpec.Clocks
	// (Clock), not to the request-body noise, so the value is still
	// compared: it must be a read of the clock made in its own call's window
	// (ClockReadHolds). A time of day whole (a type of its protocol, or a
	// text: timeOfDayOf), which names no day, is a read too, in the zone its
	// recording read it in (todRead), which a replay checks modulo a day in
	// its call's span. nil for any other leaf: a clock read that is only a
	// part of a longer value stays noise.
	Read *models.ClockOffset
	// Why, of an unaccounted drift, is why it cannot be noise; of noise
	// record's full round learned past a source it served unchanged that
	// the value moved away from (ClassifyFollowing), that source and why it
	// is none (follows.refute); of a variable
	// found in a raw mock's payload (From.Side "") or by the echo
	// (From.Echo), the evidence: what was found, where, that nothing showed
	// the value is a clock read, and how to correct it (evidence). It never
	// holds a value: it names a part of one by where it stands.
	Why string
}

// Classify decides each drifted leaf of the mock rec was paired with
// (design §7.2 step 6). at is when that mock's request was recorded;
// earlier are the HTTP mocks F served before it in the same test, leaf by
// leaf, and the test case's own request (Earlier.Given), read as they are;
// raw the other recordings that came before it: recorded before it, or
// served by F before it in the test, in the previous test, at the start-up
// or between tests (one marked OfAnotherTest is a place of the offset rule
// and the word rule alone); resp are the leaves of its recorded answer.
//
//   - Variable: the recorded value appeared earlier in the test's recording,
//     whatever its type there, in any kind of mock: whole, or a part of it
//     that drifted together with a part of the earlier value (sources). It
//     is asserted equal to the live value at that earlier place, wherever F
//     saw what: a value that came from somewhere is never stopped being
//     compared. The place a leaf names binds it: where the value was sent
//     whole, the test's own call before the run's (appeared); failing that,
//     the first raw mock holding it.
//
//   - Echo: the recorded answer echoes the recorded value (a scalar equal to
//     it, or a string holding it as a token).
//
//   - Noise otherwise: not compared, its type still enforced.
//
//   - Unaccounted: none of these, and yet the declared readings cannot show
//     it came from nowhere (unaccounted): refused, never noise. So is one a
//     word of which F gave the application in another test (givenWhy): a
//     value copied from another test's answer word for word is followed
//     nowhere, and refused naming it; and so is one whose changed instant
//     the echo finds in what F gave the application there, or in a
//     recording made after the call (echoedElsewhere), which moved as a read
//     of the clock does.
//
// A leaf its protocol types as one instant (Instant) is read whole, never
// word by word, and is noise only where its provenance shows a read of the
// application's clock (typed.go).
//
// An empty recorded value is never followed: it carries nothing to follow.
// dc keeps what each value drifted for the test (nil: for this call).
func Classify(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf) []Learned {
	return ClassifyIn(dc, drift, live, rec, at, earlier, raw, resp, nil)
}

// ClassifyIn is Classify of a call whose clock reads were made in w
// (Tracker.CallWindow; nil: none): a drifted instant that is a read of each
// run's clock within it is a clock read, as one the lead moved is.
//
// A leaf its protocol types as one instant (Instant) is classified whole,
// never word by word (typed.go). A Raw that was not read from its mock
// (Raw.Read) refuses every drift: what it held cannot be told. Where the test
// case's own request could not be read (Earlier.Unread), every value that
// changed is refused rather than learned as noise or an echo: it may have
// come from the request, and keploy cannot tell. In keploy mock's client
// tests (DriftCache.ClientTests), which have no request place, a value with
// an instant that changed is noise or an echo only where each such instant
// lies, in both runs, within w, read on the application's clock: the lead
// shows no read there (given.go).
func ClassifyIn(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window) []Learned {
	return ClassifyGiven(dc, drift, live, rec, at, earlier, raw, resp, w, nil)
}

// GivenEarlier reads the recordings F gave the application earlier in the
// running set that no other rule reads (an earlier test's than the previous
// one, another recorded test's, the call's own paired recording F served
// before it), each as F served it (GivenPlaces.ServedIn), for the offset rule,
// the word rule and the echo of what F gave there (echoedElsewhere) alone:
// their places, their answers' words and instants, read once for the set,
// without their text (Order.GivenPlaces). It is read only where a changed
// instant reaches the offset rule or the echo, or a word of what changed
// reaches the word rule (givenWhy), and then once for the call. An error, or
// a recording it could not read (GivenPlaces.Read), names one whose text
// cannot be told: every changed instant or word that reaches either rule is
// then refused with it, never learned as noise.
type GivenEarlier func() ([]GivenPlaces, error)

// ClassifyGiven is ClassifyIn with the recordings F gave the application
// earlier in the set (given; nil: none), which the offset rule, the word rule
// and the echo of what F gave there read beside raw's.
func ClassifyGiven(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned {
	return classifyGiven(dc, drift, live, rec, at, earlier, raw, resp, w, given, false)
}

// classifyGiven is ClassifyGiven, its word rule read under the rule a copy
// follows its source where follow is set (ClassifyFollowing, follows.go).
//
// A recording record's full round gave the application before the call that
// was made at the call's own instant (a fixture's whole second, or two calls
// of one clock tick) cannot be shown, from the record, to have been given to
// the recorded application before the call or after it. So it is read both
// ways, and a drift is learned by the stricter of the two (stricterOf): as an
// earlier place of the call's own test, as one recorded a moment before the
// call is (Raw.earlierThan), and as a recording made at or after the call,
// which only what F served there counts for (OfAnotherTest: the offset rule,
// the word rule and the echo of what F gave, never a variable's search or
// the call's own echo). Read as an earlier place only, an instant of its
// answer that names the moment F saw by one rule and the recorded moment by
// another (15:50:40, rounded and cut to the minute) is the place both runs
// were given, so no copy, and the value would be learned as noise, while the
// same recording read as made after the call shows F's copy of what it
// served there, and refuses it. A value the first reading binds (a variable
// or an echo) and the second refuses is refused too, and its reason names
// where the first would have bound it (stricterOf): fail closed.
//
// An exchange served leaf by leaf (Earlier) is read by when its request was
// recorded as a Raw is: recorded after the call, as a recording made after
// the call in both readings (leafedAfterCall); at the call's own instant,
// both ways (leafedAtCall). Its leaves would otherwise read as an earlier
// place both runs were given, where a minute F's run rounded from what F
// served there names both moments and is learned as noise.
func classifyGiven(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier, follow bool) []Learned {
	if dc == nil {
		dc = NewDriftCache()
	}
	given = givenOnce(given)
	earlier = timedEarlier(earlier, at)
	raw = timedRaw(raw, at)
	earlier, raw = leafedAfterCall(earlier, raw, at)
	out := classifyRead(dc, drift, live, rec, at, earlier, raw, resp, w, given, follow)
	late, ok := givenAtCall(raw, at, given)
	if !ok {
		late = raw
	}
	lateEarlier, leafedLate := leafedAtCall(earlier, at)
	if !ok && len(leafedLate) == 0 {
		return dc.withAppZone(out, at)
	}
	if len(leafedLate) > 0 {
		late = append(append([]Raw(nil), late...), leafedLate...)
	}
	alone := classifyRead(dc, drift, live, rec, at, lateEarlier, late, resp, w, given, follow)
	for i := range out {
		out[i] = stricterOf(out[i], alone[i])
	}
	return dc.withAppZone(out, at)
}

// withAppZone is out with each clock read whose learning depends on the
// application's zone (models.ClockOffset.AppZoneMatters: a wall clock of no
// zone read in it, a value that names its moment cut coarser than a
// minute) naming the zone dc read it in (DriftCache.zone, zoneName;
// models.ClockOffset.AppZone), which a replay then reads it in on any
// machine where it cannot read the application's own zone in that run
// (CheckClockRead). One keploy could not read or name (dc read it in the
// process's zone, read with no name, else in the agent's own: InZone,
// Tracker.SetAppZoneUnnamed) names none: a replay that cannot read the
// application's zone either reads it in the agent's own, refusing a plain
// read cut to an hour or coarser, and its Meet with a named learning of the place names no zone
// (models.AppZoneApart), which a replay checks as CheckClockRead says.
func (dc *DriftCache) withAppZone(out []Learned, at time.Time) []Learned {
	if dc.zone == nil || dc.zoneNoName {
		return out
	}
	name := dc.zoneName
	if name == "" {
		if at.IsZero() {
			at = time.Now()
		}
		name = models.ZoneNameOf(dc.zone, at)
	}
	if name == "" {
		return out
	}
	for i := range out {
		for _, p := range []**models.ClockOffset{&out[i].Offset, &out[i].Read} {
			if *p != nil && (*p).AppZone == "" && (*p).AppZoneMatters() {
				o := **p
				o.AppZone = name
				*p = &o
			}
		}
	}
	return out
}

// leafedAfterCall is earlier without each exchange served leaf by leaf
// (Earlier) whose request was recorded after the call (at), and raw with
// each of them as a recording F gave the application before the call that is
// no earlier place of the call's own test (lateOf): it gave the recorded
// application nothing before the call, wherever F served it this run, so
// only what F served there counts (the offset rule, the word rule and the
// echo of what F gave), as for a Raw recorded after the call, never its
// leaves as a place both runs were given, which a variable binds at or which
// shows a moment of F's no copy. The test case's own request (Given) is no
// recording, and stays.
func leafedAfterCall(earlier []Earlier, raw []Raw, at time.Time) ([]Earlier, []Raw) {
	var kept []Earlier
	var moved []Raw
	for i, e := range earlier {
		if e.Given || !e.At.After(at) {
			if moved != nil {
				kept = append(kept, e)
			}
			continue
		}
		if moved == nil {
			kept = append([]Earlier(nil), earlier[:i]...)
		}
		moved = append(moved, e.lateOf(false)...)
	}
	if moved == nil {
		return earlier, raw
	}
	return kept, append(append([]Raw(nil), raw...), moved...)
}

// timedEarlier is earlier with each exchange served leaf by leaf whose
// recorded time keploy cannot read (the zero time) read as one recorded at
// the call's own instant (at), and marked so (noTime): the record cannot show
// it was given to the recorded application before the call or after it, so
// it is read both ways (leafedAtCall), refused where either reading refuses,
// and a reason naming it says keploy cannot read when it was recorded. earlier
// is returned as it is where there is none.
func timedEarlier(earlier []Earlier, at time.Time) []Earlier {
	var out []Earlier
	for i, e := range earlier {
		if e.Given || !e.At.IsZero() || at.IsZero() {
			continue
		}
		if out == nil {
			out = append([]Earlier(nil), earlier...)
		}
		e.At, e.noTime = at, true
		if e.Payload.at.IsZero() {
			e.Payload.at = at
		}
		e.Payload.noTime = true
		out[i] = e
	}
	if out == nil {
		return earlier
	}
	return out
}

// timedRaw is raw with each recording of the call's own test whose recorded
// time keploy cannot read (the zero time) read as one made at the call's own
// instant (at), and marked so (noTime), as timedEarlier reads an exchange
// served leaf by leaf: the record cannot show it was given to the recorded
// application before the call or after it, so it is read both ways
// (givenAtCall), refused where either reading refuses, and a reason naming
// it says keploy cannot read when it was recorded. A recording of another
// test's (OfAnotherTest) is no place of the call's own test in either
// reading, and stays. raw is returned as it is where there is none.
func timedRaw(raw []Raw, at time.Time) []Raw {
	var out []Raw
	for i, x := range raw {
		if x.elsewhere || x.noTime || !x.at.IsZero() || at.IsZero() {
			continue
		}
		if out == nil {
			out = append([]Raw(nil), raw...)
		}
		x.at, x.noTime = at, true
		out[i] = x
	}
	if out == nil {
		return raw
	}
	return out
}

// leafedAtCall is earlier without each exchange served leaf by leaf whose
// request was recorded at the call's own instant (at; one whose recorded time
// keploy cannot read is read as one: timedEarlier), and each of them as a
// recording made at or after the call (lateOf), for the second reading of
// the call (classifyGiven): F served it in the test before the call, and the
// record cannot show it was given to the recorded application before the
// call or after it, so it is read both ways, as a Raw made at that instant is
// (givenAtCall), and refused where either reading refuses. late is nil where
// there is none, and earlier is then returned as it is.
func leafedAtCall(earlier []Earlier, at time.Time) (kept []Earlier, late []Raw) {
	for i, e := range earlier {
		if e.Given || !e.At.Equal(at) {
			if late != nil {
				kept = append(kept, e)
			}
			continue
		}
		if late == nil {
			kept = append([]Earlier(nil), earlier[:i]...)
		}
		late = append(late, e.lateOf(true)...)
	}
	if late == nil {
		return earlier, nil
	}
	return kept, late
}

// lateOf is e, an exchange F served leaf by leaf before the call, as a
// recording F gave the application before the call that is no earlier place
// of the call's own test (Raw.OfAnotherTest), each served (late: one made at
// the call's own instant, read both ways: Raw.late): its payload, and what F
// served at each leaf of its answer (servedAnswer), since only what F served
// there counts. An exchange whose payload could not be read, or that came
// with none, is a recording whose text cannot be told: every drift is
// refused with payloadUnread's reason (rawUnread), never learned on the
// strength of its absence.
func (e Earlier) lateOf(late bool) []Raw {
	x := e.Payload
	if !x.read {
		x = Raw{mock: e.Mock, at: e.At, answered: e.Payload.answered, conn: e.Payload.conn, pos: e.Payload.pos, noTime: e.noTime, unread: payloadUnread(e)}
	}
	x.served = true
	out := []Raw{x}
	if a, ok := e.servedAnswer(); ok {
		out = append(out, a)
	}
	for i := range out {
		out[i] = out[i].OfAnotherTest()
		out[i].late = late
	}
	return out
}

// servedAnswer is what F served at each leaf of e's answer (Seen of its
// Response, Live; never its answer's stamp), as a recording's text whose
// fields stand where the payload's answer does ("spec.resp.<leaf>"), served
// as the payload was: the values F gave the application there, a template
// value too, which its payload, the recording, may not hold. Its fields are
// the leaves' alone, none a key. The offset rule reads it by the leaves F
// served with another value than recorded (moves: servedMoves), keyed where
// the payload holds them, never by its own fields: those name no place
// another recording's payload does. false where e names no leaf of its
// answer.
func (e Earlier) servedAnswer() (Raw, bool) {
	root := e.answerRoot()
	x := Raw{mock: e.Mock, at: e.At, answered: e.Payload.answered, conn: e.Payload.conn, pos: e.Payload.pos, served: true, read: true, first: e.Payload.first, last: e.Payload.last, call: e.Payload.call, noTime: e.noTime, moves: e.servedMoves()}
	var sb strings.Builder
	for _, s := range e.Seen {
		if s.Side != Response || s.Stamp {
			continue
		}
		start := sb.Len()
		sb.WriteString(s.Live)
		x.fields = append(x.fields, rawField{start: start, end: sb.Len(), path: root + "." + s.Leaf.Path})
		sb.WriteByte('\n')
	}
	if len(x.fields) == 0 {
		return Raw{}, false
	}
	x.text = sb.String()
	return x, true
}

// answerRoot is the field of e's payload that holds its answer (answerFields:
// "Res" of an HTTP exchange), "Res" where the payload names none.
func (e Earlier) answerRoot() string {
	for _, f := range e.Payload.fields {
		if r := fieldRoot(f.path); answerFields[r] {
			return r
		}
	}
	return "Res"
}

// servedMoves are the leaves of e's answer F served with another value than
// recorded (a template value; never its answer's stamp), each where e's
// payload holds it (servedMove): the payload's field it stands in, the
// longest whose path is the leaf's or holds it, and the key written before
// it there. A leaf no field of the payload holds stands in a field of its
// own.
func (e Earlier) servedMoves() []servedMove {
	root := e.answerRoot()
	var out []servedMove
	for _, s := range e.Seen {
		if s.Side != Response || s.Stamp || s.Live == s.Leaf.Value {
			continue
		}
		full := root + "." + s.Leaf.Path
		in := ""
		for _, f := range e.Payload.fields {
			if f.key || len(f.path) <= len(in) {
				continue
			}
			if full == f.path || strings.HasPrefix(full, f.path+".") || strings.HasPrefix(full, f.path+"[") {
				in = f.path
			}
		}
		mv := servedMove{path: full, in: in, live: s.Live, quoted: s.Leaf.Type == String}
		if in == "" {
			mv.in = full
		}
		if rel := full[len(mv.in):]; strings.Contains(rel, ".") {
			name := rel[strings.LastIndexByte(rel, '.')+1:]
			if i := strings.IndexByte(name, '['); i >= 0 {
				name = name[:i]
			}
			mv.name = name
		}
		out = append(out, mv)
	}
	return out
}

// givenOnce is given read at most once, however often it is asked: both
// readings of a call (classifyGiven) read the set as one.
func givenOnce(given GivenEarlier) GivenEarlier {
	if given == nil {
		return nil
	}
	var (
		read bool
		set  []GivenPlaces
		err  error
	)
	return func() ([]GivenPlaces, error) {
		if !read {
			set, err = given()
			read = true
		}
		return set, err
	}
}

// givenAtCall is raw with each earlier place of the call's own test that
// was recorded at the call's own instant (at) and that F gave the
// application before the call (served in the test or before it: Raw.served;
// or anywhere earlier in the running set: given lists it) read as a
// recording made at or after the call is (OfAnotherTest), and whether there
// was one. One whose recorded time keploy cannot read (timedRaw) is such a
// place, and is looked up in the set by the time its mock was recorded with
// (the zero time), as the set lists it. The set is read only where such a
// place was not handed as served, and where it cannot be read, the place is
// taken as one F gave, the fail-closed side: the second reading then refuses
// what reaches the set.
func givenAtCall(raw []Raw, at time.Time, given GivenEarlier) ([]Raw, bool) {
	var (
		out     []Raw
		gave    map[string]bool
		gaveAll bool
		asked   bool
	)
	for i, x := range raw {
		if x.elsewhere || !x.at.Equal(at) {
			continue
		}
		if !x.served {
			if given == nil {
				continue
			}
			if !asked {
				asked = true
				set, err := given()
				gaveAll = err != nil
				gave = make(map[string]bool, len(set))
				for _, g := range set {
					gave[g.mock+"\x00"+g.at.String()] = true
				}
			}
			if !gaveAll && !gave[x.mock+"\x00"+x.recordedAt().String()] {
				continue
			}
		}
		if out == nil {
			out = append([]Raw(nil), raw...)
		}
		out[i] = x.OfAnotherTest()
		out[i].late = true
	}
	return out, out != nil
}

// stricterOf is the verdict of a drift read two ways (classifyGiven): the
// refusal of either, the earlier place's (asPlace) first; else what asPlace
// learned. The second reading reads fewer places of the call's own test and
// no more of anything else, so it finds no variable or echo that the first
// does not: only a refusal of its own.
//
// A value the earlier place's reading binds (a variable, or an echo) and
// the late reading refuses is refused too (fail closed): the recording may
// as well have been made after the call, where nothing binds it, and the
// value may be a copy of what F served there. Its reason says so, naming
// where the first reading would have bound it. That refuses an application
// that really did copy the value from a recording made at the call's own
// instant (a false red, never a false green).
//
// A value the late reading learns as the clock plus an offset
// (KindClockOffset), which the earlier place's reading binds (a variable,
// or an echo) or learns as noise or a plain read (KindClockRead), is
// refused too: the two readings explain
// it differently, and keploy cannot tell which the application did. One
// the earlier place's reading refuses keeps that refusal and its reason.
// (The earlier place's reading never learns an offset the late one does
// not: both read the clock alike, and the late one finds no binding the
// first does not.)
func stricterOf(asPlace, asLate Learned) Learned {
	if asPlace.Kind != KindUnaccounted && asLate.Kind == KindUnaccounted {
		if b := boundAt(asPlace); b != "" {
			asLate.Why += "; read as a recording made before the call, the value would be " + b + ", yet keploy refuses a value either reading refuses"
		}
		return asLate
	}
	if asLate.Kind == KindClockOffset && asPlace.Kind != KindClockOffset {
		if asPlace.Kind == KindUnaccounted {
			return asPlace
		}
		b := boundAt(asPlace)
		switch {
		case b != "":
		case asPlace.Kind == KindClockRead:
			b = "a plain read of the application's clock, at no offset"
		default:
			b = "noise, which no replay compares"
		}
		asLate.Why = "read as a recording made after the call, " + clockOffsetSeen(fieldName(asLate.Recorded), *asLate.Offset) + "; read as one made before it, the value would be " + b + ", and keploy refuses a value the two readings explain differently"
		asLate.Kind, asLate.Offset = KindUnaccounted, nil
		return asLate
	}
	return asPlace
}

// boundAt says where a reading binds its drifted value: a variable of the
// place it came from, or an echo of the call's answer; "" for any other.
func boundAt(ln Learned) string {
	switch ln.Kind {
	case KindVariable:
		return "a variable bound at " + placeAt(ln.From)
	case KindEcho:
		return "an echo of the call's own answer"
	}
	return ""
}

// classifyRead is classifyGiven of raw as handed: each Raw read as the one
// place it is.
func classifyRead(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier, follow bool) []Learned {
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	unreadRaw := rawUnread(raw)
	unreadReq := requestUnread(served)
	// The moved places the offset rule reads (offset.go), read once for the
	// call, and only where a changed instant or word reaches it or the word
	// rule (givenWhy): another recorded test's too (OfAnotherTest), and
	// those F gave earlier in the set (given), which no other rule reads.
	all := raw
	raw, _ = ownPlaces(raw)
	var op *offsetPlaces
	places := func() *offsetPlaces {
		if op == nil {
			var unread string
			var set []GivenPlaces
			if given != nil {
				more, err := given()
				switch {
				case err != nil:
					unread = "keploy cannot read back a recording record's full round gave the application earlier in the test set, so it cannot tell whether what changed came from it or was built on it: " + err.Error()
				default:
					unread, set = placesUnread(more), more
				}
			}
			op = dc.offsetPlacesOf(at, served, all, set)
			if unread != "" {
				op.unread = unread
			}
		}
		return op
	}
	out := make([]Learned, 0, len(drift))
	for _, p := range drift {
		r, l := rec.Leaves[p], live.Leaves[p]
		if unreadRaw != "" {
			out = append(out, Learned{Kind: KindUnaccounted, Recorded: r, Live: l, Why: unreadRaw})
			continue
		}
		if r.Type == Instant || l.Type == Instant {
			out = append(out, dc.classifyInstant(r, l, at, served, raw, resp, w, unreadReq, places))
			continue
		}
		ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
		var fol *follows
		if follow {
			fol = &follows{dc: dc, rec: r.Value, live: l.Value, at: at}
		}
		if r.Value != "" {
			if from, ok := appearedIn(dc, dc.ofIn(r.Value, l.Value, w, at), at, served, raw, fol); ok {
				ln.Kind, ln.From = KindVariable, from
				if from.Side == "" || from.Echo {
					ln.Why = evidence(dc.ofIn(r.Value, l.Value, w, at), from)
				}
				if from.startUp {
					ln.Kind, ln.From, ln.Why = KindUnaccounted, Place{}, startUpWhy(dc.ofIn(r.Value, l.Value, w, at), from)
				}
			} else if echo := Echoes(dc, r, resp); len(echo) > 0 {
				ln.Kind, ln.Echo = KindEcho, echo
			}
		}
		// Noise, or an echo, only where the test case's own request could be
		// read, and where it, as recorded, holds no moment the value was
		// recorded with that the request as sent does not give F's by (a
		// pick from it by the clock whose pick in F the request does not
		// list); and only where its readings account for all it changed. An
		// echo is answered, never asserted against a source.
		if ln.Kind == KindNoise || ln.Kind == KindEcho {
			v := dc.ofIn(r.Value, l.Value, w, at)
			why := ""
			if unreadReq != "" {
				why = unreadRequestWhy(unreadReq)
			} else if where, asRecorded := dc.requestHolds(lostMoments(v), v.liveMoments, served); where != "" {
				why = requestHoldsWhy(where, asRecorded)
			} else if m, ok := offsetOfValue(v, places); ok {
				// An instant of it built on a moved place by an offset, before
				// any lead or window excuses its words (offset.go).
				why = offsetWhy(m)
			} else if why = unaccounted(dc, v, at, served, raw, fol); why != "" {
				if fol != nil && fol.refused != "" {
					why += "; " + fol.refused
				}
			} else {
				// Nor a word of it in what F gave the application in
				// another test or in a recording made after the call, nor
				// an instant of it copied from there, or picked from it,
				// which the word rule leaves to the clock (the echo of
				// those places).
				if why = givenWhy(dc, v, at, served, raw, places); why == "" {
					why = dc.echoedElsewhere(v.instants, at, served, raw, places)
				}
			}
			// In keploy mock's client tests, a changed instant only where the
			// call's window shows it a read of the application's clock.
			if why == "" && dc.clientTests {
				why = dc.clientTestUnread(v, w)
			}
			if why != "" {
				ln.Kind, ln.Why, ln.Echo = KindUnaccounted, why, nil
			}
		}
		// Noise only where no word of what changed comes back in the
		// call's own answer: an id sent inside a longer value (a GraphQL
		// argument, an OData filter) that the answer holds is an echo keploy
		// cannot answer with this run's value.
		if ln.Kind == KindNoise {
			if why := echoedPart(dc, dc.ofIn(r.Value, l.Value, w, at), resp); why != "" {
				ln.Kind, ln.Why = KindUnaccounted, why
			}
		}
		// Words the word rule let go in record's full round: noise only
		// where the recording and F gave the application the places before
		// the call in the same order (follows.followed); an echo they stood
		// beside is refused as the word rule refuses them.
		//
		// A source the search went past (follows.refute) counts as such a
		// word, whether or not the word rule let one go there: noise only
		// in the same order, and named in the noise it is learned as.
		if fol != nil && (fol.first != "" || fol.refuted != "") {
			switch ln.Kind {
			case KindNoise:
				ln = fol.followed(ln, at, served, all)
				switch {
				case ln.Kind != KindNoise:
				case fol.refuted != "":
					ln.Why = fol.refuted + ", and keploy learns it as varying"
				case fol.moved != "":
					ln.Why = fol.moved + ", and keploy learns it as varying"
				}
			case KindEcho:
				why := fol.first
				if why == "" {
					why = fol.refuted + "; but the call's recorded answer holds it, an echo keploy cannot answer beside a source it set aside"
				}
				ln.Kind, ln.Why, ln.Echo = KindUnaccounted, why, nil
			}
		}
		// A time of day whole (textTimeOfDay), which names no day, is a
		// read modulo a day (textTodRead), checked so at every replay.
		if ln.Kind == KindNoise && dc.ofIn(r.Value, l.Value, w, at).clock.whole {
			ln.Read = textClockReadIn(r.Value, l.Value, dc.zone)
			if ln.Read == nil {
				ln.Read = dc.textTodRead(r.Value, l.Value, w, at)
			}
		}
		// A value that is one instant whole in both runs (readClockText),
		// learned as noise that the lead did not show a read of the clock
		// (Read nil), is never left noise, which would let any value pass at
		// every replay. In a call with no window (a test's own call, read by
		// the lead alone, or one whose window keploy could not tell) it is a
		// plain read of the clock, which every replay, record's confirming
		// round too, checks in its call's own span on the application's
		// clock (CheckClockRead): whether it moved by the lead in record's
		// full round depends on how late that round ran, which its verdict
		// must not. In a call whose window holds it in neither zone the
		// application's clock may write (UTC, the application's own:
		// Window.holdsRead) it is refused, as a read made before that window
		// or no plain read of the clock (outsideWindowWhy).
		//
		// Where keploy cannot tell which clock the application reads
		// (DriftCache.appClock), a call has no window and no replay can
		// check a read of the clock (Tracker.LiveSpan): one with no window
		// is refused here, at learn (noAppClockWhy), never learned as a read
		// only a replay refuses.
		//
		// One its window holds only as a wall clock of no zone in the
		// application's zone is a plain read of the clock too, never noise
		// left unchecked. Each read of a wall clock of no zone carries the
		// zone the learn step read it in (textReadZone), and a replay
		// checks it in that zone alone (clockReadIn).
		//
		// A time of day whole in both runs, which no window holds (it names
		// no day), is a read modulo a day all the same, never noise: every
		// replay checks it in its call's span (textTodRead).
		if ln.Kind == KindNoise && ln.Read == nil {
			if read := dc.textTodRead(r.Value, l.Value, w, at); read != nil {
				if w == nil && !dc.appClock {
					ln.Kind, ln.Why = KindUnaccounted, noAppClockWhy
				} else {
					ln.Read = read
				}
			} else if read := textClockReadIn(r.Value, l.Value, dc.zone); read != nil {
				switch {
				case w == nil && !dc.appClock:
					ln.Kind, ln.Why = KindUnaccounted, noAppClockWhy
				case w == nil || dc.windowHolds(dc.ofIn(r.Value, l.Value, w, at), w):
					ln.Read = read
				default:
					ln.Kind, ln.Why = KindUnaccounted, outsideWindowWhy
				}
			}
		}
		// A read whose two runs wrote it in two layouts (an ISO text, then
		// an epoch number; a time to the second, then to the minute), or in
		// one layout that names its zone in one run and none in the other
		// ("…Z", then a wall clock of no zone), is refused: a replay checks
		// the form its learn step saw, and this one saw two.
		switch {
		case ln.Read != nil && ln.Read.Layout == models.LayoutApart:
			ln.Kind, ln.Why, ln.Read = KindUnaccounted, textLayoutChangedWhy(r.Value, l.Value), nil
		case ln.Read != nil:
			if why := textFormChangedWhy(r.Value, l.Value); why != "" {
				ln.Kind, ln.Why, ln.Read = KindUnaccounted, why, nil
				break
			}
			if !models.LayoutTimeOfDay(ln.Read.Layout) {
				ln.Read.Zone = dc.textReadZone(r.Value, l.Value, w, at)
			}
			ln.Kind, ln.Why = KindClockRead, clockReadUncheckedWhy
		}
		out = append(out, ln)
	}
	return out
}

// rawUnread says why the classifier refuses raw: one of them was not read
// from its mock (Raw.Read: the zero Raw, or one built otherwise than by
// RawOf), so what it held cannot be told; "" when every one was.
func rawUnread(raw []Raw) string {
	for _, x := range raw {
		if x.read {
			continue
		}
		if x.unread != "" {
			return x.unread
		}
		name := "a recording of the test"
		if x.mock != "" {
			name = x.mock
		}
		return "keploy could not read " + name + ", an earlier place of the call, from its mock (learn.RawOf), so it cannot tell whether what changed came from there: a fault of keploy's, which keploy reports rather than learn anything"
	}
	return ""
}

// echoedPart says where the call's recorded answer (resp) holds a word of
// what drifted value v changed (a word it lost, or one outside the longest
// common subsequences, recorded side; no part of a clock read the lead
// moved, or read in its call's window), in any declared form of the answer,
// in its case or folded; "" when nowhere. Every value is read so, whatever it
// looks like: the words of an instant's date and time are words like any
// other. An answer's leaf its protocol types as one instant has none.
func echoedPart(dc *DriftCache, v *valueDrift, resp []Leaf) string {
	for i := range readings {
		words := map[string]bool{}
		for t := range v.lost[i] {
			if !clockWord(t, true, v, v.clock) {
				words[t] = true
			}
		}
		for t := range v.movedRec[i] {
			if !allClock(t, true, v.clock) {
				words[t] = true
			}
		}
		if len(words) == 0 {
			continue
		}
		for _, a := range resp {
			if a.Value == "" || a.Type == Instant {
				// An answer's instant its protocol types has no words: one
				// that holds a moment of the value is an echo (Echoes).
				continue
			}
			toks := dc.rawTokens(a.Value)[i]
			for t := range words {
				if toks[t] || toks[strings.ToLower(t)] {
					return "a word of it comes back in the answer at " + a.Path + ", which keploy cannot answer with this run's value"
				}
			}
		}
	}
	return ""
}

// run is a part of a value that drifted between record and F: what the
// recording held there and what F saw there.
type run struct{ rec, live string }

// segment is a stretch of a value that is all word characters (letters and
// digits) or none.
type segment struct {
	text string
	word bool
}

// wordByte reports whether c continues a word of a value: an ASCII letter
// or digit, or any byte of a character beyond ASCII (an emoji, a CJK or
// Cyrillic word drifts as a word too).
func wordByte(c byte) bool { return isWord(c) || c >= 0x80 }

// idByte reports whether c continues an id of a value: a word byte, or '-'
// or '_', which ids of the URL-safe alphabets carry inside them (nanoid,
// base64url, token_urlsafe), so the same id splits into a different number
// of words run to run.
func idByte(c byte) bool { return wordByte(c) || c == '-' || c == '_' }

// segmentsBy are the stretches of s that are all bytes word says continue a
// word, or none.
func segmentsBy(s string, word func(byte) bool) []segment {
	var out []segment
	for i := 0; i < len(s); {
		w, j := word(s[i]), i+1
		for j < len(s) && word(s[j]) == w {
			j++
		}
		out = append(out, segment{s[i:j], w})
		i = j
	}
	return out
}

// runsBy are the parts of a drifted value that drifted (its runs):
// recorded and live read word by word (as word says), each maximal stretch
// of changed words that no unchanged word interrupts, with what separates
// them ("<uuid>" of "<uuid>.json", "00:41Z" of a clock reading, both ids of
// "a-<id>-b-<id>"). A recorded and a live value of different makes are one
// stretch: what lies between their common start and end, with the words it
// touches.
func runsBy(recorded, live string, word func(byte) bool) []run {
	r, l := segmentsBy(recorded, word), segmentsBy(live, word)
	aligned := len(r) == len(l) && len(r) > 0 && r[0].word == l[0].word
	if !aligned {
		p, q := 0, 0
		for p < len(recorded) && p < len(live) && recorded[p] == live[p] {
			p++
		}
		for q < len(recorded)-p && q < len(live)-p && recorded[len(recorded)-1-q] == live[len(live)-1-q] {
			q++
		}
		rs, re := widen(recorded, p, len(recorded)-q, word)
		ls, le := widen(live, p, len(live)-q, word)
		if !hasWord(recorded[rs:re]) || !hasWord(live[ls:le]) {
			return nil
		}
		return []run{{recorded[rs:re], live[ls:le]}}
	}
	var out []run
	from := -1 // the run's first changed word
	last := -1 // its last
	flush := func() {
		if from >= 0 {
			var rb, lb strings.Builder
			for i := from; i <= last; i++ {
				rb.WriteString(r[i].text)
				lb.WriteString(l[i].text)
			}
			out = append(out, run{rb.String(), lb.String()})
		}
		from, last = -1, -1
	}
	for i := range r {
		if !r[i].word {
			continue
		}
		if r[i].text == l[i].text {
			flush()
			continue
		}
		if from < 0 {
			from = i
		}
		last = i
	}
	flush()
	return out
}

// widen is [i, j) of s with the words it touches at either end.
func widen(s string, i, j int, word func(byte) bool) (int, int) {
	if i > j {
		i = j
	}
	for i > 0 && word(s[i-1]) {
		i--
	}
	for j < len(s) && word(s[j]) {
		j++
	}
	return i, j
}

func hasWord(s string) bool {
	for i := 0; i < len(s); i++ {
		if wordByte(s[i]) {
			return true
		}
	}
	return false
}

// wordPairsBy are the changed words of a run, as word reads them, recorded
// and live, when both sides of it hold as many words (nil otherwise).
func wordPairsBy(x run, word func(byte) bool) []run {
	r, l := segmentsBy(x.rec, word), segmentsBy(x.live, word)
	if len(r) != len(l) {
		return nil
	}
	var out []run
	for i := range r {
		if r[i].word && l[i].word && r[i].text != l[i].text {
			out = append(out, run{r[i].text, l[i].text})
		}
	}
	return out
}

// readings are the ways a value is read into words: ASCII letters and
// digits (an id inside text of another script, "订单A7K29Q已发货"), those and
// any character beyond ASCII (an emoji, a CJK or Cyrillic word), and ids
// with '-' and '_' inside them (nanoid, base64url, token_urlsafe).
var readings = []func(byte) bool{isWord, wordByte, idByte}

// pieces are the parts of a drifted value that drifted, each recorded and
// live: its runs and their changed words, in every reading.
func pieces(recorded, live string) []run {
	seen := map[run]bool{}
	var out []run
	add := func(x run) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, word := range readings {
		for _, x := range runsBy(recorded, live, word) {
			add(x)
			for _, w := range wordPairsBy(x, word) {
				add(w)
			}
		}
	}
	return out
}

// maxMovedCells bounds the table movedWords compares two word lists in;
// beyond it every word between their common start and end has moved.
const maxMovedCells = 1 << 18

// movedWords are the words of rec and of live (a value's words, recorded and
// live, in order) that are no part of a longest common subsequence of the
// two: a word lost or gained, one held once more, and one of each pair held
// in another order. Words that kept their order stay, however much changed
// around them. Two longest subsequences are taken, ties broken each way, and
// a word outside either has moved, so of two words that swapped both have.
func movedWords(rec, live []string) (movedRec, movedLive []string) {
	p := 0
	for p < len(rec) && p < len(live) && rec[p] == live[p] {
		p++
	}
	q := 0
	for q < len(rec)-p && q < len(live)-p && rec[len(rec)-1-q] == live[len(live)-1-q] {
		q++
	}
	r, l := rec[p:len(rec)-q], live[p:len(live)-q]
	if len(r) == 0 || len(l) == 0 || len(r)*len(l) > maxMovedCells {
		return r, l
	}
	// lcs[i][j] is the length of a longest common subsequence of r[i:] and
	// l[j:].
	w := len(l) + 1
	lcs := make([]int32, (len(r)+1)*w)
	for i := len(r) - 1; i >= 0; i-- {
		for j := len(l) - 1; j >= 0; j-- {
			switch {
			case r[i] == l[j]:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	keptR, keptL := make([]int, len(r)), make([]int, len(l))
	for _, skipRec := range []bool{true, false} {
		i, j := 0, 0
		for i < len(r) && j < len(l) {
			switch {
			case r[i] == l[j] && lcs[i*w+j] == lcs[(i+1)*w+j+1]+1:
				keptR[i]++
				keptL[j]++
				i, j = i+1, j+1
			case lcs[(i+1)*w+j] > lcs[i*w+j+1] || lcs[(i+1)*w+j] == lcs[i*w+j+1] && skipRec:
				i++
			default:
				j++
			}
		}
	}
	for i, k := range keptR {
		if k < 2 {
			movedRec = append(movedRec, r[i])
		}
	}
	for j, k := range keptL {
		if k < 2 {
			movedLive = append(movedLive, l[j])
		}
	}
	return movedRec, movedLive
}

// isClock reports whether p (what a value held when recorded, and in F)
// drifted only with F's clock: each ASCII word of it that changed (that the
// other side does not hold) is a part of a clock read that moved by the
// lead, in either value (cs), and one did.
func isClock(p run, cs ...clockSet) bool {
	r, l := tokensBy(p.rec, isWord), tokensBy(p.live, isWord)
	changed := false
	for w := range r {
		if l[w] {
			continue
		}
		changed = true
		if !clockSide(w, true, cs) {
			return false
		}
	}
	for w := range l {
		if r[w] {
			continue
		}
		changed = true
		if !clockSide(w, false, cs) {
			return false
		}
	}
	return changed
}

// clockSide reports whether word w is a clock word of one of cs, on its
// recorded side or its live one.
func clockSide(w string, recorded bool, cs []clockSet) bool {
	for _, c := range cs {
		if recorded && c.rec[w] || !recorded && c.live[w] {
			return true
		}
	}
	return false
}

// tokensBy are the words of s as word reads it.
func tokensBy(s string, word func(byte) bool) map[string]bool {
	out := map[string]bool{}
	for _, seg := range segmentsBy(s, word) {
		if seg.word {
			out[seg.text] = true
		}
	}
	return out
}

// wordsBy are the words of s as word reads it, in order.
func wordsBy(s string, word func(byte) bool) []string {
	var out []string
	for _, seg := range segmentsBy(s, word) {
		if seg.word {
			out = append(out, seg.text)
		}
	}
	return out
}

// inOrder are words, a set of v's words (of its recorded value, or of what F
// saw in it: recorded), in the order they stand there: where each first
// stands as a word in the value, then in each of its other forms in turn
// (decoded, case-folded, encoded), and by text where two stand at one place
// or at none. Wherever the first word a search finds names a place or a
// reason, the words are searched in this order: one input, one reason.
func (v *valueDrift) inOrder(words map[string]bool, recorded bool) []string {
	out := make([]string, 0, len(words))
	for w := range words {
		out = append(out, w)
	}
	texts := []string{v.live}
	if recorded {
		texts[0] = v.rec
	}
	for _, f := range v.forms {
		if recorded {
			texts = append(texts, f[0])
		} else {
			texts = append(texts, f[1])
		}
	}
	pos := make(map[string]int, len(out))
	for _, w := range out {
		off := 0
		p := -1
		for _, t := range texts {
			if i := tokenIndex(t, w); i >= 0 {
				p = off + i
				break
			}
			off += len(t) + 1
		}
		if p < 0 {
			p = off
		}
		pos[w] = p
	}
	sort.Slice(out, func(i, j int) bool {
		if pos[out[i]] != pos[out[j]] {
			return pos[out[i]] < pos[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// valueDrift is what one drifted value (rec, live) changed, read once per
// test: its clock reads the lead moved, the words each reading finds the
// recorded value lost and the live one gained (in every declared form), its
// pieces, and its decoded form.
type valueDrift struct {
	rec, live    string
	clock        clockSet
	lost, gained []map[string]bool // by reading
	// movedRec and movedLive are the words of what changed, recorded and
	// live, by reading, in every form: the words outside a longest common
	// subsequence of the recorded and the live words (movedWords). A word
	// held in another order, or once more, is one.
	movedRec, movedLive []map[string]bool
	pieces              []run
	decRec, decLive     string
	decoded             bool
	// forms are the value in the declared forms: raw where it has escapes,
	// and formsOf.
	forms [][2]string
	// recTok and liveTok are every word of the value, recorded and live,
	// in every form, by reading.
	recTok, liveTok []map[string]bool
	// encodedRec is the text the recorded value's encoded runs decode to
	// (b64Text), "" when none.
	encodedRec string
	// unparsed says what of its drift the declared readings cannot read
	// (unparsed).
	unparsed string
	// wholes are the value whole, recorded, in each declared form.
	wholes map[string]bool
	// The value's instants (instants.go, readInstants): instants are those
	// of its forms that changed; recKeys and liveKeys the moments its forms
	// hold, recorded and live, and recMoments and liveMoments those moments
	// in each spelling, in the order they stand.
	instants                []driftedInstant
	recKeys, liveKeys       map[instantKey]bool
	recMoments, liveMoments []moment
}

// DriftCache keeps each value's drift for one test of the learn step: the
// earlier places of a test are read once, not once per request. It also says
// whose tests they are (ClientTests), which the classifier reads with it, and
// the zone the application writes a wall clock of no zone in. One test's
// caches share what they keep (driftState); each call reads it in the zone
// of the process that made the call where keploy read it (InZone).
type DriftCache struct {
	*driftState
	// clientTests: the test is one of keploy mock's client tests
	// (ClientTests), set as the tracker makes the cache and never after.
	clientTests bool
	// zone is the zone the application writes a wall clock of no zone in,
	// beside UTC (Tracker.SetAppZone, or the call's own process: InZone),
	// nil for UTC alone: a client test's window reads such a wall clock in
	// those two only (Window.holdsRead), and so does the clock rule of one
	// its protocol types (instantClock).
	zone *time.Location
	// zoneName is zone's name, as a learning that depends on it names it
	// (withAppZone; models.ZoneNameOf, read once as the tracker sets it);
	// zoneNoName: keploy could not read the zone by name (Go's own reading
	// of a TZ it cannot read, or the agent's own zone where it could not
	// read the call's process's), so no learning names it.
	zoneName   string
	zoneNoName bool
	// appClock: the tracker knows which clock the application reads
	// (Tracker.SetAppClock), set as the tracker makes the cache and as the
	// agent sets the clock. false (a cache made otherwise, NewDriftCache)
	// where keploy cannot tell: a call then has no window, and no replay can
	// check a read of the clock (Tracker.LiveSpan), so an instant only a
	// call's window could show a read is refused at learn (noAppClockWhy),
	// never learned as a read that only a replay refuses.
	appClock bool
}

// driftState is what one test's DriftCaches keep, shared by each zone it
// is read in (DriftCache.InZone).
type driftState struct {
	mu     sync.Mutex
	m      map[driftKey]*valueDrift
	raw    map[string]string
	rawTok map[string][]map[string]bool
	// mockText is each mock as Classify reads it (RawOf: its text and its
	// answer's clock reads), by mock: the store's mocks are read once per
	// test, not once per request.
	mockText map[*models.Mock]Raw
	// readText is each mock so read with its answer read back (ReadRaw), by
	// mock: apart from mockText, which RawOf fills with a parked mock's
	// text without its answer.
	readText map[*models.Mock]Raw
	// spans are, by text, the instants it holds (instantSpans); blanked and
	// blankTok, by payload (rawKey), its texts and words with its answer's
	// clock reads blanked (searched, searchedTokens).
	spans    map[string][]instant
	blanked  map[string][2]string
	blankTok map[string][]map[string]bool
}

// InZone is dc read in z, the zone the process that made a call writes a
// wall clock of no zone in (ProcZone, as the agent read it from the
// process), sharing what dc keeps: a learning that depends on the
// application's zone names z's name where it has one. dc itself where z is
// not known (keploy could not read the process's zone): the agent's own,
// which no learning names (a process's zone keploy cannot read may be any).
func (dc *DriftCache) InZone(z ProcZone) *DriftCache {
	if dc == nil {
		return nil
	}
	if z.Loc == nil {
		if dc.zoneNoName {
			return dc
		}
		c := *dc
		c.zoneName, c.zoneNoName = "", true
		return &c
	}
	c := *dc
	c.zone, c.zoneName, c.zoneNoName = z.Loc, z.Name, z.Name == ""
	return &c
}

// ClientTests reports whether dc is of one of keploy mock's client tests
// (Tracker.SetClientTests): a test harness's (a Playwright or CLI test, any
// harness), which gives the application its inputs and hands keploy no
// request of its own. A value the harness gave the application may change
// between the recording and record's full round, and move by about F's lead
// (a slot a minute on), and no earlier place of keploy's holds it. So there
// the lead shows no read of the application's clock, and a changed instant
// of a value (in any declared layout, or a leaf its protocol types as one)
// is learned as noise, or as an echo, only where it lies, in both runs,
// within its call's window on the application's clock as the agent's clock
// hooks report it (Window): a read of that clock for the call. Otherwise it
// is refused (ClassifyIn), and the reason says so (given.go). A
// NewDriftCache is no client test's: keploy test's, whose test case's
// request the learn step reads (Earlier.Given).
func (dc *DriftCache) ClientTests() bool { return dc != nil && dc.clientTests }

// Zone is the zone, beside UTC, in which a client test's window, and the
// clock rule of a wall clock its protocol types, read a wall clock of no
// zone (Tracker.SetAppZone): nil for UTC alone.
func (dc *DriftCache) Zone() *time.Location {
	if dc == nil {
		return nil
	}
	return dc.zone
}

// ClockReads is ClockReads of the cache's test: none in one of keploy mock's
// client tests (ClientTests), where the lead shows no read of the
// application's clock, so a value a test harness gave the application that
// the lead moved carries no clock evidence onto the variable it binds
// (models.MockVariable.Clock): a replay never lets two such variables
// share one live value (models.ReadInOneSecond).
func (dc *DriftCache) ClockReads(recorded, live string) []models.ClockRead {
	if dc.ClientTests() {
		return nil
	}
	return ClockReads(recorded, live)
}

// InstantClockRead is InstantClockRead of the cache's test: in one of keploy
// mock's client tests (ClientTests) the lead shows no read of the
// application's clock, so only w can; a wall clock of no zone is read in the
// cache's zone too (Zone).
func (dc *DriftCache) InstantClockRead(rec, live Leaf, w *Window) bool {
	return instantClockRead(rec, live, w, !dc.ClientTests(), dc.Zone())
}

// NewDriftCache makes an empty cache.
func NewDriftCache() *DriftCache {
	return &DriftCache{driftState: &driftState{m: map[driftKey]*valueDrift{}, raw: map[string]string{}, rawTok: map[string][]map[string]bool{}, mockText: map[*models.Mock]Raw{}, readText: map[*models.Mock]Raw{}, spans: map[string][]instant{}}}
}

// ReadRaw is RawOf(m, served) with m's answer read back first when the agent
// parked it on disk (models.Mock.HasSpilledResponse), m's text read once for
// the test (a nil cache reads it now). RawOf reads a parked mock as it is,
// without its answer, so a value its answer held reads as appearing nowhere.
// err is non-nil when the answer cannot be read back: what it held cannot be
// told, and nothing may be learned on the strength of its absence.
func (dc *DriftCache) ReadRaw(m *models.Mock, served bool) (Raw, error) {
	if dc == nil || !m.HasSpilledResponse() {
		if dc == nil {
			return ReadRaw(m, served)
		}
		return dc.RawOf(m, served), nil
	}
	dc.mu.Lock()
	r, ok := dc.readText[m]
	dc.mu.Unlock()
	if ok {
		r.served = served
		return r, nil
	}
	r, err := ReadRaw(m, served)
	if err != nil {
		return r, err
	}
	dc.mu.Lock()
	if dc.readText == nil {
		dc.readText = map[*models.Mock]Raw{}
	}
	dc.readText[m] = r
	dc.mu.Unlock()
	return r, nil
}

// ReadRaw is RawOf with m's answer read back first when the agent parked it
// on disk, on a copy (the pooled mock keeps it parked); err is why it cannot
// be read back.
func ReadRaw(m *models.Mock, served bool) (Raw, error) {
	if !m.HasSpilledResponse() {
		return RawOf(m, served), nil
	}
	full := m.ShallowCopy()
	if err := full.HydrateResponse(); err != nil {
		return Raw{mock: m.Name, at: m.Spec.ReqTimestampMock, answered: m.Spec.ResTimestampMock, served: served, call: callOf(m)}, err
	}
	return RawOf(full, served), nil
}

// RawOf is RawOf(m, served), m's text read once for the test (a nil cache
// reads it now).
func (dc *DriftCache) RawOf(m *models.Mock, served bool) Raw {
	if dc == nil {
		return RawOf(m, served)
	}
	dc.mu.Lock()
	r, ok := dc.mockText[m]
	dc.mu.Unlock()
	if ok {
		r.served = served
		return r
	}
	r = RawOf(m, served)
	dc.mu.Lock()
	if dc.mockText == nil {
		dc.mockText = map[*models.Mock]Raw{}
	}
	dc.mockText[m] = r
	dc.mu.Unlock()
	return r
}

// rawTokens are every word of a payload's text in every declared form, by
// reading.
func (dc *DriftCache) rawTokens(text string) []map[string]bool {
	dc.mu.Lock()
	t, ok := dc.rawTok[text]
	dc.mu.Unlock()
	if ok {
		return t
	}
	t = tokensOfForms(textForms(text))
	dc.mu.Lock()
	dc.rawTok[text] = t
	dc.mu.Unlock()
	return t
}

// rawExtra is what a payload's text is searched in besides itself: its
// other declared forms (textForms: decoded, case-folded, and the text its
// encoded runs decode to), one per line.
func (dc *DriftCache) rawExtra(text string) string {
	dc.mu.Lock()
	t, ok := dc.raw[text]
	dc.mu.Unlock()
	if ok {
		return t
	}
	t = strings.Join(textForms(text)[1:], "\n")
	dc.mu.Lock()
	dc.raw[text] = t
	dc.mu.Unlock()
	return t
}

// driftKey is a drifted value, with the window of the call it is read in
// (Window.key) and the call's recorded instant (UnixNano, 0 for none).
type driftKey struct {
	run
	w    [4]int64
	call int64
	// zone is the zone the clock reads were read in (DriftCache.zone): a
	// test's caches share what they keep across the zones of its calls'
	// processes (InZone), and a value one zone reads as a clock read
	// another may not. The key holds it, so it is never another's.
	zone *time.Location
}

// of is what the drifted value (rec, live) changed, as an earlier place
// holds it: its clock reads read by the lead alone, bounded by no call.
func (dc *DriftCache) of(rec, live string) *valueDrift { return dc.ofIn(rec, live, nil, time.Time{}) }

// ofIn is what the drifted value (rec, live) changed, its clock reads read
// by the lead and in w, the window of the call that carries it (nil: by the
// lead alone), the call recorded at call (zero: not known), which bounds
// what the lead moved (clockAt).
func (dc *DriftCache) ofIn(rec, live string, w *Window, call time.Time) *valueDrift {
	var at int64
	if !call.IsZero() {
		at = call.UnixNano()
	}
	k := driftKey{run{rec, live}, w.key(), at, dc.zone}
	dc.mu.Lock()
	d, ok := dc.m[k]
	dc.mu.Unlock()
	if ok {
		return d
	}
	lead := !dc.clientTests
	d = &valueDrift{rec: rec, live: live, clock: clockAt(rec, live, w, lead, call, dc.zone)}
	d.decRec, d.decLive = unescaped(rec), unescaped(live)
	d.decoded = d.decRec != rec || d.decLive != live
	// A value with escapes is read raw and decoded: a '%' or '\' may be
	// literal ("%<ref>%" of a LIKE, a Windows path), and a decode that
	// mangles it must not hide the word it stands before.
	merge := func(c clockSet) {
		for w := range c.rec {
			d.clock.rec[w] = true
		}
		for w := range c.live {
			d.clock.live[w] = true
		}
	}
	var dclock clockSet
	if d.decoded {
		dclock = clockAt(d.decRec, d.decLive, w, lead, call, dc.zone)
		merge(dclock)
	}
	forms, encoded := formsOf(rec, live, d.decRec, d.decLive, d.clock)
	d.encodedRec = encoded[0]
	// The clock reads of every other form (the first is the decoded value,
	// read above).
	for _, f := range forms[1:] {
		merge(clockAt(f[0], f[1], w, lead, call, dc.zone))
	}
	if d.decoded {
		forms = append([][2]string{{rec, live}}, forms...)
		d.clock.whole = d.clock.whole || dclock.whole
		// Read raw, a clock read's escapes are no words of their own: an
		// escape's remnant glued to a clock word is one ("3A41Z" of
		// "%3A41Z", "n2026" of "\n2026").
		escapeClock(rec, d.clock.rec)
		escapeClock(live, d.clock.live)
	}
	// A case-folded form reads the clock's words case-folded too.
	for w := range d.clock.rec {
		d.clock.rec[strings.ToLower(w)] = true
	}
	for w := range d.clock.live {
		d.clock.live[strings.ToLower(w)] = true
	}
	d.forms = forms
	// What the readings cannot read whole, in every form but the raw one
	// of a value with escapes (whose '%' and '\' are the escapes).
	for i, f := range forms {
		if d.decoded && i == 0 {
			continue
		}
		if why := unparsed(f[0], f[1], d.clock); why != "" {
			d.unparsed = why
			break
		}
	}
	// The value whole in each form (each line of its decoded text) that
	// drifted: the same value whole in another place, in any reading, is
	// followed. A line both sides hold (a JWT's header) did not drift.
	liveLines := map[string]bool{}
	for _, f := range forms {
		for _, w := range strings.Split(f[1], "\n") {
			liveLines[w] = true
		}
	}
	for _, f := range forms {
		for _, w := range strings.Split(f[0], "\n") {
			if w != "" && !liveLines[w] {
				if d.wholes == nil {
					d.wholes = map[string]bool{}
				}
				d.wholes[w] = true
			}
		}
	}
	for _, word := range readings {
		lost, gained := map[string]bool{}, map[string]bool{}
		recTok, liveTok := map[string]bool{}, map[string]bool{}
		for _, f := range forms {
			r, l := tokensBy(f[0], word), tokensBy(f[1], word)
			for t := range r {
				recTok[t] = true
				if !l[t] {
					lost[t] = true
				}
			}
			for t := range l {
				liveTok[t] = true
				if !r[t] {
					gained[t] = true
				}
			}
		}
		d.lost, d.gained = append(d.lost, lost), append(d.gained, gained)
		d.recTok, d.liveTok = append(d.recTok, recTok), append(d.liveTok, liveTok)
		movedRec, movedLive := map[string]bool{}, map[string]bool{}
		for _, f := range forms {
			mr, ml := movedWords(wordsBy(f[0], word), wordsBy(f[1], word))
			for _, t := range mr {
				movedRec[t] = true
			}
			for _, t := range ml {
				movedLive[t] = true
			}
		}
		d.movedRec, d.movedLive = append(d.movedRec, movedRec), append(d.movedLive, movedLive)
	}
	// Pieces of the value in every form.
	for _, f := range forms {
		d.pieces = append(d.pieces, pieces(f[0], f[1])...)
	}
	// Its instants (instants.go).
	d.readInstants()
	dc.mu.Lock()
	dc.m[k] = d
	dc.mu.Unlock()
	return d
}

// clockWord reports whether token t, which value d lost (recorded) or
// gained, changed only with F's clock: every ASCII word of it the other side
// of d does not hold is a clock word of one of cs (a token whose words the
// other side all holds is only regrouped: nothing new).
func clockWord(t string, recorded bool, d *valueDrift, cs ...clockSet) bool {
	other := map[string]bool{}
	for _, f := range d.forms {
		side := f[1]
		if !recorded {
			side = f[0]
		}
		for w := range tokensBy(side, isWord) {
			other[w] = true
		}
	}
	ws := tokensBy(t, isWord)
	if len(ws) == 0 {
		// No word of a clock read: never one.
		return false
	}
	for w := range ws {
		if !other[w] && !clockSide(w, recorded, cs) {
			return false
		}
	}
	return true
}

// unescaped is s with its percent-escapes decoded and its backslash escapes
// read: "\uXXXX" (and an escaped surrogate pair) as the character it names,
// "\\" as "\", and the others of a JSON string ("\n", "\t", "\"", "\/") as
// a space, a separator: a value right after one ("uploads%2F<id>",
// "\n<id>") is a word of its own there.
func unescaped(s string) string {
	// Each escape on its own (a stray '%' leaves the others to decode), and
	// again while that changes anything, a few times at most (a value
	// encoded inside a value encoded again: "%252F", "\\\\n").
	for i := 0; i < 4; i++ {
		d := percentEscape.ReplaceAllStringFunc(s, func(m string) string {
			b, err := hex.DecodeString(m[1:])
			if err != nil {
				return m
			}
			return string(b)
		})
		d = escapedSurrogates.ReplaceAllStringFunc(d, func(m string) string {
			// Four hex digits each (escapedSurrogates): 16 bits.
			hi, _ := strconv.ParseUint(m[2:6], 16, 16)
			lo, _ := strconv.ParseUint(m[8:12], 16, 16)
			return string(utf16.DecodeRune(rune(hi), rune(lo)))
		})
		d = backslashEscape.ReplaceAllStringFunc(d, func(m string) string {
			switch {
			case m == `\\`:
				return `\`
			case m[1] == 'u':
				// The character it names (a JSON writer's escape of what
				// is beyond ASCII).
				// Four hex digits (backslashEscape): 16 bits.
				if r, err := strconv.ParseUint(m[2:], 16, 16); err == nil {
					return string(rune(r))
				}
			}
			return " "
		})

		if d == s {
			break
		}
		s = d
	}
	return s
}

var (
	// escapedSurrogates are a high and a low surrogate escaped as two
	// \uXXXX: one character beyond the basic plane (an emoji).
	escapedSurrogates = regexp.MustCompile(`\\u[dD][89abAB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}`)
	percentEscape     = regexp.MustCompile(`%[0-9a-fA-F]{2}`)
	backslashEscape   = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|[nrtbf"\\/])`)
)

// sources reports whether the earlier place e (what F saw drift there) is
// where the drifted value v came from, whole or in part (whole: the earlier
// place held v's recorded value whole, the recorded value appeared there:
// design v14 §7.2 step 6; a whole repeated clock value is followed too, not
// learned as noise). A part counts where the two drifted together
// (co-drift), recorded and live alike, however short:
//
//   - in one reading of the values or of their percent- and
//     backslash-decoded forms, the two both lost a word and both gained a
//     word, wherever they sit and whatever else changed around them
//     ("<id>" of "<id>.json", of "doc:<id>", of "uploads%2F<id>"; "<trace>"
//     of "<trace>:<span1>" and "<trace>:<span2>"; a ref next to a fresh
//     base64 signature in two callback URLs);
//   - or a piece of one (a run or a changed word) is a token of the other
//     where the other drifted: its recorded text in the recorded value and
//     its live text in the live one, neither in the other side.
//
// What drifted only because F's clock runs FullRoundLead past the recording
// is no part: a word that is a part of a clock read that moved by exactly
// the lead, in either value (two clock reads of one second in other
// precisions or forms share their minute and second; two UUID v7s of one
// minute share their time group). Every other shared changed word is a value
// that flowed.
func sources(e, v *valueDrift) (found, whole bool) {
	if e.rec == v.rec {
		return true, true
	}
	// The same value whole in another reading (decoded, a base64 run's
	// text): it flowed, and cannot be bound there.
	for w := range v.wholes {
		if e.wholes[w] {
			return true, false
		}
	}
	// A whole clock read standing whole in the other's base64 text (a
	// JWT's iat): a whole repeated value, as in a payload.
	if e.clock.whole && v.b64Holds(e) || v.clock.whole && e.b64Holds(v) {
		return true, false
	}
	drifted := func(p run, rec, live string) bool {
		return ContainsToken(rec, p.rec) && ContainsToken(live, p.live) && !ContainsToken(live, p.rec) && !ContainsToken(rec, p.live)
	}
	// v drifted with e also where e held a moment of v's that changed both
	// ways, in any layout (instants.go, rule 1), or v's text whole, recorded
	// and live; its words co-drift below as any value's do. This only finds
	// more. A clock read the lead moved, or read in its call's window, in
	// either is no part, as its words are none below: two clock reads of one
	// moment drift alike whatever made them.
	if !v.clock.whole && !e.clock.whole {
		whole := run{v.rec, v.live}
		if e.holdsMomentOf(v) || drifted(whole, e.rec, e.live) || v.decoded && drifted(run{v.decRec, v.decLive}, e.rec, e.live) || e.decoded && drifted(whole, e.decRec, e.decLive) {
			return true, false
		}
	}
	for i := range readings {
		lost, gained := false, false
		for t := range v.lost[i] {
			if e.lost[i][t] && !clockWord(t, true, v, v.clock, e.clock) {
				lost = true
				break
			}
		}
		for t := range v.gained[i] {
			if e.gained[i][t] && !clockWord(t, false, v, v.clock, e.clock) {
				gained = true
				break
			}
		}
		if lost && gained {
			return true, false
		}
	}
	for _, p := range v.pieces {
		if !isClock(p, v.clock, e.clock) && (drifted(p, e.rec, e.live) || e.decoded && drifted(p, e.decRec, e.decLive)) {
			return true, false
		}
	}
	for _, p := range e.pieces {
		if !isClock(p, e.clock, v.clock) && (drifted(p, v.rec, v.live) || v.decoded && drifted(p, v.decRec, v.decLive)) {
			return true, false
		}
	}
	return false, false
}

// appeared is the earlier place of the test's recording the drifted value
// (recorded, now) came from: a leaf of an HTTP mock F served before (every
// one of them came earlier in the test) that F saw drift (sources), else a
// raw mock served before or recorded before at (of which F saw no live
// value).
func appeared(dc *DriftCache, recorded, now string, at time.Time, served []Earlier, raw []Raw) (Place, bool) {
	return appearedIn(dc, dc.of(recorded, now), at, served, raw, nil)
}

// appearedIn is appeared of the drifted value v, read in its call's window.
//
// Under the rule a copy follows its source (fol non-nil: record's full
// round, ClassifyFollowing), a raw mock F served unchanged (Raw.AsRecorded)
// whose payload holds a word of the recorded value (the value whole, or a
// word of what it lost), but not the value F saw, is no place the value
// came from: F sent and gave the application that payload byte for byte as
// recorded, and the value moved, while a copy moves with its source. The
// search goes on past it (fol.refuted names the first such place, which a
// value learned as noise names: follows.refute); the word rule then reads
// its words as the rule reads every word both runs gave (follows.excusedRaw,
// followed). A place F did not serve, or served changed, or whose payload
// holds the value F saw too, stays where the value came from. A leaf F
// served as recorded (Seen.Live) is no place either way: it did not drift.
func appearedIn(dc *DriftCache, v *valueDrift, at time.Time, served []Earlier, raw []Raw, fol *follows) (Place, bool) {
	recorded := v.rec
	// The place is where the value was sent whole, not where it was answered
	// or sent inside a longer value (a variable is bound where the
	// application sent it, whole): the test's own call that sent it first,
	// else the run's, else another's; then the same inside a longer value;
	// an answer that gave it only when no call sent it. Earliest within each.
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			// A place F saw hold its recorded value did not drift: the
			// value there is the same every run, and a value that drifted
			// here did not come from it.
			if s.Live == s.Leaf.Value {
				continue
			}
			var ok, whole bool
			if s.Leaf.Type == Instant {
				// An instant its protocol types has no words: what v drifted
				// with there is its moments (dc.instantSource).
				ok, whole = dc.instantSource(s, v), s.Leaf.Value == recorded
			} else {
				ok, whole = sources(dc.of(s.Leaf.Value, s.Live), v)
			}
			if !ok {
				continue
			}
			if r := placeRank(e, s, whole); r < rank {
				best, rank = seenPlace(e, s, whole), r
			}
			if rank == 0 {
				return best, true
			}
		}
	}
	if rank < 7 {
		return best, true
	}
	// The echo (instants.go): where an earlier place holds the moment F saw
	// in v (a pick from served data, by its clock), a place that holds the
	// moment v was recorded with is where it came from, before any lead or
	// window excuses a word of it. An exchange F served leaf by leaf first,
	// then a raw mock (below).
	echoed := dc.echoedIn(v, at, served, raw)
	if from, ok := dc.echoServed(echoed, recorded, served); ok {
		return from, true
	}
	// Of a mock with no live view, F cannot tell whether a place drifted:
	// the recorded value, or what of it drifted (a changed word, a piece),
	// counts as a token of its payload (a token stored in a row before it
	// went out in "Bearer <token>"), but for what drifted with F's clock. A
	// whole clock read the lead moved counts only where it stands whole
	// there (a payload holding it with more precision, a zone or a date
	// around it holds a clock read of its own form, which is noise). It errs
	// to refusing.
	//
	// Instants (instants.go): a payload is searched with a protocol-phase
	// answer's own clock reads blanked (rule 2), but where the payloads hold
	// the instant F saw too (echoedInstants). Every value is searched whole
	// and by its words and pieces, whatever it looks like: only a clock read
	// its call's window or the lead shows (clockIn), a provenance, is none.
	//
	// The words are searched in the order they stand in the value (inOrder),
	// never in a set's: the first one found names the place and the reason.
	var searched []string
	if !v.clock.whole {
		searched = append(searched, recorded)
	}
	for i := range readings {
		for _, t := range v.inOrder(v.lost[i], true) {
			if t != recorded && !clockWord(t, true, v, v.clock) {
				searched = append(searched, t)
			}
		}
	}
	for _, p := range v.pieces {
		if p.rec != recorded && !isClock(p, v.clock) {
			searched = append(searched, p.rec)
		}
	}
	keep := dc.stampsEchoed(v, served, raw)
	for _, x := range raw {
		if !x.earlierThan(at) {
			continue
		}
		atCall := x.at.Equal(at)
		if from, ok := dc.echoRaw(x, echoed); ok {
			from.AtCall, from.noTime = atCall, x.noTime
			from.startUp = x.swapHolds(x.text, raw, func(own string) bool { return dc.holdsRecorded(own, echoed) })
			return from, true
		}
		texts := dc.searched(x, keep)
		if v.clock.whole {
			for _, text := range texts {
				if containsWholeClock(text, recorded) {
					swap := x.swapHolds(text, raw, func(own string) bool { return containsWholeClock(own, recorded) })
					return Place{Mock: x.mock, Field: dc.fieldOf(x, texts, recorded), Held: recorded, AtCall: atCall, noTime: x.noTime, startUp: swap}, true
				}
				if v.decoded && containsWholeClock(text, v.decRec) {
					swap := x.swapHolds(text, raw, func(own string) bool { return containsWholeClock(own, v.decRec) })
					return Place{Mock: x.mock, Field: dc.fieldOf(x, texts, v.decRec), Held: v.decRec, AtCall: atCall, noTime: x.noTime, startUp: swap}, true
				}
			}
		}
		for _, w := range searched {
			if ContainsToken(texts[0], w) || ContainsToken(texts[1], w) {
				p := Place{Mock: x.mock, Field: dc.fieldOf(x, texts, w), Held: w, AtCall: atCall, noTime: x.noTime}
				p.startUp = x.swapHolds(texts[0], raw, func(own string) bool { return holdsToken(own, w) })
				// A word of a connection's start-up a swap can change is
				// never set aside as a source the value moved away from: F
				// may have given the application's connection another
				// connection's start-up, whose word the value then copied
				// (isStartUp, swapHolds). The value is refused there
				// (startUpWhy). A word every start-up carries alike reads
				// as any recording's.
				//
				// A payload F served as recorded is set aside whole: F gave
				// every word of it. One F gave its view of (ServedLive) is
				// set aside a word at a time, its fields read one by one
				// (liveAt), so the search goes on to the value's next word
				// there: a field F gave another value at that the value
				// drifted with stays a place.
				if !p.startUp && fol.refute(x, texts, keep, v, p) {
					if x.AsRecorded() {
						break
					}
					continue
				}
				return p, true
			}
		}
	}
	return Place{}, false
}

// echoServed is the place of an exchange F served leaf by leaf (or the test
// case's request) that holds, as recorded, in every form (a typed time of
// day as the echo reads it, seenHoldsRecorded), the moment a drifted value
// (recorded) was recorded with, by the rule the echo found it a copy by
// (echoedIn, holdsRecorded): the first by placeRank. ok is false when none
// does.
func (dc *DriftCache) echoServed(echoed []echoMatch, recorded string, served []Earlier) (Place, bool) {
	if len(echoed) == 0 {
		return Place{}, false
	}
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			if !dc.holdsRecorded(s.Leaf.Value, echoed) && !dc.holdsRecorded(dc.rawExtra(s.Leaf.Value), echoed) && !seenHoldsRecorded(s.Leaf, s.Leaf.Value, echoed) {
				continue
			}
			whole := s.Leaf.Value == recorded
			if r := placeRank(e, s, whole); r < rank {
				best, rank = seenPlace(e, s, whole), r
				best.Echo = true
			}
		}
	}
	return best, rank < 7
}

// echoRaw is the place of payload x that holds the recorded moment of one of
// echoed (echoServed): the field that does, else the payload, in every form.
// ok is false when x holds none.
func (dc *DriftCache) echoRaw(x Raw, echoed []echoMatch) (Place, bool) {
	if len(echoed) == 0 {
		return Place{}, false
	}
	for _, f := range x.fields {
		if !f.key && dc.holdsRecorded(x.text[f.start:f.end], echoed) {
			return Place{Mock: x.mock, Field: f.describe(), Echo: true}, true
		}
	}
	if dc.holdsRecorded(x.text, echoed) || dc.holdsRecorded(dc.rawExtra(x.text), echoed) {
		return Place{Mock: x.mock, Echo: true}, true
	}
	return Place{}, false
}

// heldAt names the earliest place that holds the moment F saw of one of
// echoed (holdsLive: an exchange F served leaf by leaf, as recorded or as F
// served it, a typed time of day there as the echo reads it, seenHoldsLive,
// then a raw mock served earlier or recorded before at): "in <mock> at
// <field>", "" when none does.
func (dc *DriftCache) heldAt(echoed []echoMatch, at time.Time, served []Earlier, raw []Raw) string {
	holds := func(text string) bool {
		return dc.holdsLive(text, echoed) || dc.holdsLive(dc.rawExtra(text), echoed)
	}
	for _, e := range served {
		for _, s := range e.Seen {
			// As echoedIn reads it: what F served, or gave the application
			// as its test case's request, not what the application itself
			// sent where it drifted.
			if holds(s.Leaf.Value) || seenHoldsLive(s.Leaf, s.Leaf.Value, echoed) || (s.Side == Response || e.Given || s.Live == s.Leaf.Value) && (holds(s.Live) || seenHoldsLive(s.Leaf, s.Live, echoed)) {
				return "in " + e.Mock + " at " + s.Leaf.Path
			}
		}
	}
	for _, x := range raw {
		if !x.earlierThan(at) {
			continue
		}
		note := x.atNote(at)
		for _, f := range x.fields {
			if !f.key && holds(x.text[f.start:f.end]) {
				return "in " + x.mock + " at " + f.describe() + note
			}
		}
		if holds(x.text) {
			return "in " + x.mock + "'s payload" + note
		}
	}
	return ""
}

// evidence is why the drifted value v is a variable of from (appearedIn), a
// place in a raw mock's payload or one the echo found, as a reason names it:
// what was found there and in which field, why no clock read of the
// application's accounts for it, and how to correct it. It names a part of
// the value by where it stands in it, never by its text: a value may be a
// credential.
func evidence(v *valueDrift, from Place) string {
	at := placeAt(from)
	var found, provenance string
	switch {
	case from.Echo:
		found, provenance = echoFound(at), echoProvenance
	case from.Held == v.rec || from.Held == v.decRec:
		found = "its recorded value stands whole in " + at
	default:
		found = "a part of its recorded value (" + partOf(v.rec, from.Held) + ") stands in " + at
	}
	if provenance == "" {
		provenance = notClock(v)
	}
	return found + "; " + provenance + ", so keploy follows it as a value from there. " + correction(from.Mock)
}

// startUpWhy is why a drifted value v whose recorded value, or a part of
// it, stands in a connection's recorded start-up where a swap of start-ups
// can change it (from: isStartUp, swapHolds) is refused: a word a start-up
// carries never varies, and the value that changed may be a copy of
// another connection's start-up.
func startUpWhy(v *valueDrift, from Place) string {
	at := placeAt(from)
	var found string
	switch {
	case from.Echo:
		found = echoFound(at)
	case from.Held == v.rec || from.Held == v.decRec:
		found = "its recorded value stands whole in " + at
	default:
		found = "a part of its recorded value (" + partOf(v.rec, from.Held) + ") stands in " + at
	}
	return found + ", a connection's recorded start-up. A word a start-up carries where connections differ (a backend pid, a secret key, a server parameter set per connection) never varies, and keploy gives each connection the application opens a recorded start-up by the order the connections open in, not by what each sent: a connection may get another connection's start-up, and a value that changed may be a copy of its word, so keploy cannot tell a copy from a varying value. If the field changes every run on its own, declare it noise in keploy.yml, and keploy stops comparing it"
}

// placeAt names where from stands, as a reason names it: "<mock> at
// <leaf or field>", or "<mock>'s payload", and that its mock was recorded at
// the call's own instant where it was (Place.AtCall).
func placeAt(from Place) string {
	note := ""
	switch {
	case from.noTime:
		note = noTimeNote
	case from.AtCall:
		note = atCallNote
	}
	switch {
	case from.Path != "":
		return from.Mock + " at " + from.Path + note
	case from.Field != "":
		return from.Mock + " at " + from.Field + note
	}
	return from.Mock + "'s payload" + note
}

// echoFound is what the echo found, at at (evidence); echoProvenance why
// that move shows no read of the clock.
func echoFound(at string) string {
	return "the moment it was recorded with stands in " + at + ", and the moment record's full round saw in it stands earlier in the test too"
}

const echoProvenance = "a pick from what the application was given, by its clock, moves as a read of the clock does, so its move shows no read of the clock"

// notClock says why no read of the application's clock accounts for v.
func notClock(v *valueDrift) string {
	if len(v.clock.reads) > 0 {
		return "a part of it is a read of the application's clock, but that part is not"
	}
	return "nothing shows the value is a read of the application's clock (it did not move by the lead that record's full round runs ahead of the recording, and no window of its call shows a read)"
}

// correction is how to correct a refusal of a value found in mock.
func correction(mock string) string {
	return "If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it; otherwise it flowed from " + mock + ", where keploy cannot follow it yet"
}

// partOf names where part stands in value, by its characters' places,
// never by its text: "its characters 15 to 26", or "a part of it in another
// form" (decoded, case-folded) where value holds it in none of its own.
func partOf(value, part string) string {
	i := strings.Index(value, part)
	if part == "" || i < 0 {
		return "a part of it in another form"
	}
	a := utf8.RuneCountInString(value[:i]) + 1
	return "its characters " + strconv.Itoa(a) + " to " + strconv.Itoa(a+utf8.RuneCountInString(part)-1)
}

// placeRank orders the places a value may have come from (appearedIn): the
// request that sent it whole first, the test's own call before the run's,
// the run's before another's; then the same inside a longer value; then an
// answer, or the test case's own request, the earlier first.
func placeRank(e Earlier, s Seen, whole bool) int {
	// The test case's own request is what the application was given, as an
	// answer is: a call that sent the value, where it can be bound, is
	// named before it.
	if s.Side != Request || e.Given {
		return 6
	}
	r := 2
	switch {
	case e.Own:
		r = 0
	case e.Run:
		r = 1
	}
	if !whole {
		r += 3
	}
	return r
}

// seenPlace is the place s of e is.
func seenPlace(e Earlier, s Seen, whole bool) Place {
	return Place{Mock: e.Mock, Side: s.Side, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
		Var: s.Place, Live: s.Live, Whole: whole, Own: e.Own, Run: e.Run, Given: e.Given, Name: s.Name}
}

// echoedIn are v's changed instants the echo takes for a copy of what the
// application was given (echoedInstants): a payload served earlier in the
// test or recorded before at (raw), in every form, and every value of an
// exchange F served earlier (served), on either side, as recorded and as F
// served it (an answer's), in every form. nil when none.
func (dc *DriftCache) echoedIn(v *valueDrift, at time.Time, served []Earlier, raw []Raw) []echoMatch {
	if len(v.instants) == 0 {
		return nil
	}
	return echoedInstants(v, dc.earlierMoments(at, served, raw), dc.recordedMoments(at, served, raw))
}

// earlierMoments are the moments the earlier places of a call recorded at
// at hold, as the echo reads them (echoedIn): every payload served earlier
// in the test or recorded before at (raw), in every form, as it stands, and
// every value of an exchange F served earlier (served), on either side, as
// recorded and as F served it, in every form, and the test case's own
// request, as recorded and as sent.
func (dc *DriftCache) earlierMoments(at time.Time, served []Earlier, raw []Raw) *heldMoments {
	var held heldMoments
	for _, x := range raw {
		if x.earlierThan(at) {
			dc.heldIn(&held, x.text, dc.rawExtra(x.text))
		}
	}
	for _, e := range served {
		for _, s := range e.Seen {
			dc.heldIn(&held, s.Leaf.Value, dc.rawExtra(s.Leaf.Value))
			heldSeen(&held, s.Leaf, s.Leaf.Value)
			// What F served (an answer's value, or a request's that did not
			// drift), and the test case's request as keploy test sent it
			// (Earlier.Given). What the application itself sent in F, where
			// it drifted, is its own output, whose co-drift with v sources
			// reads: two reads of the clock in one moment drift alike.
			if s.Side == Response || e.Given || s.Live == s.Leaf.Value {
				dc.heldIn(&held, s.Live, dc.rawExtra(s.Live))
				heldSeen(&held, s.Leaf, s.Live)
			}
		}
	}
	return &held
}

// recordedMoments are the moments the recordings of a call's earlier places
// hold, as recorded: every payload served earlier in the test or recorded
// before at (raw), and every value of an exchange F served earlier (served)
// as recorded, in every form, and the test case's own request as recorded.
func (dc *DriftCache) recordedMoments(at time.Time, served []Earlier, raw []Raw) *heldMoments {
	var held heldMoments
	for _, x := range raw {
		if x.earlierThan(at) {
			dc.heldIn(&held, x.text, dc.rawExtra(x.text))
		}
	}
	for _, e := range served {
		for _, s := range e.Seen {
			dc.heldIn(&held, s.Leaf.Value, dc.rawExtra(s.Leaf.Value))
			heldSeen(&held, s.Leaf, s.Leaf.Value)
		}
	}
	return &held
}

// Echoes are the answer leaves that hold r's recorded value: a scalar equal
// to it (WholeEcho, a place it can be answered at), and every other place
// keploy cannot answer it at: a string or a key of the answer's JSON objects
// (Keys) holding it as a token, in any declared form (rawExtra: decoded,
// case-folded, inside base64), an instant its protocol types (Instant) that
// the value holds, in any layout and form, and an unnamed leaf equal to it.
func Echoes(dc *DriftCache, r Leaf, resp []Leaf) []Leaf {
	if r.Value == "" {
		return nil
	}
	if dc == nil {
		dc = NewDriftCache()
	}
	var out []Leaf
	for _, a := range resp {
		switch {
		case WholeEcho(r, a), a.Unnamed && a.Value == r.Value:
			out = append(out, a)
		case a.Type == Instant:
			// An answer's instant its protocol types echoes the value
			// where the value holds that moment, in any layout and form:
			// it has no words to hold.
			if ti, ok := readTyped(a); ok {
				m := ti.moment(ti.res)
				if dc.holdsExact(r.Value, m) || dc.holdsExact(dc.rawExtra(r.Value), m) {
					out = append(out, a)
				}
			}
		case a.Type == String || a.Type == Keys:
			if ContainsToken(a.Value, r.Value) {
				out = append(out, a)
				continue
			}
			if ContainsToken(strings.ToLower(a.Value), strings.ToLower(r.Value)) {
				out = append(out, a)
				continue
			}
			if f := dc.rawExtra(a.Value); f != "" && (ContainsToken(f, r.Value) || ContainsToken(f, strings.ToLower(r.Value))) {
				out = append(out, a)
			}
		}
	}
	return out
}

// WholeEcho reports whether answer leaf a holds r's recorded value as its
// whole value, a string, a number or a boolean (an id sent as text may come
// back as a number): a place the value can be answered at.
func WholeEcho(r, a Leaf) bool {
	if a.Unnamed || r.Value == "" || a.Value != r.Value {
		return false
	}
	switch a.Type {
	case String, Number, Bool:
		return true
	}
	return false
}

// payloadKeys are the fields of a mock's spec that say where and when it was
// recorded, not what it carried: a value found in them did not flow from the
// mock.
var payloadKeys = map[string]bool{
	"metadata": true, "created": true, "reqtimestampmock": true, "restimestampmock": true,
	"reqbodynoise": true, "correlations": true, "variables": true, "timestamp": true, "async": true,
}

// RawOf is a mock as Classify reads it when no leaf names its values: its
// recorded request and answer, the payload only, as one text.
func RawOf(m *models.Mock, served bool) Raw {
	r := Raw{mock: m.Name, at: m.Spec.ReqTimestampMock, answered: m.Spec.ResTimestampMock, served: served, call: callOf(m)}
	b, err := json.Marshal(payloadSpec(m.Spec))
	if err != nil {
		return r
	}
	v, ok := DecodeJSON(b)
	if !ok {
		return r
	}
	r.read, r.startUp = true, isStartUp(m)
	// The payload's own text, not its JSON: a JSON string escapes what
	// separates a value from its neighbours ("\r\n" of a RESP frame), and a
	// value after an escape would be no token there.
	payload := dropKeys(v, 0)
	r.text, r.fields = textFields(payload)
	if r.startUp {
		for i, f := range r.fields {
			r.fields[i].conn = !f.key && connFields[f.path]
		}
	}
	if m.IsProtocolPhase() {
		r.answerClock = answerClock(payload, m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
	}
	r.stamps = answerStamps(r.text, r.fields, m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
	return r
}

// isStartUp reports whether m is a connection's recorded start-up: what the
// server answered a connection's opening with (a Postgres start-up: its
// protocol version, the server's version and parameters, the backend pid
// and secret key; the SSLRequest's answer before it). Record's full round
// gives each connection the application opens one of the recorded start-ups
// by the order the connections open in, not by what each connection sent,
// so two connections whose roles in the application differ between the
// recording and F (a pool's threads that open in either order) each get the
// other's. A word a start-up carries where the start-ups differ (its
// backend pid and secret key, swapHolds) is therefore never a source that
// can vary: the recorded value copied it from its connection's start-up,
// and a value that changed may be a copy of another connection's
// (startUpWhy). Only a PostgresV3 session mock is read so; the older
// Postgres kinds, which carry their start-up in packets, and other
// protocols' start-ups are read as any recording.
func isStartUp(m *models.Mock) bool {
	return m.Kind == models.PostgresV3 && m.Spec.PostgresV3 != nil && m.Spec.PostgresV3.Type == models.PostgresV3TypeSession
}

// connFields are the fields of a connection's recorded start-up (isStartUp)
// that are the connection's own, whatever the server and the other
// start-ups recorded: its backend pid and secret key (BackendKeyData), as
// textFields names them.
var connFields = map[string]bool{
	"postgresV3.session.backendProcessID": true,
	"postgresV3.session.backendSecretKey": true,
}

// callOf is the call m answers, as one recording of a call is given in place
// of another: its kind, and of an HTTP call its method, its authority and
// how many segments its path has (the operation the HTTP adapter pairs a
// request within). Another kind is read as one call: two of its recordings
// may each stand in for the other, which only refuses more.
func callOf(m *models.Mock) string {
	call := string(m.Kind)
	if r := m.Spec.HTTPReq; r != nil {
		host, path := r.URL, ""
		if u, err := url.Parse(r.URL); err == nil {
			host, path = u.Host, u.Path
		}
		call += " " + string(r.Method) + " " + host + " /" + strconv.Itoa(len(strings.Split(strings.TrimPrefix(path, "/"), "/")))
	}
	return call
}

// payloadSpec is spec without what says, below the spec's own fields
// (payloadKeys), where and when it was recorded: a Postgres query's
// invocation id (its statement's hash, connection, recorded instant and
// sequence; models.PostgresV3QuerySpec.InvocationID). Its date and time words
// are the recording's own clock, and a value found there did not flow from
// it: read as payload, a clock the app read late in F (past what the lead
// accounts for) was a value from that recording, refused as changed. spec is
// not changed: the copy is.
func payloadSpec(spec models.MockSpec) models.MockSpec {
	if pg := spec.PostgresV3; pg != nil && pg.Query != nil && pg.Query.InvocationID != "" {
		q := *pg.Query
		q.InvocationID = ""
		p := *pg
		p.Query = &q
		spec.PostgresV3 = &p
	}
	return spec
}

// textOf writes v's keys and scalars to sb, each on a line of its own, in a
// stable order.
func textOf(v interface{}, sb *strings.Builder) {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(k)
			sb.WriteByte('\n')
			textOf(t[k], sb)
		}
	case []interface{}:
		for _, child := range t {
			textOf(child, sb)
		}
	case string:
		sb.WriteString(t)
		sb.WriteByte('\n')
	case json.Number:
		sb.WriteString(t.String())
		sb.WriteByte('\n')
	case bool:
		sb.WriteString(strconv.FormatBool(t))
		sb.WriteByte('\n')
	}
}

// textFields is v's text as textOf writes it, with where each of its
// scalars and keys stands in it (rawField).
func textFields(v interface{}) (string, []rawField) {
	var sb strings.Builder
	var fields []rawField
	var walk func(v interface{}, path string)
	scalar := func(s, path string, key bool) {
		start := sb.Len()
		sb.WriteString(s)
		fields = append(fields, rawField{start: start, end: sb.Len(), path: path, key: key})
		sb.WriteByte('\n')
	}
	walk = func(v interface{}, path string) {
		switch t := v.(type) {
		case map[string]interface{}:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				scalar(k, p, true)
				walk(t[k], p)
			}
		case []interface{}:
			for i, child := range t {
				walk(child, path+"["+strconv.Itoa(i)+"]")
			}
		case string:
			scalar(t, path, false)
		case json.Number:
			scalar(t.String(), path, false)
		case bool:
			scalar(strconv.FormatBool(t), path, false)
		}
	}
	walk(v, "")
	return sb.String(), fields
}

// fieldOf is the field of x's text, as searched (texts: its text and its
// other forms, a stamp and an answer clock read blanked), that holds word w
// as a token, in its own text or in a declared form of it; "" when none
// does (w stands across two fields).
func (dc *DriftCache) fieldOf(x Raw, texts [2]string, w string) string {
	for _, f := range x.fields {
		if f.end > len(texts[0]) {
			break
		}
		own := texts[0][f.start:f.end]
		if ContainsToken(own, w) {
			return f.describe()
		}
	}
	for _, f := range x.fields {
		if f.end > len(texts[0]) {
			break
		}
		for _, form := range textForms(texts[0][f.start:f.end])[1:] {
			if ContainsToken(form, w) {
				return f.describe()
			}
		}
	}
	return ""
}

// holdsToken reports whether a field's text own holds word w as a token, in
// its own text or in a declared form of it (fieldOf).
func holdsToken(own, w string) bool {
	if ContainsToken(own, w) {
		return true
	}
	for _, form := range textForms(own)[1:] {
		if ContainsToken(form, w) {
			return true
		}
	}
	return false
}

// swapHolds reports whether x, a connection's recorded start-up, holds what
// in finds (a word, a whole clock, an echoed moment) in a field a swap of
// start-ups can change: the connection's own backend pid or secret key
// (rawField.conn), or a field another start-up of raw recorded otherwise
// or lacks (a server parameter set per connection), or in no one field.
// text is x's text as searched. A field every start-up recorded alike (the
// protocol and server versions, the server's parameters: 0, 3, 16, 4096,
// on, UTC, UTF8 on Postgres 16) is no such field: whichever start-up F gave
// a connection, it carried the same word there, so a value that changed
// while it did not is read as any recording's word. False for any other
// mock.
func (x Raw) swapHolds(text string, raw []Raw, in func(own string) bool) bool {
	if !x.startUp {
		return false
	}
	held := false
	for _, f := range x.fields {
		if f.end > len(text) || !in(text[f.start:f.end]) {
			continue
		}
		held = true
		if f.conn || x.differs(f, raw) {
			return true
		}
	}
	return !held
}

// differs reports whether a start-up of raw lacks x's field f, or recorded
// it otherwise: a swap of start-ups can change the word there. x itself,
// in raw too, never differs.
func (x Raw) differs(f rawField, raw []Raw) bool {
	own := x.text[f.start:f.end]
	for _, y := range raw {
		if !y.startUp {
			continue
		}
		same := false
		for _, g := range y.fields {
			if g.path == f.path && g.key == f.key {
				same = y.text[g.start:g.end] == own
				break
			}
		}
		if !same {
			return true
		}
	}
	return false
}

// specNames are the names a mock file gives the spec's fields whose JSON
// names differ (an HTTP mock's req and resp, a Mongo mock's requests and
// responses, a generic mock's buffers).
var specNames = map[string]string{
	"Req": "req", "Res": "resp", "http2Req": "req", "http2Resp": "resp",
	"MongoRequests": "requests", "MongoResponses": "responses",
	"RequestBin": "genericrequests", "ResponseBin": "genericresponses", "Message": "message", "Origin": "origin",
}

// describe is f as a reason names it: by the names the mock file gives it
// ("spec.resp.header.Date").
func (f rawField) describe() string {
	var b strings.Builder
	b.WriteString("spec")
	for _, seg := range strings.Split(f.path, ".") {
		name, index := seg, ""
		if i := strings.IndexByte(seg, '['); i >= 0 {
			name, index = seg[:i], seg[i:]
		}
		if n, ok := specNames[name]; ok {
			name = n
		}
		b.WriteString("." + name + index)
	}
	if f.key {
		return "the name of " + b.String()
	}
	return b.String()
}

// dropKeys is v without what payloadKeys names of the spec itself (depth
// 0) and the timestamps of its requests and answers (depth 1): never a key
// of the payload's own values (a jsonb document's "metadata" or "created").
func dropKeys(v interface{}, depth int) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			lk := strings.ToLower(k)
			if depth == 0 && payloadKeys[lk] || depth == 1 && lk == "timestamp" {
				continue
			}
			out[k] = dropKeys(child, depth+1)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, child := range t {
			out[i] = dropKeys(child, depth+1)
		}
		return out
	}
	return v
}

// commaFraction is a decimal comma's fraction after a time (a Python or
// Log4j stamp, "10:00:41,123 "): milli-, micro- or nanoseconds, then a
// zone, a space or the end.
var commaFraction = regexp.MustCompile(`^,(?:\d{3}){1,3}(?:[ Z+-]|$)`)

// containsWholeClock reports whether text holds clock read v as it stands:
// as a token that no longer clock read of the text strictly holds (one with
// more precision, a zone, or a date around it is a clock read of its own
// form). The same value whole is a whole repeated value, followed (a
// refusal, where it cannot be bound); "1760436041,42", the next column of a
// row, is still the second whole.
func containsWholeClock(text, v string) bool {
	if v == "" {
		return false
	}
	var spans [][]int
	for _, lay := range clockLayouts {
		spans = append(spans, lay.re.FindAllStringIndex(text, -1)...)
	}
	for i := 0; i <= len(text)-len(v); {
		j := strings.Index(text[i:], v)
		if j < 0 {
			return false
		}
		at := i + j
		if tokenAt(text, v, at) {
			longer := false
			for _, sp := range spans {
				if sp[0] <= at && at+len(v) <= sp[1] && sp[1]-sp[0] > len(v) {
					// A comma after a datetime is as likely the next
					// field of a row as a decimal comma: it errs to
					// the whole value.
					if sp[0] == at && strings.HasPrefix(text[at+len(v):sp[1]], ",") && !commaFraction.MatchString(text[at+len(v):]) {
						continue
					}
					longer = true
					break
				}
			}
			if !longer {
				return true
			}
		}
		i = at + 1
	}
	return false
}

// escapeClock adds to set (clock words of a value) each raw word of s that
// is an escape's remnant glued to one ("3A41Z" of "%3A41Z", "n2026" of
// "\\n2026").
func escapeClock(s string, set map[string]bool) {
	hexDigit := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
	i := 0
	for _, seg := range segmentsBy(s, isWord) {
		w := seg.text
		if seg.word && i > 0 {
			switch {
			case s[i-1] == '%' && len(w) > 2 && hexDigit(w[0]) && hexDigit(w[1]) && set[w[2:]]:
				set[w] = true
			case s[i-1] == '\\' && len(w) > 1 && strings.IndexByte("nrtbf", w[0]) >= 0 && set[w[1:]]:
				set[w] = true
			}
		}
		i += len(w)
	}
}

// b64Holds reports whether the text d's recorded encoded runs decode to
// (a JWT's claims) holds the recorded value of o whole, as it stands
// (containsWholeClock).
func (d *valueDrift) b64Holds(o *valueDrift) bool {
	if d.encodedRec == "" {
		return false
	}
	return containsWholeClock(d.encodedRec, o.rec) || o.decoded && containsWholeClock(d.encodedRec, o.decRec)
}

// ReadBack is where a value of a recorded answer was written earlier in this
// run, by a write the read is linked to, with another value than its
// recording's: the request place, held whole, of the earliest exchange served
// before that marked a place a variable can hold Written into record
// (Seen.Record), whose recorded value is r's and whose live value differs
// from it. record is the record the read names, as the adapter names it (the
// record its filter or its identity names); "" for a read the adapter links
// to no record, which reads nothing back.
//
// That is where the value was first stored, so every read of it, and every
// later write that carries it on, follows one variable. A call that answers
// with what an earlier call wrote (a time, a code the application made and
// stored) is then answered with what this run wrote there: r's place is an
// answer place of the variable bound at that earlier place.
//
// What decides is the evidence alone: the write drifted, the adapter marked
// the place Written, and the read names the record it wrote. A value is read
// back whatever its length, script or type (the request place's type is not
// compared with r's: an id stored as a number is read back as text), and a
// value no linked write stored is never read back, however it looks.
//
// Every write into the record of r's recorded value is a value r may be, and
// the evidence does not say which write r reads: another field's, or a later
// write of one field (which carries the value on when it holds the same live
// value, and replaces it when it holds another). One at no place a variable
// can hold is one too: the store answers what it wrote all the same, though
// no variable is bound there. r is read back only when every such write holds
// one live value, other than the recorded one: a write that drifted to
// another, one that kept the recorded value, and one whose live value F
// could not read ("": it may hold anything) are each another value r may be,
// in whichever order they came, so r is read back from none, and its drift
// stays unexplained.
func ReadBack(r Leaf, record string, earlier []Earlier) (Place, bool) {
	if r.Unnamed || r.Value == "" || record == "" {
		return Place{}, false
	}
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	var first Place
	found := false
	// live is the live value every write of r's recorded value holds, once
	// one was seen (wrote).
	live, wrote := "", false
	for _, e := range served {
		if e.Given {
			continue
		}
		for _, s := range e.Seen {
			if s.Side != Request || !s.Written || s.Record != record || s.Leaf.Value != r.Value {
				continue
			}
			if !wrote {
				live, wrote = s.Live, true
			} else if s.Live != live {
				return Place{}, false
			}
			if s.Place == "" || found {
				continue
			}
			first = Place{Mock: e.Mock, Side: Request, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
				Var: s.Place, Live: s.Live, Whole: true, Own: e.Own, Run: e.Run, Name: s.Name}
			found = true
		}
	}
	// No write is at a place a variable can hold, the writes kept the
	// recorded value, or F could not read it: nothing this run wrote is r's.
	if !found || live == r.Value || live == "" {
		return Place{}, false
	}
	return first, true
}

// Named is e with each request place that holds one of vars (as its bind or
// a use) naming that variable (Seen.Name), the first by name when two hold
// one place. An echo is not named: it is its call's alone, and a later call
// that reads its value back binds it there as a variable of the test
// (ReadBack). e's own Seen is left as it was: the result holds a copy, with
// no name at any other place.
func Named(e Earlier, vars models.MockVariables) Earlier {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	at := map[string]string{}
	for _, name := range names {
		v := vars[name]
		if v.Echo {
			continue
		}
		if v.Bind != "" && at[v.Bind] == "" {
			at[v.Bind] = name
		}
		for _, u := range v.Use {
			if at[u] == "" {
				at[u] = name
			}
		}
	}
	e.Seen = append([]Seen(nil), e.Seen...)
	for i, s := range e.Seen {
		e.Seen[i].Name = ""
		if s.Side == Request && s.Place != "" {
			e.Seen[i].Name = at[s.Place]
		}
	}
	return e
}
