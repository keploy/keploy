package models

import "strconv"

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
	// Unnamed is why Comm and Exe are empty on a mock or a call whose
	// process keploy saw but would not name: UnnamedPIDNamespace. PID is
	// then that process's kernel pid, which a log line gives instead of a
	// name (ProcessText). Neither is ever written to a recording's file, a
	// test case's or a report (yaml and bson "-", and Persisted, which every
	// mock, test-case and report writer writes): the agent hands them to
	// keploy record with the mock (gob) and to the replay with the call
	// (json); the log lines that name the process say both, a committed line
	// why alone (FileProcessText).
	Unnamed string `json:"unnamed,omitempty" yaml:"-" bson:"-"`
	PID     uint32 `json:"pid,omitempty" yaml:"-" bson:"-"`
}

// UnnamedPIDNamespace is Origin.Unnamed when keploy runs in a PID namespace
// of its own (utils.KernelPidsAreLocal false): the kernel pid its proxy is
// told names another process in keploy's /proc, or none, so keploy names
// none rather than guess.
const UnnamedPIDNamespace = "pid_namespace"

// pidNamespaceWhy is why keploy names no process it was told of by a
// kernel pid (UnnamedPIDNamespace), and what has it named.
const pidNamespaceWhy = "keploy runs in a PID namespace of its own, where the kernel's process numbers name other processes or none, so it reads no process by one; run keploy in the host PID namespace (--pid=host, or hostPID in Kubernetes) to have it named. keploy's own docker and compose modes run its agent in a PID namespace of its own (the application's), where it names no process yet"

// PIDNamespaceReason says why keploy names no process for kernel pid pid
// (UnnamedPIDNamespace), and how to have it named, as a log line says it.
func PIDNamespaceReason(pid uint32) string {
	return "kernel pid " + strconv.FormatUint(uint64(pid), 10) + ": " + pidNamespaceWhy
}

// ProcessText names the process o says made a call, as a log line names it:
// "`sqs` (`./sqs`)" (comm, then argv[0] when it differs); for one keploy saw
// but would not name, "a process keploy could not name (" and
// PIDNamespaceReason ")", its kernel pid with it; "" when o names none,
// which the caller says its own way.
func (o *Origin) ProcessText() string {
	return o.processText(true)
}

// FileProcessText is ProcessText as a committed line (mappings.yaml's
// reasons) names the process: for one keploy would not name, why without its
// kernel pid, which no committed file holds (S12 v16 §4.4: a pid differs
// from one recording to the next).
func (o *Origin) FileProcessText() string {
	return o.processText(false)
}

func (o *Origin) processText(withPID bool) string {
	if o == nil {
		return ""
	}
	switch {
	case o.Comm == "" && o.Exe == "":
		if o.Unnamed == UnnamedPIDNamespace {
			if !withPID {
				return "a process keploy could not name (" + pidNamespaceWhy + ")"
			}
			return "a process keploy could not name (" + PIDNamespaceReason(o.PID) + ")"
		}
		return ""
	case o.Exe == "" || o.Exe == o.Comm:
		return "`" + o.Comm + "`"
	case o.Comm == "":
		return "`" + o.Exe + "`"
	}
	return "`" + o.Comm + "` (`" + o.Exe + "`)"
}

// Persisted is what of o a file holds: o without Unnamed and PID, which only
// travel; nil when nothing is left, so a mock whose process keploy would not
// name and whose clock is unset is written with no origin, as one recorded
// without the switch is.
func (o *Origin) Persisted() *Origin {
	if o == nil || o.Unnamed == "" && o.PID == 0 {
		return o
	}
	p := Origin{Clock: o.Clock, Comm: o.Comm, Exe: o.Exe}
	if p == (Origin{}) {
		return nil
	}
	return &p
}

// PersistedCalls is calls as a file holds them (a test run's report): each
// Source as Persisted leaves it, without what only travels. changed says
// whether any did; calls itself is returned when none did, else a copy:
// calls is never edited.
func PersistedCalls(calls []UnmatchedCall) (out []UnmatchedCall, changed bool) {
	for i := range calls {
		if p := calls[i].Source.Persisted(); p != calls[i].Source {
			if !changed {
				out, changed = append([]UnmatchedCall(nil), calls...), true
			}
			out[i].Source = p
		}
	}
	if !changed {
		return calls, false
	}
	return out, true
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
