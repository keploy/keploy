package models

import (
	"errors"
	"fmt"
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
// the BodyBytes of its body the upstream sent.
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
	// timeout only: a truncate always sends the head, an eof never does
	// (an eof after the head is a truncate).
	AfterHead bool `json:"after_head,omitempty" yaml:"after_head,omitempty" bson:"after_head,omitempty"`
	// BodyBytes is how many bytes of the body the upstream sent before the
	// fault, once it had sent the head.
	BodyBytes int64 `json:"body_bytes,omitempty" yaml:"body_bytes,omitempty" bson:"body_bytes,omitempty"`
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
	// connection until the application closes it or the run ends.
	FaultTimeout FaultKind = "timeout"
	// FaultTruncate: the upstream sent the head, with the Content-Length it
	// sent, and BodyBytes of the body, then closed the connection.
	FaultTruncate FaultKind = "truncate"
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
		if f.AfterHead || f.BodyBytes != 0 {
			return fmt.Errorf("fault delay with after_head or body_bytes: a delay sends the whole answer")
		}
		return nil
	case "":
		return fmt.Errorf("fault with no kind: write one of reset, eof, timeout, truncate, delay")
	default:
		return fmt.Errorf("%w: fault kind %q is not one this keploy knows (reset, eof, timeout, truncate, delay); upgrade keploy", ErrFaultUnsupported, f.Kind)
	}
	if f.Delay != 0 {
		return fmt.Errorf("fault %s with a delay: only kind delay takes one", f.Kind)
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
	case FaultDelay:
		return "answer held " + f.Delay.String()
	}
	return fmt.Sprintf("fault %q", string(f.Kind))
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

// FaultServed says, for a replay summary, the fault the mock's answer ends in,
// as the application met it: "mock-12: upstream reset after 0 bytes, as
// recorded". It is "" for a mock whose answer ends in none.
func (m *Mock) FaultServed() string {
	if m == nil || m.Spec.Fault == nil {
		return ""
	}
	return m.Name + ": " + m.Spec.Fault.String() + ", as recorded"
}
