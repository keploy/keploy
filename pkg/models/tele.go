package models

import "sync"

type TeleEvent struct {
	InstallationID string    `json:"installationId"`
	EventType      string    `json:"eventType"`
	Meta           *sync.Map `json:"meta"`
	CreatedAt      int64     `json:"createdAt"`
	TeleCheck      bool      `json:"tele_check"`
	OS             string    `json:"os"`
	KeployVersion  string    `json:"keploy_version"`
	Arch           string    `json:"arch"`
}
