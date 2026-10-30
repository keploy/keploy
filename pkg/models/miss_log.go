package models

import (
	"fmt"
	"strings"
)

// A miss as a log line names it, wherever it is logged: by the agent as the
// miss is made, and by the CLI and keploy mock as a test or a set fails for
// it. A log is pasted into issues, shipped to collectors and bundled for
// support. A miss's summary, diff and field diffs hold the values the
// application sent and the recording holds (an e-mail address in a body, a
// token in a query, a statement's literals); the report and the test report
// keep them for the person who reads them. A log line keeps what tells the
// calls apart: the call's shape, the fields that differ, and how long their
// values are.

// The protocols whose summary keploy writes from no value of the call
// (mismatch.ProtocolGeneric, mismatch.ProtocolDNS).
const (
	logProtocolGeneric = "Generic"
	logProtocolDNS     = "DNS"
)

// LoggedCall is the call m names, as a log line names it: the parser's own
// log summary (LogSummary) when it stated one; otherwise its shape
// (MissShape), an HTTP or gRPC call by its method and its target's shape,
// with every path segment its field diffs or variable places name written
// {} whatever its look, a statement with its literals written "?", and a
// MongoDB command by its command, namespace and filter keys, each by its
// look; a generic exchange and a DNS question by the summary keploy wrote of
// it (request counts and sizes; a question's type and name, a
// destination's).
// "" for a call of any other protocol, whose summary may hold a key or a
// document.
func LoggedCall(m UnmatchedCall) string {
	if m.LogSummary != "" {
		return m.LogSummary
	}
	if shape := MissShape(m); shape != "" {
		return shape
	}
	switch m.Protocol {
	case logProtocolGeneric, logProtocolDNS:
		return m.ActualSummary
	}
	return ""
}

// LoggedCallName is LoggedCall for a line that names calls among others, as
// "test: call (...)": a call keploy has no shape for is named by its
// protocol ("a Redis call"), so the line still says what kind of call it
// was, and never by its summary, which may hold a key or a document. A call
// of no protocol, or of one keploy did not tell (Protocol "unknown"), is "a
// call".
func LoggedCallName(m UnmatchedCall) string {
	if call := LoggedCall(m); call != "" {
		return call
	}
	if m.Protocol == "" || strings.EqualFold(m.Protocol, "unknown") {
		return "a call"
	}
	return "a " + m.Protocol + " call"
}

// LoggedDiff is how m's call differs, as a log line says it: each field
// diff by its field's shape (FieldShape), how it differs, and how many bytes
// its recorded and live values hold; a generic exchange's diff as keploy
// wrote it (the closest recording's name and its similarity); and any other
// diff, a text that may quote the values, by its length.
func LoggedDiff(m UnmatchedCall) string {
	switch {
	case len(m.FieldDiffs) > 0:
		return loggedFieldDiffs(m.FieldDiffs)
	case m.Diff == "":
		return ""
	case m.Protocol == logProtocolGeneric:
		return m.Diff
	}
	return fmt.Sprintf("%d bytes, in the report", len(m.Diff))
}

// loggedFieldDiffs is the field diffs as a report renders them, with each
// value written as its length.
func loggedFieldDiffs(diffs []MockFieldDiff) string {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		field := FieldShape(d.Path)
		switch d.Kind {
		case DiffKindMissingInLive:
			parts = append(parts, fmt.Sprintf("%s: recorded (%d bytes), absent in live call", field, len(d.Expected)))
		case DiffKindMissingInMock:
			parts = append(parts, fmt.Sprintf("%s: live (%d bytes), absent in recording", field, len(d.Actual)))
		case DiffKindTypeChanged:
			parts = append(parts, field+": type changed")
		default:
			parts = append(parts, fmt.Sprintf("%s: recorded %d bytes, live %d bytes", field, len(d.Expected), len(d.Actual)))
		}
	}
	return strings.Join(parts, "; ")
}
