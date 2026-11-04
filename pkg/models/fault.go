package models

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// A Fault on a mock says how the call ended without its whole answer: the
// upstream reset the connection, closed it, sent part of the answer, or sent
// nothing before the application gave up. A recorded fault is what the
// upstream did at record, as the application saw it; replay gives the
// application the same fault for the same request.
//
// The answer's head, when the upstream sent one before the fault (truncate,
// or AfterHead), is the mock's response as recorded: its status line, its
// reason phrase and its headers, Content-Length as the upstream sent it, and
// the BodyBytes of its body the upstream sent. A stopped_reading fault's is a
// whole answer's instead, the fault keeping how the upstream framed the body
// (FaultStoppedReading).
//
// The format is closed: a kind this keploy does not know, or a field a kind
// does not take, is refused by Validate with the reason named, never served
// as some other fault or as a whole answer.
//
// On disk, on an HTTP mock's spec:
//
//	fault:
//	    kind: truncate
//	    body_bytes: 10
//	    observed: unexpected EOF
type Fault struct {
	// Kind is how the call ended.
	Kind FaultKind `json:"kind" yaml:"kind" bson:"kind"`
	// AfterHead: the upstream sent the answer's head (status line and
	// headers) and BodyBytes of its body before the fault. For reset and
	// timeout, and always for stopped_reading: a truncate always sends the
	// head, an eof never does (an eof after the head is a truncate).
	AfterHead bool `json:"after_head,omitempty" yaml:"after_head,omitempty" bson:"after_head,omitempty"`
	// BodyBytes is how many bytes of the body the upstream sent before the
	// fault, once it had sent the head.
	BodyBytes int64 `json:"body_bytes,omitempty" yaml:"body_bytes,omitempty" bson:"body_bytes,omitempty"`
	// ContentLength, for stopped_reading, is the Content-Length the upstream
	// sent the answer's head with, which replay sends it with: 0 for a head
	// with none. The mock's response carries the length of what the
	// application read instead, for a reader that drops the fault.
	ContentLength int64 `json:"content_length,omitempty" yaml:"content_length,omitempty" bson:"content_length,omitempty"`
	// Chunked, for stopped_reading: the upstream sent the body chunked,
	// which replay sends it as. A head with neither this nor a
	// ContentLength left the body to the connection's close to end.
	Chunked bool `json:"chunked,omitempty" yaml:"chunked,omitempty" bson:"chunked,omitempty"`
	// AppEndedFirst, for stopped_reading and timeout: at record the
	// application had ended its side (its FIN, or a reset) before the
	// upstream's end reached it, and the call was judged by that end. A FIN
	// cannot say whether the application closed or only half-closed
	// (shutdown(SHUT_WR)) and read on, so at replay its FIN, before the
	// answer or after it, does not end the hold: only a reset or the run's
	// end does. A fault without it (written by hand, or by a keploy that did
	// not record it) ends the hold at the application's FIN, unless its
	// socket says that FIN was in before the answer was written.
	AppEndedFirst bool `json:"app_ended_first,omitempty" yaml:"app_ended_first,omitempty" bson:"app_ended_first,omitempty"`
	// Delay, for a delay, is how long the whole answer is held before it is
	// sent, bounded by the run. Only a delay takes one.
	Delay time.Duration `json:"delay,omitempty" yaml:"delay,omitempty" bson:"delay,omitempty"`
	// Observed is what the recorder saw, for a reader ("read: connection
	// reset by peer"). Replay does not read it.
	Observed string `json:"observed,omitempty" yaml:"observed,omitempty" bson:"observed,omitempty"`
}

// FaultKind is how a call ended without its whole answer.
type FaultKind string

const (
	// FaultReset: the upstream reset the connection; the application read
	// ECONNRESET. Before any byte of an answer, or after the head and
	// BodyBytes of the body (AfterHead).
	FaultReset FaultKind = "reset"
	// FaultEOF: the upstream closed the connection (a FIN) before any byte
	// of an answer; the application read the end of the stream.
	FaultEOF FaultKind = "eof"
	// FaultTimeout: no answer came, or no more of one after the head and
	// BodyBytes of the body (AfterHead), before the application closed the
	// connection: its own client gave up. It is the application's close that
	// ends it, never a clock: replay answers nothing more and holds the
	// connection until the application closes it or the run ends; one
	// recorded with AppEndedFirst, until a reset or the run's end.
	FaultTimeout FaultKind = "timeout"
	// FaultTruncate: the upstream sent the head, with the Content-Length it
	// sent, and BodyBytes of the body, then closed the connection.
	FaultTruncate FaultKind = "truncate"
	// FaultStoppedReading: the application read the answer's head and
	// BodyBytes of its body, then stopped reading and closed the connection,
	// whether the upstream sent more after that or nothing: what it read is
	// all it had, its end never. Its mock's response is a whole answer's,
	// the body as far as the application read it, decoded, with a
	// Content-Length of its size and no Transfer-Encoding, so a reader of
	// the mock that drops the fault serves it whole. The fault keeps how
	// the upstream framed the body (ContentLength, Chunked). Replay sends
	// that head framed so, and the body without its end: under the
	// upstream's Content-Length, which it does not reach; as one chunk with
	// no last chunk; or, framed by the close, as it is; a compressed body
	// flushed, not ended. Then it holds the connection until the
	// application closes it or the run ends (with AppEndedFirst, as a
	// recording writes it, until a reset or the run's end), never ended by
	// a clock. AfterHead always.
	FaultStoppedReading FaultKind = "stopped_reading"
	// FaultDelay: the whole answer, sent once Delay has passed. A recording
	// holds none (a record makes no verdict on wall-clock time); a delay is
	// written by hand or by a tool, and replay bounds it by the run.
	FaultDelay FaultKind = "delay"
)

