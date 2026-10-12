package models

import "time"

// ScopeStartMetadataKey is the Spec.Metadata key under which a recorded mock
// carries the start of the scope window that owned it (FormatScopeStart).
//
// A browser harness pins the clock its tests read so the values they put in
// requests (generated names, "expires in 7 days") come out the same at record
// and replay. Pinned to one start for the whole run, a test half an hour in
// read a clock half an hour behind the browser and the server, and checks
// against their real clock (a sign-in "3386 s old", an agent "connected an hour
// ago") failed while recording. Pinned to its own window's start, each test
// reads the real time it started at record -- and, read back from its mocks,
// the same time at replay. ScopeAck.ClockBase hands it to the harness.
const ScopeStartMetadataKey = "scopeStart"

// FormatScopeStart is how a scope start travels: RFC 3339 in UTC to the
// millisecond, which every harness clock (JavaScript's Date included) parses.
// The zero time formats as "".
func FormatScopeStart(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// StampScopeStart records on m the start of the scope that owned it. An empty
// start leaves m unchanged.
func StampScopeStart(m *Mock, start string) {
	if m == nil || start == "" {
		return
	}
	if m.Spec.Metadata == nil {
		m.Spec.Metadata = map[string]string{}
	}
	m.Spec.Metadata[ScopeStartMetadataKey] = start
}

// ScopeClockBases returns, for each scope in table, the scope start its own
// mocks were recorded under: the value most of them carry (a capture at a
// scope boundary can carry the neighbour's). A scope whose mocks carry none --
// no mocks, or a recording made before the stamp -- is left out.
func ScopeClockBases(table map[string][]string, pools ...[]*Mock) map[string]string {
	start := map[string]string{}
	for _, pool := range pools {
		for _, m := range pool {
			if m == nil {
				continue
			}
			if s := m.Spec.Metadata[ScopeStartMetadataKey]; s != "" {
				start[m.Name] = s
			}
		}
	}
	if len(start) == 0 {
		return nil
	}
	out := map[string]string{}
	for scope, names := range table {
		votes := map[string]int{}
		best, bestN := "", 0
		for _, n := range names {
			s := start[n]
			if s == "" {
				continue
			}
			votes[s]++
			if votes[s] > bestN || (votes[s] == bestN && s < best) {
				best, bestN = s, votes[s]
			}
		}
		if best != "" {
			out[scope] = best
		}
	}
	return out
}
