package models

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
