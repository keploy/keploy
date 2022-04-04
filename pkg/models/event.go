package models

import (
	"errors"
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Event struct {
	ID             primitive.ObjectID     `bson:"_id"`
	InstallationID string                 `json:"installationID" bson:"installationID,omitempty"`
	EventType      string                 `json:"eventType" bson:"eventType,omitempty"`
	Meta           map[string]interface{} `json:"meta" bson:"meta"`
}

func (e *Event) Bind(r *http.Request) error {
	if e.EventType == "" {
		return errors.New("EventType cant be empty")
	}
	return nil
}
