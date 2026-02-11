package telemetry

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"go.keploy.io/server/v3/pkg/models"
)

func marshalEvent(event models.TeleEvent) ([]byte, error) {
	return json.Marshal(event)
}

func unmarshalResp(resp *http.Response) (string, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var res map[string]string
	if err := json.Unmarshal(body, &res); err != nil {
		return "", err
	}

	id, ok := res["InstallationID"]
	if !ok {
		return "", errors.New("InstallationID not present")
	}
	return id, nil
}
