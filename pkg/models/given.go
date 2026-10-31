package models

import (
	"strconv"
	"strings"
)

// A given is a value the test chose, not the application: a name carrying
// the test's own clock, a token it signed. The test tells keploy the value
// it handed the application (POST /agent/scope/given, the proven wire), and
// keploy treats it as keploy test treats a template key: a Given variable
// binding (VariableBinding.Given) of the open test, which the learn step
// follows into the calls that carry it as a use-only variable, and a replay
// checks that each such call carries the value the test gives then. In a
// record it is written into the test's mapping (MappedTestCase.Given), so a
// replay pairs each new value with the recorded one.
//
// A given is small and bounded (MaxGivenName, MaxGivenValue, and per test
// and per record by the agent). A replay holds the values a test gives only
// while the test is open, and the values the set's tests gave when recorded
// for the whole replay of the set (its scope table's, until the next set's
// replace them); a record holds each test's with its window, for the test's
// mapping, until the next record session begins, at most 64 MiB a session.

// GivenPrefix starts every given's variable name. The learn step's own names
// use only letters, digits, '_' and '-', so a given can never take or lose a
// learned variable's name. It ends in ':', not '.': a variable's name is a key
// of a mock's variables (MockSpec.Variables), which Keploy Cloud keeps in
// MongoDB, where a '.' in a key is read as a path. MongoDB stores and reads
// back a document holding one (4.4, 5.0 and 7.0 do), but no query, update or
// index by path can name that key.
const GivenPrefix = "given:"

// MaxGivenName, MaxGivenValue and MaxGivenSource bound a given's name,
// value and source.
const (
	MaxGivenName   = 64
	MaxGivenValue  = 64 << 10
	MaxGivenSource = 1 << 10
)

// GivenReq is the body of POST /agent/scope/given: a value the open test
// gave the application. Source is where the test gave it (file:line,
// relative to the project), used only in wording.
type GivenReq struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"`
}

// GivenResp answers a given: the variable keploy holds it as.
type GivenResp struct {
	V        int    `json:"v"`
	Status   string `json:"status"`
	Variable string `json:"variable"`
}

// GivenValue is one value a test gave while it was open: its variable
// (GivenVariable), the value, and where the test gave it.
type GivenValue struct {
	Name   string `json:"name" yaml:"name"`
	Value  string `json:"value" yaml:"value"`
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
}

// ValidGivenName reports whether name may name a given: 1 to MaxGivenName
// characters, each a letter, a digit, '_' or '-'.
func ValidGivenName(name string) bool {
	if name == "" || len(name) > MaxGivenName {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// GivenVariable is the variable name of the n-th value (from 1) a test gave
// under name: given:<name>, then given:<name>#2, #3. The n-th repeat pairs
// with the recording's n-th, so a test that gives in another order than it
// was recorded in fails, and never passes falsely.
func GivenVariable(name string, n int) string {
	if n <= 1 {
		return GivenPrefix + name
	}
	return GivenPrefix + name + "#" + strconv.Itoa(n)
}

// IsGivenVariable reports whether a variable name is a given's.
func IsGivenVariable(name string) bool {
	return strings.HasPrefix(name, GivenPrefix)
}

// ValidGivenVariable reports whether v is a given's variable name as
// GivenVariable writes it: given:<name>, or given:<name>#<n> with n from 2,
// where name is a ValidGivenName. A miss's evidence names a given only when
// it is one (UnmatchedCall.Sanitized): a variables block edited by hand can
// hold any name.
func ValidGivenVariable(v string) bool {
	name, ok := strings.CutPrefix(v, GivenPrefix)
	if !ok {
		return false
	}
	if base, n, repeat := strings.Cut(name, "#"); repeat {
		k, err := strconv.Atoi(n)
		if err != nil || k < 2 || strconv.Itoa(k) != n {
			return false
		}
		name = base
	}
	return ValidGivenName(name)
}
