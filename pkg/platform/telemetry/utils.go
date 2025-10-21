package telemetry

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func marshalEvent(event models.TeleEvent, log *zap.Logger) (bin []byte, err error) {

	bin, err = json.Marshal(event)
	if err != nil {
		log.Fatal("failed to marshal event struct into json", zap.Error(err))
	}
	return
}

func unmarshalResp(resp *http.Response, log *zap.Logger) (id string, err error) {

	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			log.Debug("failed to close connecton reader", zap.String("url", "https://telemetry.keploy.io/analytics"), zap.Error(err))
			return
		}
	}(resp.Body)

	var res map[string]string
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debug("failed to read response from telemetry server", zap.String("url", "https://telemetry.keploy.io/analytics"), zap.Error(err))
		return
	}

	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Debug("failed to read testcases from telemetry server", zap.Error(err))
		return
	}

	id, ok := res["InstallationID"]
	if !ok {
		log.Debug("InstallationID not present")
		err = errors.New("InstallationID not present")
		return
	}
	return
}
