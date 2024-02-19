package telemetry

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/proxy/util"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

var teleUrl = "https://telemetry.keploy.io/analytics"

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	store          TelemetryStore
	KeployVersion  string
	GlobalMap      map[string]interface{}
	client         *http.Client
	mutex          sync.RWMutex
}

func NewTelemetry(logger *zap.Logger, enabled, offMode bool, version string, GlobalMap map[string]interface{}) *Telemetry {
	if version == "" {
		version = utils.Version
	}
	tele := Telemetry{
		Enabled:       enabled,
		OffMode:       offMode,
		logger:        logger,
		store:         &Telemetry{},
		KeployVersion: version,
		GlobalMap:     GlobalMap,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
	return &tele
}

func (tele *Telemetry) ExtractInstallationId(isNewConfigPath bool) error {
	var (
		path = fs.FetchHomeDirectory(isNewConfigPath)
	)
	file, err := os.OpenFile(filepath.Join(path, "installation-id.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	tele.mutex.Lock()
	err = decoder.Decode(&tele.InstallationID)
	tele.mutex.Unlock()

	if errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to decode the installation-id yaml. error: %v", err.Error())
	}
	if err != nil {
		return fmt.Errorf("failed to decode the installation-id yaml. error: %v", err.Error())
	}
	return nil
}

func (tele *Telemetry) GenerateTelemetryConfigFile(id string) error {
	path := fs.FetchHomeDirectory(true)
	util.CreateYamlFile(path, "installation-id", tele.logger)

	data := []byte{}

	d, err := yamlLib.Marshal(&id)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	err = os.WriteFile(filepath.Join(path, "installation-id.yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write installation id in yaml file. error: %s", err.Error())
	}

	return nil
}

func (tele *Telemetry) Ping(isTestMode bool) {
	if !tele.Enabled {
		return
	}

	go func() {
		defer utils.HandlePanic()
		for {
			tele.store.ExtractInstallationId(true)

			event := models.TeleEvent{
				EventType: "Ping",
				CreatedAt: time.Now().Unix(),
			}

			if len(tele.InstallationID) == 0 {
				//Check in the old keploy folder.
				tele.store.ExtractInstallationId(false)
				if len(tele.InstallationID) == 0 {
					bin, err := marshalEvent(event, tele.logger)
					if err != nil {
						break
					}
					resp, err := http.Post(teleUrl, "application/json", bytes.NewBuffer(bin))
					if err != nil {
						tele.logger.Debug("failed to send request for analytics", zap.Error(err))
						break
					}
					installation_id, err := unmarshalResp(resp, tele.logger)
					if err != nil {
						break
					}
					tele.mutex.Lock()
					tele.InstallationID = installation_id
					tele.mutex.Unlock()
				}
				tele.store.GenerateTelemetryConfigFile(tele.InstallationID)
			}

			tele.SendTelemetry("Ping")
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (tele *Telemetry) Testrun(success int, failure int) {
	tele.SendTelemetry("TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
}

// func (tele *Telemetry) UnitTestRun(cmd string, success int, failure int) {
// 	tele.SendTelemetry("UnitTestRun", map[string]interface{}{"Cmd": cmd, "Passed-UnitTests": success, "Failed-UnitTests": failure})
// }

// Telemetry event for the Mocking feature test run
func (tele *Telemetry) MockTestRun(utilizedMocks int) {
	tele.SendTelemetry("MockTestRun", map[string]interface{}{"Utilized-Mocks": utilizedMocks})
}

// Telemetry event for the tests and mocks that are recorded
func (tele *Telemetry) RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int) {
	tele.SendTelemetry("RecordedTestSuite", map[string]interface{}{"test-set": testSet, "tests": testsTotal, "mocks": mockTotal})
}

func (tele *Telemetry) RecordedTestAndMocks() {
	tele.SendTelemetry("RecordedTestAndMocks", map[string]interface{}{"mocks": make(map[string]int)})
}

// Telemetry event for the mocks that are recorded in the mocking feature
func (tele *Telemetry) RecordedMocks(mockTotal map[string]int) {
	tele.SendTelemetry("RecordedMocks", map[string]interface{}{"mocks": mockTotal})
}

func (tele *Telemetry) RecordedMock(mockType string) {
	tele.SendTelemetry("RecordedMock", map[string]interface{}{"mock": mockType})
}

func (tele *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {
	go func() {

		if tele.Enabled {
			event := models.TeleEvent{
				EventType: eventType,
				CreatedAt: time.Now().Unix(),
			}
			event.Meta = make(map[string]interface{})
			if len(output) != 0 {
				event.Meta = output[0]
			}

			if tele.GlobalMap != nil {
				event.Meta["global-map"] = tele.GlobalMap
			}
			tele.mutex.Lock()
			if tele.InstallationID == "" {
				tele.store.ExtractInstallationId(true)
				if tele.InstallationID == "" {
					tele.store.ExtractInstallationId(false)
				}
			}
			event.InstallationID = tele.InstallationID
			tele.mutex.Unlock()
			event.OS = runtime.GOOS
			event.KeployVersion = tele.KeployVersion
			event.Arch = runtime.GOARCH
			bin, err := marshalEvent(event, tele.logger)
			if err != nil {
				tele.logger.Debug("failed to marshal event", zap.Error(err))
				return
			}

			req, err := http.NewRequest(http.MethodPost, teleUrl, bytes.NewBuffer(bin))
			if err != nil {
				tele.logger.Debug("failed to create request for analytics", zap.Error(err))
				return
			}

			req.Header.Set("Content-Type", "application/json; charset=utf-8")

			defer utils.HandlePanic()
			resp, err := tele.client.Do(req)
			if err != nil {
				tele.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, tele.logger)
		}
	}()
}
