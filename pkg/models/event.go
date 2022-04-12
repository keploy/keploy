package models

import (
	"errors"
	"net/http"
)

type Event struct {
	InstallationID string                 `json:"installationId"`
	EventType      string                 `json:"eventType"`
	Meta           map[string]interface{} `json:"meta"`
	CreatedAt      int64                  `json:"createdAt"`
}

func (e *Event) Bind(r *http.Request) error {
	if e.EventType == "" {
		return errors.New("EventType cant be empty")
	}
	return nil
}
