package models

// Shared event-type string constants used by the telemetry emitter
// (pkg/platform/telemetry). Only the event names introduced or
// modified by this PR are extracted as constants. Pre-existing event
// names (Ping, RecordedTestSuite, RecordedBatch, TestSetRun,
// MockTestRun, RecordedMocks) remain inline string literals at their
// existing call sites to avoid churn in code paths this PR does not
// touch.
const (
	TeleEventTestRun                = "TestRun"
	TeleEventRecordSessionCompleted = "RecordSessionCompleted"
)

type TeleEvent struct {
	InstallationID string                 `json:"installationId"`
	EventType      string                 `json:"eventType"`
	Meta           map[string]interface{} `json:"meta"`
	CreatedAt      int64                  `json:"createdAt"`
	TeleCheck      bool                   `json:"tele_check"`
	OS             string                 `json:"os"`
	KeployVersion  string                 `json:"keploy_version"`
	Arch           string                 `json:"arch"`
	IsCI           bool                   `json:"is_ci"`
	CIProvider     string                 `json:"ci_provider,omitempty"`
	GitRepo        string                 `json:"git_repo,omitempty"`
}
