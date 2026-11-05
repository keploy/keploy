package models

import (
	"maps"
	"slices"
	"strings"
)

// How a mock was authored (Authored.How).
const (
	// AuthoredDerived is a mock written from a recorded one: its request is
	// the recorded mock's, copied as it is, and its answer is the recorded
	// answer with Authored.Delta applied.
	AuthoredDerived = "derived"
	// AuthoredGenerated is a mock written for a call no recording holds:
	// its request and its answer are both the author's.
	AuthoredGenerated = "generated"
)

// Authored says that a mock was written by a person or an agent rather than
// recorded, and what was written: how (AuthoredDerived or
// AuthoredGenerated), why, and, for a derived mock, the recorded mock it
// was derived from and what it changed in that mock's answer. Every
// matcher serves an authored mock as it serves a recorded one; the field
// only carries this record with it, so that each rewrite of a recording
// keeps it and each reader can say which answers were authored. It is a
// kind-agnostic top-level `authored:` block of the mock's document, like
// `variables:` and `clocks:`; nil, so omitted, for a recorded mock.
// Additive: a recording without it loads as before, and an older keploy
// ignores the block.
type Authored struct {
	// How is AuthoredDerived or AuthoredGenerated.
	How string `json:"how" yaml:"how" bson:"how"`
	// Reason is the author's own words for why the mock exists ("declined
	// card"), said wherever the mock is reported.
	Reason string `json:"reason" yaml:"reason" bson:"reason"`
	// Source is, for a derived mock, the recorded mock it was derived from,
	// as "<test>/<mock>"; empty for a generated one.
	Source string `json:"source,omitempty" yaml:"source,omitempty" bson:"source,omitempty"`
	// Request is the digest of the mock's request as it was authored
	// ("sha256:<hex>"), which, for a derived mock, is its source's request
	// at the time: a reader tells by it whether the request, or its
	// source's, changed since.
	Request string `json:"request,omitempty" yaml:"request,omitempty" bson:"request,omitempty"`
	// Answer is the digest of the answer the mock serves as it was
	// authored ("sha256:<hex>"): for a derived mock, its source's answer
	// with Delta applied. A reader tells by it whether the answer was
	// edited since it was written, by hand or by any writer other than
	// keploy's authoring.
	Answer string `json:"answer,omitempty" yaml:"answer,omitempty" bson:"answer,omitempty"`
	// Delta is, for a derived mock, what it changed in its source's answer;
	// nil for a generated one.
	Delta *AuthoredDelta `json:"delta,omitempty" yaml:"delta,omitempty" bson:"delta,omitempty"`
	// Malformed says that its author wrote an answer body that does not
	// parse as its Content-Type says, on purpose: a generated mock's body,
	// or the body a derived mock's Delta sets.
	Malformed bool `json:"malformed,omitempty" yaml:"malformed,omitempty" bson:"malformed,omitempty"`
}

// AuthoredDelta is what a derived mock changed in its source's answer.
// Each field left empty is a part of the answer kept as recorded.
type AuthoredDelta struct {
	// Status is the answer's status code; 0 keeps the recorded one.
	Status int `json:"status,omitempty" yaml:"status,omitempty" bson:"status,omitempty"`
	// Body is the answer's whole body, which replaces the recorded one.
	Body *string `json:"body,omitempty" yaml:"body,omitempty" bson:"body,omitempty"`
	// JSONPatch is an RFC 6902 patch document, as JSON text, applied to the
	// recorded body. It and Body are never both set.
	JSONPatch string `json:"json_patch,omitempty" yaml:"json_patch,omitempty" bson:"json_patch,omitempty"`
	// Header is the headers set on the answer, each replacing the recorded
	// value of its name.
	Header map[string]string `json:"header,omitempty" yaml:"header,omitempty" bson:"header,omitempty"`
	// DropHeader is the headers taken off the answer.
	DropHeader []string `json:"drop_header,omitempty" yaml:"drop_header,omitempty" bson:"drop_header,omitempty"`
	// Fault is the fault the answer ends in (MockSpec.Fault), which
	// replaces the recorded one, if any; nil keeps the recorded answer's
	// ending.
	Fault *Fault `json:"fault,omitempty" yaml:"fault,omitempty" bson:"fault,omitempty"`
}

// Clone is a copy of a that shares nothing with it; nil for nil.
func (a *Authored) Clone() *Authored {
	if a == nil {
		return nil
	}
	c := *a
	if a.Delta != nil {
		d := *a.Delta
		if a.Delta.Body != nil {
			b := *a.Delta.Body
			d.Body = &b
		}
		d.Header = maps.Clone(a.Delta.Header)
		d.DropHeader = slices.Clone(a.Delta.DropHeader)
		if a.Delta.Fault != nil {
			f := *a.Delta.Fault
			d.Fault = &f
		}
		c.Delta = &d
	}
	return &c
}

// IsAuthored reports whether m was written, derived or generated, rather
// than recorded (MockSpec.Authored). A mock whose payload is left on disk
// (LeavePayloadOnDisk) keeps the answer until its payload is read back.
func (m *Mock) IsAuthored() bool {
	if m == nil {
		return false
	}
	if m.onDisk != nil {
		return m.onDisk.authored
	}
	return m.Spec.Authored != nil
}

// Replaces is, for a derived mock (AuthoredDerived), the name of the
// recording it was derived from: the mock part of Authored.Source
// ("<test>/<mock>"). A replay serves a derived mock in its own test in
// that recording's place: a replay of the test never serves the
// recording, which keeps answering every other test as it did
// (WithoutReplaced). "" for a generated or a recorded mock, and for a
// Source that names no mock.
func (m *Mock) Replaces() string {
	if m == nil {
		return ""
	}
	if m.onDisk != nil {
		return m.onDisk.replaces
	}
	return m.Spec.Authored.replaces()
}

// replaces is Mock.Replaces of a mock whose authored record is a.
func (a *Authored) replaces() string {
	if a == nil || a.How != AuthoredDerived {
		return ""
	}
	i := strings.LastIndex(a.Source, "/")
	if i < 0 {
		return ""
	}
	return a.Source[i+1:]
}

// ReplacedBy names the recordings the derived mocks of own, the mocks a
// replay serves a test as its own, replace in that test (Mock.Replaces).
// nil when own holds no derived mock.
func ReplacedBy(own []*Mock) map[string]struct{} {
	var out map[string]struct{}
	for _, m := range own {
		if n := m.Replaces(); n != "" {
			if out == nil {
				out = map[string]struct{}{}
			}
			out[n] = struct{}{}
		}
	}
	return out
}

// WithoutReplaced is mocks less each one named in replaced (ReplacedBy), in
// mocks' order: the pool a replay serves a test whose own mocks replace
// them. It shares mocks' backing array only when it drops none.
func WithoutReplaced(mocks []*Mock, replaced map[string]struct{}) []*Mock {
	if len(replaced) == 0 {
		return mocks
	}
	out := make([]*Mock, 0, len(mocks))
	for _, m := range mocks {
		if m != nil {
			if _, gone := replaced[m.Name]; gone {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}
