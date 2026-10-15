package models

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// A stateful dependency is a request recorded several times with successive
// responses (a counter, a created-then-read row). The HTTP matcher's
// stateful cursor (cursorPick) serves the recordings of one request in record
// order and then saturates on the last (ConsumeCursorSaturate).

// perTestSequencePrefix marks the sequence of per-test recordings
// (StatefulSequenceKey).
const perTestSequencePrefix = "perTest\x00"

// StatefulSequenceKey is the identity the HTTP matcher's stateful cursor
// (cursorPick) groups m's recordings by, and whether m is one it walks: an
// HTTP mock DeriveLifetime gives the record-order cursor
// (ConsumeCursorSaturate). Recordings with the same key are one sequence: the
// same method, URL and body (byte-exact), and the same lifetime class:
// per-test recordings form their own sequences, apart from reusable ones
// (IsPerTestSequenceKey). (No recording is both per-test and on the cursor
// until recorders state per-test lifetimes for HTTP; cursorPick splits the
// classes from then.)
//
// It is not the identity of every walk. Under the proven switch the strict
// and variable passes (strictCursorPick, pickHit) walk every cursor recording
// that fits the call, which can differ at a noise-excused body field or at a
// variable's place, and so have different keys: those walks have no fixed
// key.
//
// A mock not derived yet is derived on a copy, without a side effect: it does
// not count in LegacyKindFallbackFires, which measures the mocks a replay
// loads to serve, and m is left as it was.
func StatefulSequenceKey(m *Mock) (string, bool) {
	if m == nil || m.Kind != HTTP || m.Spec.HTTPReq == nil {
		return "", false
	}
	info := m.TestModeInfo
	if !info.LifetimeDerived {
		c := *m
		c.derive(false)
		info = c.TestModeInfo
	}
	if info.Consume != ConsumeCursorSaturate {
		return "", false
	}
	key := HTTPSequenceKey(string(m.Spec.HTTPReq.Method), m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body)
	if info.Lifetime == LifetimePerTest {
		key = perTestSequencePrefix + key
	}
	return key, true
}

// HTTPSequenceKey is the request part of a stateful sequence's identity: its
// method, URL and a hash of its body. The HTTP integration's stateful cursor
// is keyed on it.
func HTTPSequenceKey(method, url, body string) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(url)
	b.WriteByte('\x00')
	sum := sha256.Sum256([]byte(body))
	b.WriteString(hex.EncodeToString(sum[:]))
	return b.String()
}

// IsPerTestSequenceKey reports whether key, a StatefulSequenceKey, is a
// per-test sequence's.
func IsPerTestSequenceKey(key string) bool {
	return strings.HasPrefix(key, perTestSequencePrefix)
}
