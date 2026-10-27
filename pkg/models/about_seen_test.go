package models

import (
	"reflect"
	"strings"
	"testing"
)

// seen is cut at 64 characters with the full length noted, and a value at a
// credential header, at a key that looks like one, or of a credential's
// shape is never written, its length alone noted. Row 9.
func TestSeenIsCutAndNeverWritesACredential(t *testing.T) {
	long := strings.Repeat("a", 64) + "bcdef"
	for _, tc := range []struct{ place, value, want string }{
		{"body.id", "o-1111", "o-1111"},
		{"body.id", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"body.id", long, strings.Repeat("a", 64) + "... (69 chars)"},
		{"body.name", strings.Repeat("é", 70), strings.Repeat("é", 64) + "... (70 chars)"},
		{"header.authorization", "Bearer abc", "<not written: a credential header, 10 chars>"},
		{"header.Proxy-Authorization", "Basic xyz", "<not written: a credential header, 9 chars>"},
		{"header.cookie", "sid=1", "<not written: a credential header, 5 chars>"},
		{"header.set-cookie", "sid=1", "<not written: a credential header, 5 chars>"},
		{"header.x-api-key", "k", "<not written: a credential header, 1 chars>"},
		{"header.x-csrf-token", "c", "<not written: a credential header, 1 chars>"},
		{"header.idempotency-key", "i", "<not written: a credential header, 1 chars>"},
		{"body.password", "hunter2", "<not written: a credential field, 7 chars>"},
		{"url.query.api_key", "q", "<not written: a credential field, 1 chars>"},
		{"body.note", "eyJhbGciOi.eyJzdWIiOi.c2lnbmF0dXJl", "<not written: a credential, 34 chars>"},
	} {
		if got := SeenValue(tc.place, tc.value); got != tc.want {
			t.Errorf("SeenValue(%q, %q) = %q, want %q", tc.place, tc.value, got, tc.want)
		}
	}
	// A variable's value is one value at its bind and at each use: a
	// credential place among them keeps it out at every one.
	s := NewAboutSeen("t-rec", "t-full", "body.data", "header.authorization")
	if s.Record != "<not written: a credential header, 5 chars>" || s.Full != "<not written: a credential header, 6 chars>" {
		t.Errorf("seen %+v, want the values not written", s)
	}
	if s := NewAboutSeen("r", "f", "body.data"); s.Record != "r" || s.Full != "f" {
		t.Errorf("seen %+v, want both values", s)
	}
}

// A call key spells the protocol, the destination, the operation and the
// call's place; a place below 1 is none.
func TestAboutCallSpellsTheKey(t *testing.T) {
	if got := AboutCall("http", "audit:8080", "POST /v1/events", 2); got != "http audit:8080 POST /v1/events #2" {
		t.Errorf("AboutCall = %q", got)
	}
	if got := AboutCall("http", "audit:8080", "POST /v1/events", 0); got != "" {
		t.Errorf("AboutCall with no place = %q, want none", got)
	}
}

// The first learner wins: a place in both keeps the first's about, and
// neither input is changed.
func TestMergeAboutsKeepsTheFirstLearners(t *testing.T) {
	a := map[string]About{"body.a": {Why: "first"}}
	b := map[string]About{"body.a": {Why: "second"}, "body.b": {Why: "second"}}
	got := MergeAbouts(a, b)
	want := map[string]About{"body.a": {Why: "first"}, "body.b": {Why: "second"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeAbouts = %+v, want %+v", got, want)
	}
	if len(a) != 1 || len(b) != 2 {
		t.Fatalf("an input changed: %+v %+v", a, b)
	}
	if MergeAbouts(nil, nil) != nil {
		t.Fatal("two empty inputs merge to a map")
	}
}

// Every header the founder named, and every one keploy already treats as a
// credential, is a key IsSensitiveKey names, so About.Seen never writes it.
func TestEveryCredentialHeaderIsASensitiveKey(t *testing.T) {
	for _, h := range append(append([]string(nil), AboutSeenCredentialHeaders...), CredentialHeaders...) {
		if !IsSensitiveKey(h) || credentialAt("header."+h, "") != "a credential header" {
			t.Errorf("%s is not read as a credential header", h)
		}
	}
}
