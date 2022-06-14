package telemetry

import (
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func marshalEvent(event models.Event, log *zap.Logger) (bin []byte, err error) {
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
			log.Error("failed to close connecton reader", zap.String("url", "http://localhost:3030/analytics"), zap.Error(err))
			return
		}
	}(resp.Body)
	var res map[string]string
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error("failed to read response from telemetry server", zap.String("url", "http://localhost:3030/analytics"), zap.Error(err))
		return
	}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Error("failed to read testcases from telemetry server", zap.Error(err))
		return
	}
	id, ok := res["InstallationID"]
	if !ok {
		log.Error("InstallationID not present")
		err = errors.New("InstallationID not present")
		return
	}
	return
}
