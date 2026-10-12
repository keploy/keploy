package models

// RecordStartEnv names the variable keploy sets for the command it wraps: the
// time the recording started, RFC 3339 in UTC to the millisecond. A record
// sets it to its own start; a replay sets it to the start of the recording it
// replays, read back from the mocks (RecordStartMetadataKey).
//
// It is there for a test harness that builds values from the clock which end
// up in requests -- generated names such as `e2e-app-${Date.now()}`. A harness
// that freezes its clock at a fixed date makes those names repeatable but puts
// the tests years away from the server's real time, so anything the server
// checks against its clock (a token's expiry, a heartbeat's age, a date picker
// that refuses the past) fails while recording. Anchored here instead, the
// names are the same at record and replay and the clock is the recording's
// own.
const RecordStartEnv = "KEPLOY_RECORD_START"

// RecordStartMetadataKey is the Spec.Metadata key under which every recorded
// mock carries RecordStartEnv's value. It travels inside the mock, so any
// store that keeps mocks whole -- the local files, the registry -- gives a
// replay the start of the recording it pulled.
const RecordStartMetadataKey = "recordStart"

// StampRecordStart records start on m, so a replay of m can hand the same
// start to the test command. An empty start leaves m unchanged.
func StampRecordStart(m *Mock, start string) {
	if m == nil || start == "" {
		return
	}
	if m.Spec.Metadata == nil {
		m.Spec.Metadata = map[string]string{}
	}
	m.Spec.Metadata[RecordStartMetadataKey] = start
}

// RecordStartOf returns the recording start the first stamped mock in pools
// carries, or "" when none is stamped (a recording made before the stamp).
// Every mock of one recording carries the same start.
func RecordStartOf(pools ...[]*Mock) string {
	for _, pool := range pools {
		for _, m := range pool {
			if m == nil {
				continue
			}
			if s := m.Spec.Metadata[RecordStartMetadataKey]; s != "" {
				return s
			}
		}
	}
	return ""
}
