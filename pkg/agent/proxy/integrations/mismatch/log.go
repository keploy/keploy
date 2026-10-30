package mismatch

import (
	"go.keploy.io/server/v3/pkg/models"
)

// A miss report as a log line names it. A log is pasted into issues, shipped
// to collectors and bundled for support. A report's summary, diff and field
// diffs hold the values the application sent and the recording holds (an
// e-mail address in a body, a token in a query, a statement's literals); the
// report file keeps them for the person who reads it. A log line keeps what
// tells the calls apart: the call's shape, the fields that differ, and how
// long their values are.

// LogCall is the call a report names, as a log line names it: the parser's
// own log summary (models.MockMismatchReport.LogSummary) when it set one;
// otherwise (models.LoggedCall) its shape, an HTTP or gRPC call by its method
// and its target's shape, a statement with its literals written "?", a
// MongoDB command by its command and namespace; a generic exchange and a DNS
// question by the summary keploy wrote of it. "" for a call of any other
// protocol, whose summary may hold a key or a document.
func LogCall(r *models.MockMismatchReport) string {
	if r == nil {
		return ""
	}
	return models.LoggedCall(loggedCall(r))
}

// LogCallName is LogCall for a line that names calls among others
// (models.LoggedCallName): a call keploy has no shape for is named by its
// protocol ("a Redis call"), never by its summary.
func LogCallName(r *models.MockMismatchReport) string {
	if r == nil {
		return models.LoggedCallName(models.UnmatchedCall{})
	}
	return models.LoggedCallName(loggedCall(r))
}

// loggedCall is the part of r a log line names a call by.
func loggedCall(r *models.MockMismatchReport) models.UnmatchedCall {
	return models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary,
		FieldDiffs: r.FieldDiffs, VariablePlaces: r.VariablePlaces, LogSummary: r.LogSummary}
}

// LogDiff is how a report's call differs, as a log line says it
// (models.LoggedDiff): each field diff by its field's shape, how it differs
// and its values' lengths; a generic exchange's diff as keploy wrote it; and
// any other diff, a text that may quote the values, by its length.
func LogDiff(r *models.MockMismatchReport) string {
	if r == nil {
		return ""
	}
	return models.LoggedDiff(models.UnmatchedCall{Protocol: r.Protocol, Diff: r.Diff, FieldDiffs: r.FieldDiffs})
}

// LogNextStep is a report's next step as a log line says it: the matcher's
// advice, or the learn step's reason, with every field path it names in its
// shape (models.UnmatchedCall.SanitizedNextStep, the form a proven run's
// evidence keeps), so a map key that may be a value (an e-mail address, a
// user ID) is {}, and a sessionCalls pathPrefix it advises (the call's own
// path, which the report keeps) in the call's shape, as LogCall writes it. The text is keploy's own words: a matcher or a learn step
// writes no value into it (lengths and types instead).
func LogNextStep(r *models.MockMismatchReport) string {
	if r == nil {
		return ""
	}
	return models.UnmatchedCall{NextSteps: r.NextSteps, FieldDiffs: r.FieldDiffs, VariablePlaces: r.VariablePlaces}.SanitizedNextStep()
}