// ErrFaultUnsupported marks a fault this keploy cannot enact: a kind or a
// field of a newer format, or a fault on a call of a protocol it enacts no
// faults for.
var ErrFaultUnsupported = errors.New("unsupported fault")

// SendsHead reports whether the fault comes after the answer's head: the
// head is sent before it.
func (f *Fault) SendsHead() bool {
	return f.Kind == FaultTruncate || f.AfterHead
}

// Validate checks the fault is one this keploy can enact, spelled the one way
// the format has for it. The error names what is wrong.
func (f *Fault) Validate() error {
	if f == nil {
		return nil
	}
	switch f.Kind {
	case FaultReset, FaultTimeout:
	case FaultStoppedReading:
		if !f.AfterHead {
			return fmt.Errorf("fault stopped_reading without after_head: an application that closed before the head came had no answer; write kind timeout")
		}
		if f.ContentLength < 0 {
			return fmt.Errorf("fault stopped_reading with content_length %d: a length sent is not negative; leave it out for a head that sent none", f.ContentLength)
		}
		if f.ContentLength > 0 && f.Chunked {
			return fmt.Errorf("fault stopped_reading with content_length and chunked: a head frames its body one way; keep the one the upstream sent")
		}
	case FaultEOF:
		if f.AfterHead {
			return fmt.Errorf("fault eof with after_head: an upstream that closed after the head truncated the answer; write kind truncate")
		}
	case FaultTruncate:
		if f.AfterHead {
			return fmt.Errorf("fault truncate with after_head: a truncate always sends the head; leave after_head out")
		}
	case FaultDelay:
		if f.Delay <= 0 {
			return fmt.Errorf("fault delay of %s: a delay holds the answer for a positive duration", f.Delay)
		}
		if f.AfterHead || f.BodyBytes != 0 || f.ContentLength != 0 || f.Chunked || f.AppEndedFirst {
			return fmt.Errorf("fault delay with after_head, body_bytes, content_length, chunked or app_ended_first: a delay sends the whole answer")
		}
		return nil
	case "":
		return fmt.Errorf("fault with no kind: write one of reset, eof, timeout, truncate, stopped_reading, delay")
	default:
		return fmt.Errorf("%w: fault kind %q is not one this keploy knows (reset, eof, timeout, truncate, stopped_reading, delay); upgrade keploy", ErrFaultUnsupported, f.Kind)
	}
	if f.Delay != 0 {
		return fmt.Errorf("fault %s with a delay: only kind delay takes one", f.Kind)
	}
	if f.Kind != FaultStoppedReading && (f.ContentLength != 0 || f.Chunked) {
		return fmt.Errorf("fault %s with content_length or chunked: only kind stopped_reading takes them; its head is the mock's response as the upstream sent it", f.Kind)
	}
	if f.AppEndedFirst && f.Kind != FaultStoppedReading && f.Kind != FaultTimeout {
		return fmt.Errorf("fault %s with app_ended_first: only kinds stopped_reading and timeout take it; the upstream's end judged this call", f.Kind)
	}
	if f.BodyBytes < 0 {
		return fmt.Errorf("fault %s with body_bytes %d: a count of bytes sent is not negative", f.Kind, f.BodyBytes)
	}
	if f.BodyBytes > 0 && !f.SendsHead() {
		return fmt.Errorf("fault %s with body_bytes %d and no head: body bytes come after the head; set after_head", f.Kind, f.BodyBytes)
	}
	return nil
}

