package models

// Origin is where a recorded test or call came from, under the proven switch
// (S12 v16 §2.2, §4.4); nil when recorded without it, and then nothing is
// written.
//
// Only a record under the switch sets one. A file's origin is read and carried
// whatever the switch: every reader (YAML, JSON, gob) takes it, and every
// rewrite of a mock or a test case (--remove-unused-mocks, the noise
// write-back, templatize, normalize, an update) writes it back, so a run
// without the switch never strips what a run with it recorded. Only its use
// (routing, the reason lines) is under the switch.
//
// A mock's origin is written as its document's top-level origin block
// (yaml.NetworkTrafficDoc.Origin), a test case's likewise: Mock.Origin and
// TestCase.Origin are yaml "-" because the document, not the model, is what a
// recording's file holds.
type Origin struct {
	// Clock names the clock of the stamp that opens the record: a test case's
	// window start (HTTPReq/GrpcReq.Timestamp), a mock's ReqTimestampMock.
	// Equal non-empty values are one clock. "" is untrusted (a fallback, a
	// substituted or post-hole stamp) and never routes. Values are
	// fakeconn.ClockID's: "proxy:<agent run>", "ktime:<boot id>.<anchor>".
	Clock string `json:"clock,omitempty" yaml:"clock,omitempty" bson:"clock,omitempty"`
	// Comm and Exe are, on a mock, the process that made the call: its comm
	// and argv[0]. No more of its command line is ever stored.
	Comm string `json:"comm,omitempty" yaml:"comm,omitempty" bson:"comm,omitempty"`
	Exe  string `json:"exe,omitempty" yaml:"exe,omitempty" bson:"exe,omitempty"`
}

// clone is a copy of o that shares nothing with it; nil for nil.
func (o *Origin) clone() *Origin {
	if o == nil {
		return nil
	}
	c := *o
	return &c
}

// clock is o's Clock, "" for a nil o: the clock of a record without an
// origin is untrusted.
func (o *Origin) clock() string {
	if o == nil {
		return ""
	}
	return o.Clock
}
