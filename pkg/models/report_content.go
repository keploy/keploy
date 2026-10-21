package models

import (
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/utils/logcontent"
)

// A mock-mismatch report is kept in the test-run report, printed on every
// miss, written to JUnit and uploaded, and parsers put the application's
// content in it: the values of the fields a call differs in, the whole
// recorded and live requests, a SQL statement's literals. It takes the
// consent a log line takes, logcontent's switch (KEPLOY_LOG_CONTENT): with
// it off, the default, a report says what differed (the call, its fields'
// paths, how each differs, counts) and never the values. The functions here
// are the one rule every builder, writer and printer of a report applies.

// WithheldValue stands, in a report and its rendering, for a value the
// application sent or keploy recorded, when the report carries no content.
const WithheldValue = "(withheld)"

// ContentWithheldNote tells a reader of a report whose values were withheld
// how to see them.
const ContentWithheldNote = "values are withheld; set " + logcontent.EnvVar + "=1 to include them"

// ReportFileWithheldNote tells a reader of a report file (keploy report) whose
// values are withheld how to see them. The values a file was written without
// come back only from a replay with logcontent's switch set, not from setting
// it where the file is read: with it set there, every call still withheld was
// written so, and the note says to replay; without it, the note says both.
func ReportFileWithheldNote() string {
	if logcontent.On() {
		return "values were withheld when this report was written; replay with " + logcontent.EnvVar + "=1 to include them"
	}
	return ContentWithheldNote + " (for a report written without them, set it for the replay)"
}

// ReportQuote is v as a report's text may quote it: strconv.Quote(v) with
// logcontent's switch on, and WithheldValue with it off. A parser whose
// report text names a value the application sent or keploy recorded passes it
// through this, so the text keeps naming the field and drops the value.
func ReportQuote(v string) string {
	if logcontent.On() {
		return strconv.Quote(v)
	}
	return WithheldValue
}

// ReportValue is v, a value already written as the text shows it (a JSON
// literal, a quoted string), as a report's text may carry it: v with
// logcontent's switch on, and WithheldValue with it off.
func ReportValue(v string) string {
	if logcontent.On() {
		return v
	}
	return WithheldValue
}

// ReportSQL is a SQL statement as a report's text may carry it: whole with
// logcontent's switch on, and with every literal written "?" (SQLShape) with
// it off.
func ReportSQL(q string) string {
	if logcontent.On() {
		return q
	}
	return SQLShape(q)
}

// ProtocolUnknown is the protocol of a miss whose parser reported no report
// (UnmatchedCall.Protocol): the proxy names it by its error's text.
const ProtocolUnknown = "unknown"

// SummaryWithoutContent is a miss's summary (MockMismatchReport.ActualSummary)
// as a report without content carries it.
//
//   - SQL: the statement with its literals written "?" (SQLShape).
//   - HTTP and gRPC: the method and the path, without a query, and with every
//     path segment keploy knows holds one of the application's values written
//     {}: a place in places ("url.path[<n>]": a test variable's, or one a
//     field diff names). Every other segment is kept, as keploy names a test
//     case by its path; the parsers take the path without its query already
//     (URL.Path; HTTP/2 cuts it), and the query is cut here too.
//   - A miss with no report (ProtocolUnknown) is named by an error's text,
//     which no report builder wrote, so it is withheld.
//   - Any other protocol's is its command or operation, by the contract of
//     the report builder (mismatch.NewReport), and kept.
//
// The SQL rule is applied once, where a report is first withheld (Build, or
// a writer's WithoutContent of a report nobody withheld); the path rule may
// be applied again, as later steps add places to a report.
func SummaryWithoutContent(protocol, summary string, places []string) string {
	switch {
	case isSQLCall(protocol):
		return SQLShape(summary)
	case protocol == ProtocolUnknown && summary != "":
		return WithheldValue
	}
	return pathSummaryWithoutContent(protocol, summary, places)
}

// pathSummaryWithoutContent is SummaryWithoutContent's HTTP and gRPC rule, and
// summary unchanged for any other protocol.
func pathSummaryWithoutContent(protocol, summary string, places []string) string {
	if !IsPathProtocol(protocol) {
		return summary
	}
	if i := strings.IndexByte(summary, '?'); i >= 0 {
		summary = strings.TrimRightFunc(summary[:i], func(r rune) bool { return r == ' ' })
	}
	return holePathPlaces(summary, places)
}

