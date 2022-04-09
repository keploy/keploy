package models

import (
	"errors"
	"net/http"
)

type Event struct {
	InstallationID string                 `json:"installationId" bson:"installationId,omitempty"`
	EventType      string                 `json:"eventType" bson:"eventType,omitempty"`
	Meta           map[string]interface{} `json:"meta" bson:"meta"`
}

func (e *Event) Bind(r *http.Request) error {
	if e.EventType == "" {
		return errors.New("EventType cant be empty")
	}
	return nil
}
