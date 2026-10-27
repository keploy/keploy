package models

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The spellings of About.Seen and About.Call, beside the format's words
// (AboutKey and the rest): renaming one before the format is agreed is a
// change here.
const (
	// AboutSeenCut is how many characters of a value About.Seen writes.
	AboutSeenCut = 64
)

// AboutSeenCredentialHeaders are headers whose value About.Seen never
// writes. IsSensitiveKey names each of them and each of CredentialHeaders
// (TestEveryCredentialHeaderIsASensitiveKey pins it), so credentialAt reads
// IsSensitiveKey alone.
var AboutSeenCredentialHeaders = []string{"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key"}

// SeenValue is value as About.Seen writes it at place (a protocol's leaf
// path: "header.x-api-key", "body.items.0.id"): cut to AboutSeenCut
// characters with its full length noted, or, at a place or of a shape keploy
// treats as a credential, not written at all, its length alone noted.
func SeenValue(place, value string) string {
	n := utf8.RuneCountInString(value)
	if what := credentialAt(place, value); what != "" {
		return "<not written: " + what + ", " + strconv.Itoa(n) + " chars>"
	}
	if n <= AboutSeenCut {
		return value
	}
	return string([]rune(value)[:AboutSeenCut]) + "... (" + strconv.Itoa(n) + " chars)"
}

// NewAboutSeen is what the recording (record) and record's full round (full)
// saw at a decision's places, each as SeenValue writes it at the first of
// places keploy treats as a credential when one is (a variable's value is
// one value at its bind and at each use); else only the value's own shape
// can keep it out.
func NewAboutSeen(record, full string, places ...string) *AboutSeen {
	at := ""
	for _, p := range places {
		if credentialAt(p, "") != "" {
			at = p
			break
		}
	}
	return &AboutSeen{Record: SeenValue(at, record), Full: SeenValue(at, full)}
}

// credentialAt names what makes value at place one About.Seen does not
// write: a credential header, a key that looks like one, or a value of a
// credential's shape; "" for any other.
func credentialAt(place, value string) string {
	switch {
	case IsSensitiveKey(place) && strings.HasPrefix(strings.ToLower(place), "header."):
		return "a credential header"
	case IsSensitiveKey(place):
		return "a credential field"
	case LooksLikeSecretValue(value):
		return "a credential"
	}
	return ""
}

// AboutCall is About.Call: the protocol, the destination as recorded, the
// operation (HTTP: method and path, no query; Mongo: command and
// collection), then #n, the call's place among the test's calls with the
// same key (n from 1). "" when n is not a place.
func AboutCall(protocol, dest, operation string, n int) string {
	if n < 1 {
		return ""
	}
	return protocol + " " + dest + " " + operation + " #" + strconv.Itoa(n)
}