// RenderFieldDiffs renders field diffs as one line, for the surfaces that
// cannot show structured data (a table cell, a log line, the report's Diff):
// each field's path, how it differs and, unless withheld, its values.
func RenderFieldDiffs(diffs []MockFieldDiff, withheld bool) string {
	if len(diffs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		if withheld {
			parts = append(parts, d.Path+": "+DiffKindText(d.Kind))
			continue
		}
		switch d.Kind {
		case DiffKindMissingInLive:
			parts = append(parts, fmt.Sprintf("%s: recorded %q, absent in live call", d.Path, d.Expected))
		case DiffKindMissingInMock:
			parts = append(parts, fmt.Sprintf("%s: live %q, absent in recording", d.Path, d.Actual))
		case DiffKindTypeChanged:
			parts = append(parts, fmt.Sprintf("%s: type changed (recorded %s, live %s)", d.Path, d.Expected, d.Actual))
		default:
			parts = append(parts, fmt.Sprintf("%s: recorded %q, live %q", d.Path, d.Expected, d.Actual))
		}
	}
	return strings.Join(parts, "; ")
}

// DiffKindText says how a field differs, without its values.
func DiffKindText(k MockFieldDiffKind) string {
	switch k {
	case DiffKindMissingInLive:
		return "absent in live call"
	case DiffKindMissingInMock:
		return "absent in recording"
	case DiffKindTypeChanged:
		return "type changed"
	case DiffKindVariable:
		return "not the test variable's value"
	}
	return "value changed"
}

// WithoutContent is r without the application's content, as a report
// carries it with logcontent's switch off (ContentWithheld): its field diffs
// keep their paths and kinds and lose their values, the rendered requests are
// dropped, saying whether they were the same (RequestsIdentical), and its
// summary is SummaryWithoutContent's. A report already
// withheld (mismatch.Build) keeps its Diff and its summary's SQL, the
// parser's own words, and loses again any content written into it since. Any
// other report's Diff is text nobody vetted, so it is rendered again from the
// field diffs' paths and kinds. Its NextSteps, and a Diff it keeps, are the
// parser's words, and keep everything but their quotes of the summary, which
// name the call as its summary now does (QuotesOfSummary).
func (r MockMismatchReport) WithoutContent() MockMismatchReport {
	withholdContent(r.Protocol, r.VariablePlaces, &r.ActualSummary, &r.Diff, &r.NextSteps, &r.FieldDiffs, &r.ClosestMockReq, &r.ReceivedReq, &r.RequestsIdentical, &r.ContentWithheld)
	return r
}

// WithoutContent is c without the application's content, as
// MockMismatchReport.WithoutContent makes a report: the rule a writer or
// printer of a report applies to a call it was handed, from an agent that may
// not have withheld it or from a report file written with the switch on.
func (c UnmatchedCall) WithoutContent() UnmatchedCall {
	withholdContent(c.Protocol, c.VariablePlaces, &c.ActualSummary, &c.Diff, &c.NextSteps, &c.FieldDiffs, &c.ClosestMockReq, &c.ReceivedReq, &c.RequestsIdentical, &c.ContentWithheld)
	return c
}

// ReportContent is c as a writer or printer of a report may keep it: whole
// with logcontent's switch on, WithoutContent with it off.
func (c UnmatchedCall) ReportContent() UnmatchedCall {
	if logcontent.On() {
		return c
	}
	return c.WithoutContent()
}

// ReportContent is calls as a writer or printer of a report may keep them
// (UnmatchedCall.ReportContent), in a new slice, so the caller's is never
// changed under a reader holding it: nil for none.
func ReportContent(calls []UnmatchedCall) []UnmatchedCall {
	if calls == nil {
		return nil
	}
	out := make([]UnmatchedCall, len(calls))
	for i, c := range calls {
		out[i] = c.ReportContent()
	}
	return out
}

// ReportContent is r as a writer or printer of a report read from a file may
// keep it: the unmatched calls of each of its tests and of its set as
// UnmatchedCall.ReportContent keeps them. A report file written with the
// switch on, or by an older keploy, is read with this before anything of it
// is printed or written again: its calls lose what WithoutContent has a rule
// for, the values of their field diffs, their rendered requests, the
// summary's SQL literals, query and known path places, and every quote of
// the summary in their words. A summary of any other protocol is kept as its
// builder wrote it, which by the builder's contract names the command or
// operation (SummaryWithoutContent): a file whose builder broke that contract
// keeps what it wrote there (integrations' Mongo builder named a command's
// first value before it named only a collection), as a reader cannot
// withhold what it has no rule for. r itself is not changed: the copy shares
// only what it does not change.
func (r *TestReport) ReportContent() *TestReport {
	if r == nil || logcontent.On() {
		return r
	}
	out := *r
	out.UnclaimedCalls = ReportContent(r.UnclaimedCalls)
	if r.Tests != nil {
		out.Tests = make([]TestResult, len(r.Tests))
		for i, t := range r.Tests {
			t.FailureInfo.UnmatchedCalls = ReportContent(t.FailureInfo.UnmatchedCalls)
			out.Tests[i] = t
		}
	}
	return &out
}

