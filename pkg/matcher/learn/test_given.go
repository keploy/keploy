package learn

import (
	"strconv"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

// A VALUE THE TEST GAVE.
//
// A test may tell keploy a value it chose and handed the application (POST
// /agent/scope/given, models.GivenPrefix): a name carrying the test's own
// clock, a token it signed. It is bound for the test as a template key is
// (variables.Binding.Given), so a call that carries it whole is learned as a
// use of the given's variable, and each replay checks that the call carries
// the value the test gives then. Unlike a template key, keploy does not read
// it from the test case's request, so the variable says where the test gave
// it and how to undo it (models.MockVariable.Why), for a person or a tool
// reading the mock (the legibility rule).

// givenWhyValue bounds how much of a value the why quotes: the variable's
// recorded: holds the whole of it.
const givenWhyValue = 80

// GivenWhy is why a mock's variable name is a given the test gave (source:
// where it gave it, file:line, empty when unknown): the call carried the
// value whole when recorded (recorded) and in record's full round (full),
// each the value the test gave then. It says what each replay checks and how
// to undo it, and is written as the variable's why.
func GivenWhy(name, source, recorded, full string) string {
	where, call := "", "the test's call that gives it"
	if source != "" {
		where = " (" + source + ")"
		call = "the test's call that gives it at " + source
	}
	return "the test gave " + name + where + ": " + quoteGiven(recorded) + " when recorded, " + quoteGiven(full) +
		" in record's full round, and this call carried it whole both times. Each replay checks that this call carries the value the test gives then. " +
		"To compare the recorded value instead, delete this " + name + " block; to stop giving it, remove " + call + " and re-record."
}

// TestGiven is a value the running test gave keploy as name (a
// models.GivenPrefix variable) as an earlier place of its calls
// (Tracker.GiveValue, Earlier.Gave): its one Seen holds the value the test
// gave when recorded (recorded) and in record's full round (value). test
// names the test ("test <id>"). A given the recording lacks (recorded "")
// is no place: nothing the recording holds came from it.
func TestGiven(test, name, recorded, value string) Earlier {
	e := Earlier{Mock: test, Given: true, Gave: true}
	if recorded != "" {
		e.Seen = []Seen{{Side: Request, Leaf: Leaf{Path: name, Type: String, Value: recorded}, Live: value}}
	}
	return e
}

// GivenDerivedWhy is why a call's leaf at path is refused that carries a
// value made from the one the test gave as name (source: where it gave it,
// file:line, empty when unknown), found at the given's place
// (Earlier.Gave): a part of it, or it in another form. keploy follows a
// given only where a call carries it whole, so the value is checked by
// nothing it could learn, and is never learned as noise.
func GivenDerivedWhy(path, name, source string) string {
	where := ""
	if source != "" {
		where = " (" + source + ")"
	}
	return path + " carries a value made from " + GivenNamed(name) + where + " (a part of it, or it in another form), and keploy follows a given only where a call carries it whole. " +
		"Give the value the application sends on as a given of its own, or have the test give the value the application sends whole"
}

// GivenNamed is how a learn step's reason names a given variable: the value
// the test gave as it, or, for a template key the test case's request gave,
// the test case's key.
func GivenNamed(name string) string {
	if models.IsGivenVariable(name) {
		return "the value the test gave as " + name
	}
	return "the test case's " + name
}

// quoteGiven quotes v, cut to givenWhyValue bytes on a rune boundary.
func quoteGiven(v string) string {
	if len(v) <= givenWhyValue {
		return strconv.Quote(v)
	}
	cut := givenWhyValue
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return strconv.Quote(v[:cut]) + "... (" + strconv.Itoa(len(v)) + " bytes)"
}
