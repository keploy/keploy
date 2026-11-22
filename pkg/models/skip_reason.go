package models

import "strings"

// ManualSkipClass is the class of a quarantine a person added, whose reason is
// their own text.
const ManualSkipClass = "manual"

// SkipReasonText is the one text of the reason keploy skips a test for
// (TestResult.SkipReason, config.Test.SkipReasons), from its class, its reason
// and any detail: "<class>/<reason>", or "manual: <text>" for a quarantine a
// person added, then ": <detail>" when there is detail. Every writer of a skip
// reason builds it here, so the run's log, its summary, the report and JUnit
// say it one way.
func SkipReasonText(class, reason, detail string) string {
	var text string
	switch class {
	case "":
		text = reason
	case ManualSkipClass:
		text = class + ": " + reason
	default:
		text = class + "/" + reason
	}
	if detail != "" {
		if text != "" {
			text += ": "
		}
		text += detail
	}
	return text
}

// SkipReasonOneLine is a skip reason on one line: its runs of white space,
// line breaks included, each one space.
func SkipReasonOneLine(reason string) string {
	return strings.Join(strings.Fields(reason), " ")
}
