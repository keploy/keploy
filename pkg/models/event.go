package models

type TeleEvent struct {
	InstallationID string                 `json:"installationId"`
	EventType      string                 `json:"eventType"`
	Meta           map[string]interface{} `json:"meta"`
	CreatedAt      int64                  `json:"createdAt"`
	TeleCheck      bool                   `json:"tele_check"`
}
