package telemetry

import (
	"encoding/json"

	"go.keploy.io/server/v3/pkg/models"
)

func marshalEvent(event models.TeleEvent) ([]byte, error) {
	return json.Marshal(event)
}
