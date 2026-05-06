package models


// Event-type names emitted by this package. Only the names this file's
// methods actually emit are listed; existing pre-existing emissions
// (Ping, RecordedTestSuite, RecordedBatch, TestSetRun, MockTestRun,
// RecordedMocks) keep their original string literals to avoid churn in
// callers we did not touch.
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