func withholdContent(protocol string, places []string, summary, diff, next *string, diffs *[]MockFieldDiff, mockReq, liveReq *string, identical, withheld *bool) {
	// The places a field diff names hold the application's values too.
	known := append([]string(nil), places...)
	for _, d := range *diffs {
		known = append(known, d.Path)
	}
	was := *summary
	if *withheld {
		*summary = pathSummaryWithoutContent(protocol, *summary, known)
		*diff = QuotesOfSummary(protocol, *diff, was, *summary)
	} else {
		// Before the diffs lose their values: a diff of the whole path is
		// read by them.
		known = append(known, pathDiffPlaces(*summary, *diffs)...)
		*summary = SummaryWithoutContent(protocol, *summary, known)
		*diff = RenderFieldDiffs(*diffs, true)
	}
	*next = QuotesOfSummary(protocol, *next, was, *summary)
	*diffs = fieldDiffsWithoutValues(*diffs)
	if !*withheld || *mockReq != "" || *liveReq != "" {
		// The requests the report holds now: those it was made with, or, in
		// a report withheld before, those written into it since (a not-owned
		// miss's recording and its call, rendered again), which the
		// comparison made then was not of.
		*identical = *mockReq != "" && *mockReq == *liveReq
	}
	*mockReq, *liveReq = "", ""
	*withheld = true
}

// QuotesOfSummary is text, words a parser wrote into a report (its
// NextSteps, or a Diff the report keeps), with every quote of the call's
// summary written as the summary is now that the report withholds content:
// was is the summary before, now after. A parser's words name fields, mocks
// and commands, never a value, and the one piece of the call they quote is
// its summary, or, of a call with a path (IsPathProtocol), the summary's
// path, as Go quotes a string (strconv.Quote, the session-call hint's
// pathPrefix: the HTTP and HTTP/2 parsers name the call's path there as
// their summary does). A reader that withholds the summary, from a report
// made with logcontent's switch on, or a withheld one whose summary gained
// holes since, withholds the same text wherever the words quote it: the
// whole summary, quoted or not, and its path, quoted. Text that quotes
// neither is returned as it was.
func QuotesOfSummary(protocol, text, was, now string) string {
	if text == "" || was == "" || was == now {
		return text
	}
	quotes := [][2]string{{strconv.Quote(was), strconv.Quote(now)}, {was, now}}
	if IsPathProtocol(protocol) {
		if ws, we, ok := shapePath(was); ok {
			if ns, ne, ok := shapePath(now); ok && was[ws:we] != now[ns:ne] {
				quotes = append(quotes, [2]string{strconv.Quote(was[ws:we]), strconv.Quote(now[ns:ne])})
			}
		}
	}
	for _, q := range quotes {
		text = strings.ReplaceAll(text, q[0], q[1])
	}
	return text
}

// PathFieldDiff is the path of the field diff that says a call's whole URL
// path is not the recorded one (the HTTP matchers'): its recorded value is
// the closest recording's path, its live value the call's.
const PathFieldDiff = "path"

// pathDiffPlaces are the places (url.path[<n>]) of the segments of summary's
// path that a PathFieldDiff of diffs finds are not the recorded path's. The
// diff's live value is the summary's path, so with its values withheld those
// segments would still name them: they are the values that differ, whatever
// their look. Each segment is compared with the recorded path's
// (PathPlacesOtherThan). It reads the diff's recorded value, so it is
// applied before the values are withheld.
func pathDiffPlaces(summary string, diffs []MockFieldDiff) []string {
	var places []string
	for _, d := range diffs {
		if d.Path == PathFieldDiff {
			places = append(places, PathPlacesOtherThan(summary, d.Expected)...)
		}
	}
	return places
}

// PathPlacesOtherThan are the places (url.path[<n>]) of the segments of
// summary's path that are not recorded's, the path of the recording the call
// was compared with: each segment is compared with recorded's at the same
// place and kept only when it is that one; a segment past recorded's end is
// named, so what cannot be compared is withheld. The call holds the
// application's values there, whatever their look: a report names them as
// places (MockMismatchReport.VariablePlaces) when the comparison found its
// path differs from recorded at places keploy knows vary (the strict HTTP
// comparison's learned or keploy.yml url noise), and a "path" field diff's
// are found the same way (pathDiffPlaces).
func PathPlacesOtherThan(summary, recorded string) []string {
	segs, ok := shapePathSegments(summary)
	if !ok {
		return nil
	}
	rec := strings.Split(strings.TrimPrefix(recorded, "/"), "/")
	var places []string
	for i, s := range segs {
		if i >= len(rec) || rec[i] != s {
			places = append(places, "url.path["+strconv.Itoa(i)+"]")
		}
	}
	return places
}

// fieldDiffsWithoutValues is diffs with their paths and kinds and no values,
// in a new slice: the caller's may be shared with the report it came from.
func fieldDiffsWithoutValues(diffs []MockFieldDiff) []MockFieldDiff {
	if diffs == nil {
		return nil
	}
	out := make([]MockFieldDiff, len(diffs))
	for i, d := range diffs {
		out[i] = MockFieldDiff{Path: d.Path, Kind: d.Kind}
	}
	return out
}
