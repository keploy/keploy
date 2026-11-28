// Package models provides data models for the keploy.
package models

type TestSet struct {
	PreScript  string                 `json:"pre_script" bson:"pre_script" yaml:"preScript"`
	PostScript string                 `json:"post_script" bson:"post_script" yaml:"postScript"`
	AppCommand string                 `json:"app_command" bson:"app_command" yaml:"appCommand"`
	Template   map[string]interface{} `json:"template" bson:"template" yaml:"template"`
	Secret     map[string]interface{} `json:"secret" bson:"secret" yaml:"secret,omitempty"`
	Metadata   map[string]interface{} `json:"metadata" bson:"metadata" yaml:"metadata"`
	// RecordStopKilled, under the proven switch, says why keploy record had
	// to kill the application it stopped at the recording's end: what the
	// application did as it stopped is not in the recording, so a replay
	// that makes such a call cannot judge it (P101-b-k). Empty when the
	// stop did not kill, and always with the switch off.
	RecordStopKilled string `json:"record_stop_killed,omitempty" bson:"record_stop_killed,omitempty" yaml:"recordStopKilled,omitempty"`
	// RecordMocksDropped says the recording's agent dropped mocks after it
	// had captured them (memory pressure, its mock output): what the agent's
	// summary of the session said of them (StreamEndSummary.MocksDropped).
	// The recording does not hold them, so a replay may find no mock for a
	// call that needs one. Nil when the agent dropped none, or sent no
	// summary (an agent older than it).
	RecordMocksDropped *MockDropSummary `json:"record_mocks_dropped,omitempty" bson:"record_mocks_dropped,omitempty" yaml:"recordMocksDropped,omitempty"`
	// RecordedWindows is the immutable [request, response] schedule of every
	// test case recorded for this test set, persisted at record time so tests
	// deleted later (dedup, --must-pass, auto-replay) remain accounted for when
	// deriving outside-window background mocks.
	RecordedWindows []TestWindow `json:"recorded_windows,omitempty" bson:"recorded_windows,omitempty" yaml:"recorded_windows,omitempty"`
}

// Secret interface for types that support secret configuration.
type Secret interface {
	SetSecrets(secrets map[string]interface{})
}

func (ts *TestSet) SetSecrets(secrets map[string]interface{}) {
	ts.Secret = secrets
}

func (ts *TestSet) WithoutSecrets() *TestSet {
	if ts == nil {
		return &TestSet{}
	}

	testSetCopy := *ts
	testSetCopy.Secret = nil
	return &testSetCopy
}

type Plan struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	KUnits int    `json:"kunits,omitempty"`
}
