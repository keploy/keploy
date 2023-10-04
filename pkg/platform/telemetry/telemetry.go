package telemetry

import (
	"bytes"
	"net/http"
	"runtime"
	"time"

	"go.keploy.io/server/pkg/models"
	sentry "github.com/getsentry/sentry-go"
	"github.com/go-git/go-git/v5/plumbing"
	v "github.com/hashicorp/go-version"
	"github.com/go-git/go-git/v5"
	"go.uber.org/zap"
)

func getKeployVersion() string {

	repo, err := git.PlainOpen(".")
	if err != nil {
		return "v0.1.0-dev"
	}

	tagIter, err := repo.Tags()
	if err != nil {
		return "v0.1.0-dev"
	}
	var latestTag string
	var latestTagVersion *v.Version

	err = tagIter.ForEach(func(tagRef *plumbing.Reference) error {
		tagName := tagRef.Name().Short()
		tagVersion, err := v.NewVersion(tagName)
		if err == nil {
			if latestTagVersion == nil || latestTagVersion.LessThan(tagVersion) {
				latestTagVersion = tagVersion
				latestTag = tagName
			}
		}
		return nil
	})

	if err != nil {
		return "v0.1.0-dev"
	}

	return latestTag + "-dev"
}

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	store          FS
	KeployVersion  string
	GlobalMap      map[string]interface{}
	client         *http.Client
}

func NewTelemetry(enabled, offMode bool, store FS, logger *zap.Logger, KeployVersion string, GlobalMap map[string]interface{}) *Telemetry {

	tele := Telemetry{
		Enabled:       enabled,
		OffMode:       offMode,
		logger:        logger,
		store:         store,
		KeployVersion: KeployVersion,
		GlobalMap:     GlobalMap,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
	return &tele
}

func (ac *Telemetry) Ping(isTestMode bool) {
	if !ac.Enabled {
		return
	}

	go func() {
		defer sentry.Recover()
		for {
			var count int64
			var id string

			if ac.Enabled {
				id, _ = ac.store.Get(true)
				count = int64(len(id))
			}
			event := models.TeleEvent{
				EventType: "Ping",
				CreatedAt: time.Now().Unix(),
			}
			ac.InstallationID = id

			if count == 0 {
				//Check in the old keploy folder.
				id, _ = ac.store.Get(false)
					count = int64(len(id))
				if count == 0{
					bin, err := marshalEvent(event, ac.logger)
					if err != nil {
						break
					}
					resp, err := http.Post("https://telemetry.keploy.io/analytics", "application/json", bytes.NewBuffer(bin))
					if err != nil {
						ac.logger.Debug("failed to send request for analytics", zap.Error(err))
						break
					}
					installation_id, err := unmarshalResp(resp, ac.logger)
					if err != nil {
						break
					}
					id = installation_id
				ac.InstallationID = id
				ac.store.Set(id)
				}

			}
			ac.SendTelemetry("Ping")
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (ac *Telemetry) Testrun(success int, failure int) {
	ac.SendTelemetry("TestRun", map[string]interface{}{ "Passed-Tests": success, "Failed-Tests": failure})
}

// func (ac *Telemetry) UnitTestRun(cmd string, success int, failure int) {
// 	ac.SendTelemetry("UnitTestRun", map[string]interface{}{"Cmd": cmd, "Passed-UnitTests": success, "Failed-UnitTests": failure})
// }

// Telemetry event for the Mocking feature test run
func (ac *Telemetry) MockTestRun(utilizedMocks int) {
	ac.SendTelemetry("MockTestRun", map[string]interface{}{ "Utilized-Mocks": utilizedMocks})
}

// Telemetry event for the tests and mocks that are recorded
func (ac *Telemetry) RecordedTestsAndMocks(testSet string, testsTotal int, mockTotal map[string]int) {
	ac.SendTelemetry("RecordedTest", map[string]interface{}{"test-set": testSet, "tests": testsTotal, "mocks": mockTotal})
}

// Telemetry event for the mocks that are recorded in the mocking feature
func (ac *Telemetry) RecordedMock(mockTotal map[string]int) {
	ac.SendTelemetry("RecordedMock", map[string]interface{}{"mocks": mockTotal})
}

func (ac *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {

	if ac.Enabled {
		event := models.TeleEvent{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		event.Meta = make(map[string]interface{})
		if len(output) != 0 {
			event.Meta = output[0]
		}

		if ac.GlobalMap != nil {
			event.Meta["global-map"] = ac.GlobalMap
		}

		if ac.InstallationID == "" {
			id := ""
			id, _ = ac.store.Get(true)
			if id == "" {
				id, _ = ac.store.Get(false)
			}
			ac.InstallationID = id
		}
		event.InstallationID = ac.InstallationID
		event.OS = runtime.GOOS
		if ac.KeployVersion == "" {
			ac.KeployVersion = getKeployVersion()
		}
		event.KeployVersion = ac.KeployVersion
		bin, err := marshalEvent(event, ac.logger)
		if err != nil {
			ac.logger.Debug("failed to marshal event", zap.Error(err))
			return
		}

		req, err := http.NewRequest(http.MethodPost, "https://telemetry.keploy.io/analytics", bytes.NewBuffer(bin))
		if err != nil {
			ac.logger.Debug("failed to create request for analytics", zap.Error(err))
			return
		}

		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		if !ac.OffMode {
			resp, err := ac.client.Do(req)
			if err != nil {
				ac.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			ac.logger.Debug("Sent the event to the telemetry server.")
			unmarshalResp(resp, ac.logger)
			return
		}
		go func() {
			defer sentry.Recover()
			resp, err := ac.client.Do(req)
			if err != nil {
				ac.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, ac.logger)
		}()
	}
}
