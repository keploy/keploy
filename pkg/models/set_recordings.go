package models

import (
	"context"
	"encoding/json"
	"strings"
)

// What a test set's recordings hold of the shape of a call outside every
// test (UnmatchedCall.SetRecorded), read from every recording of the set as
// the replay loaded it: each test's window and the session's, spent or not.
// A miss's match phase speaks only of the pool its matcher read, one test's
// window and the session's less what was spent, so a call whose shape the set
// recorded only in another test's window (a timer of the application's,
// recorded as it fired inside one test, made between two others on replay)
// found none there although the set has some.

// RecordedCall is a recording of a test set as SetShapeRecordings reads it:
// its kind and the shape of its call (Shape), Read false where keploy reads no
// shape from it.
type RecordedCall struct {
	Kind  Kind
	Shape string
	Read  bool
}

// RecordedCallOf is m as SetShapeRecordings reads it. Keploy reads the shape
// of an HTTP mock's call (its method and its URL's target shape, with each
// path place the mock declares for a test variable written {}) and of a
// MongoDB mock's OP_MSG command (the command and the collection it runs on, as
// a miss's summary names them, in their shape); of a mock whose payload is on
// disk, from its payload read back. A mock of any other kind, one whose
// payload cannot be read back, and a MongoDB mock with no OP_MSG command it
// can read are not read.
func RecordedCallOf(m *Mock) RecordedCall {
	if m == nil {
		return RecordedCall{}
	}
	r := RecordedCall{Kind: m.Kind}
	if m.Kind != HTTP && m.Kind != Mongo {
		return r
	}
	if d := m.onDisk; d != nil {
		read, err := d.src.LoadPayload(context.Background(), d.at)
		if err != nil || read == nil {
			return r
		}
		c := *m
		c.onDisk = nil
		c.Spec = read.Spec
		m = &c
	}
	switch {
	case m.Kind == HTTP && m.Spec.HTTPReq != nil:
		shape := CallShape(string(HTTP), string(m.Spec.HTTPReq.Method)+" "+strings.TrimSpace(m.Spec.HTTPReq.URL))
		if shape != "" {
			r.Shape, r.Read = holePathPlaces(shape, DeclaredPathPlaces(m)), true
		}
	case m.Kind == Mongo:
		if s, ok := mongoRecordedSummary(m.Spec.MongoRequests); ok {
			r.Shape, r.Read = mongoShape(s), true
		}
	}
	return r
}

// mongoMsgSection opens a recorded OP_MSG body section: "{ SectionSingle
// msg: <extended JSON> }".
const mongoMsgSection = "{ SectionSingle msg: "

// mongoRecordedSummary is the call of a MongoDB mock's first request as its
// parser's miss summary names one: the command (the first key of its OP_MSG
// body section) and, when that key's value is a collection's name other than
// the $cmd pseudo-collection, " on <collection>". False for a request that is
// no OP_MSG, or whose body section does not read as a command.
func mongoRecordedSummary(reqs []MongoRequest) (string, bool) {
	if len(reqs) == 0 {
		return "", false
	}
	// A request that is no OP_MSG (or a nil one) is a nil msg.
	msg, _ := reqs[0].Message.(*MongoOpMessage)
	if msg == nil {
		return "", false
	}
	for _, sec := range msg.Sections {
		body, ok := strings.CutPrefix(sec, mongoMsgSection)
		if !ok {
			continue
		}
		// A token that is not there (a read error) is nil, which is
		// neither the document's "{" nor a name.
		dec := json.NewDecoder(strings.NewReader(body))
		if open, _ := dec.Token(); open != json.Delim('{') {
			return "", false
		}
		key, _ := dec.Token()
		cmd, _ := key.(string)
		if cmd == "" {
			return "", false
		}
		value, _ := dec.Token()
		if coll, _ := value.(string); coll != "" && coll != "$cmd" {
			return cmd + " on " + coll, true
		}
		return cmd, true
	}
	return "", false
}

// recordedKindOf is the kind of the mocks that record a call of protocol, as
// a miss names its protocol, for the protocols whose recordings keploy reads
// a shape from (RecordedCallOf): HTTP's and MongoDB's.
func recordedKindOf(protocol string) (Kind, bool) {
	switch {
	case strings.EqualFold(protocol, "HTTP"), strings.EqualFold(protocol, "HTTPS"):
		return HTTP, true
	case isMongoCall(protocol):
		return Mongo, true
	}
	return "", false
}

// SetShapeRecordings says what recorded, every recording of c's test set
// (RecordedCallOf), holds of c's shape: RecordedShape when one of c's kind has
// it (a path segment either side writes {} stands for any), RecordedNone when
// keploy read every recording of c's kind and none has it (or the set has
// none of c's kind), and "" when it cannot tell: c's protocol is one keploy
// reads no shape of from a recording (any but HTTP and MongoDB), c's summary
// has no shape, or a recording of c's kind was not read.
func SetShapeRecordings(c UnmatchedCall, recorded []RecordedCall) string {
	kind, ok := recordedKindOf(c.Protocol)
	if !ok {
		return ""
	}
	shape := MissShape(c)
	if isMongoCall(c.Protocol) {
		// A miss's shape names its database and filter keys where its
		// rendered request holds them; a recording's names neither.
		shape = CallShape(c.Protocol, c.ActualSummary)
	}
	if shape == "" {
		return ""
	}
	out := RecordedNone
	for _, r := range recorded {
		if r.Kind != kind {
			continue
		}
		if !r.Read {
			out = ""
			continue
		}
		if sameCallShape(shape, r.Shape) {
			return RecordedShape
		}
	}
	return out
}

// sameCallShape reports whether shapes a and b are one call's: equal, or
// equal segment by segment where either writes a segment {}.
func sameCallShape(a, b string) bool {
	if a == b {
		return true
	}
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] && as[i] != pathHole && bs[i] != pathHole {
			return false
		}
	}
	return true
}