// String says what the application gets, for output and summaries:
// "upstream reset after 0 bytes", "upstream sent the head and 10 body bytes,
// then closed".
func (f *Fault) String() string {
	if f == nil {
		return ""
	}
	sent := "0 bytes"
	if f.SendsHead() {
		sent = fmt.Sprintf("the head and %d body bytes", f.BodyBytes)
	}
	switch f.Kind {
	case FaultReset:
		return "upstream reset after " + sent
	case FaultEOF:
		return "upstream closed with no answer"
	case FaultTimeout:
		if f.AfterHead {
			return "upstream sent " + sent + ", then nothing until the application closed"
		}
		return "no answer until the application closed"
	case FaultTruncate:
		return "upstream sent " + sent + ", then closed"
	case FaultStoppedReading:
		return "the application read " + sent + ", then stopped reading and closed"
	case FaultDelay:
		return "answer held " + f.Delay.String()
	}
	return fmt.Sprintf("fault %q", string(f.Kind))
}

// HTTPFault is the kind an HTTP mock whose answer ends in a fault is written
// under, on disk only: read back, it is an HTTP mock (HTTP) with its
// Spec.Fault. A keploy that knows no faults (v3.6.204 and older) reads
// `kind: Http`, ignores the `fault:` block, and serves the head and the
// partial body as a whole answer; the same document under a kind it does not
// know is one it skips, saying it was recorded by a newer keploy, so the
// call misses. TestFaultMockKindHasNoHyphen keeps it out of the "-" kinds an
// open-source keploy skips without a word.
const HTTPFault Kind = "HttpFault"

// DiskKind is the kind m is written under: HTTPFault for an HTTP mock with a
// fault, m.Kind otherwise.
func (m *Mock) DiskKind() Kind {
	if m.Kind == HTTP && m.Spec.Fault != nil {
		return HTTPFault
	}
	return m.Kind
}

// FaultKindMismatch is why a document's kind and its fault do not go
// together, "" when they do: a fault on a mock written as HTTP (an older
// keploy would serve its head as a whole answer), or an HTTPFault document
// with no fault.
func FaultKindMismatch(docKind Kind, f *Fault) string {
	switch {
	case docKind == HTTP && f != nil:
		return "an HTTP mock with a fault is written as kind " + string(HTTPFault) + ", not " + string(HTTP) + ", so that a keploy that knows no faults skips it instead of serving its head as a whole answer; change its kind, or re-record it"
	case docKind == HTTPFault && f == nil:
		return "a mock of kind " + string(HTTPFault) + " has no fault: give it its fault, or make it kind " + string(HTTP) + " if its answer was whole"
	}
	return ""
}

// ValidateFault checks the mock's fault, if it has one: a fault this keploy
// can enact (Fault.Validate), on a call of a protocol it enacts faults for.
// Only HTTP's are enacted; a fault on any other call fails closed, never
// served as its recorded answer.
func (m *Mock) ValidateFault() error {
	if m == nil || m.Spec.Fault == nil {
		return nil
	}
	if m.Kind != HTTP {
		return fmt.Errorf("%w: mock %q is a %s call with a fault (%s); this keploy enacts faults on HTTP calls only", ErrFaultUnsupported, m.Name, m.Kind, m.Spec.Fault)
	}
	if err := m.Spec.Fault.Validate(); err != nil {
		return fmt.Errorf("mock %q: %w", m.Name, err)
	}
	return nil
}

// FaultOf is the fault m's answer ends in (Spec.Fault), nil for none. A mock
// whose payload is on disk (LeavePayloadOnDisk) has its Spec.Fault back only
// once the payload is read back; it keeps a copy for this until then.
func (m *Mock) FaultOf() *Fault {
	if m == nil {
		return nil
	}
	if m.onDisk != nil {
		return m.onDisk.fault
	}
	return m.Spec.Fault
}

// FaultServed says, for a replay summary, the fault the mock's answer ends in,
// as the application met it: "mock-12: upstream reset after 0 bytes, as
// recorded". It is "" for a mock whose answer ends in none.
func (m *Mock) FaultServed() string {
	f := m.FaultOf()
	if f == nil {
		return ""
	}
	return m.Name + ": " + f.String() + ", as recorded"
}

// FaultsServed is what a replay summary says of the faults a replay served:
// served(name) for each mock it consumed, the line of that mock's fault
// (Mock.FaultServed), "" for a mock whose answer ends in none, which is not
// named. The lines are in the order the mocks were first served
// (MockState.Timestamp), by name between mocks first served at one time.
func FaultsServed(consumed []MockState, served func(name string) string) []string {
	type line struct {
		state MockState
		text  string
	}
	var lines []line
	for _, st := range consumed {
		if text := served(st.Name); text != "" {
			lines = append(lines, line{st, text})
		}
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].state.Timestamp != lines[j].state.Timestamp {
			return lines[i].state.Timestamp < lines[j].state.Timestamp
		}
		return lines[i].state.Name < lines[j].state.Name
	})
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}
