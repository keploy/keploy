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

// SkipKind is what a skip is, beside its reason: config.Test.SkipKinds gives
// it for the run, and TestResult.SkipKind carries it, so the run's summary,
// the text report and JUnit each say a skipped test is what it is. A skip
// whose caller names no kind is SkipQuarantined, as every skip was before
// there were kinds, and is said as it was.
type SkipKind string

const (
	// SkipQuarantined is a test keploy skips while a quarantine holds it. It
	// is the kind of a skip whose caller names none.
	SkipQuarantined SkipKind = ""
	// SkipThisRunOnly is a test skipped for this run only: no quarantine
	// holds it, and a run that does not skip it runs it (keploy test
	// --judge's rerun skips the tests it suggested quarantining; a cloud
	// replay skips a test set whose mocks did not download).
	SkipThisRunOnly SkipKind = "this_run_only"
	// SkipUntilRerecorded is a test skipped until it is recorded again: no
	// quarantine holds it, and every run skips it until a new recording of
	// it is vouched for (a cloud replay skips a test with no mapping entry).
	SkipUntilRerecorded SkipKind = "until_rerecorded"
)

// SkipKinds returns the kinds of skip keploy says apart, in the order a
// summary lists them: the quarantined first, as before there were kinds. Each
// call returns a new slice, so no caller can change the order another reads.
func SkipKinds() []SkipKind {
	return []SkipKind{SkipQuarantined, SkipThisRunOnly, SkipUntilRerecorded}
}

// Said is the kind a skip of kind k is said as: k when keploy knows it, and
// SkipQuarantined for a kind it does not (one a later keploy wrote into a
// report), whose skip is said as every skip was before there were kinds.
func (k SkipKind) Said() SkipKind {
	switch k {
	case SkipThisRunOnly, SkipUntilRerecorded:
		return k
	}
	return SkipQuarantined
}
