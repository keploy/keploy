//go:build linux

package conn

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
)

var Emoji = "\U0001F430" + " Keploy:"

// Factory is a routine-safe container that holds a trackers with unique ID, and able to create new tracker.
type Factory struct {
	connections         map[ID]*Tracker
	inactivityThreshold time.Duration
	mutex               *sync.RWMutex
	logger              *zap.Logger
	config              *config.Config
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration, logger *zap.Logger, config *config.Config) *Factory {
	return &Factory{
		connections:         make(map[ID]*Tracker),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
		logger:              logger,
		config:              config,
	}
}

// ProcessActiveTrackers iterates over all conn the trackers and checks if they are complete. If so, it captures the ingress call and
// deletes the tracker. If the tracker is inactive for a long time, it deletes it.
func (factory *Factory) ProcessActiveTrackers(ctx context.Context, t chan *models.TestCase, opts models.IncomingOptions) {
	factory.mutex.Lock()
	defer factory.mutex.Unlock()
	var trackersToDelete []ID
	for connID, tracker := range factory.connections {
		select {
		case <-ctx.Done():
			return
		default:
			ok, requestBuf, responseBuf, reqTimestampTest, resTimestampTest := tracker.IsComplete()
			if ok {

				if len(requestBuf) == 0 || len(responseBuf) == 0 {
					factory.logger.Warn("failed processing a request due to invalid request or response", zap.Any("Request Size", len(requestBuf)), zap.Any("Response Size", len(responseBuf)))
					continue
				}
				parsedHTTPReq, err := pkg.ParseHTTPRequest(requestBuf)
				if err != nil {
					utils.LogError(factory.logger, err, "failed to parse the http request from byte array", zap.Any("requestBuf", requestBuf))
					continue
				}
				parsedHTTPRes, err := pkg.ParseHTTPResponse(responseBuf, parsedHTTPReq)
				if err != nil {
					utils.LogError(factory.logger, err, "failed to parse the http response from byte array", zap.Any("responseBuf", responseBuf))
					continue
				}
				basePath := factory.config.Record.BasePath
				parsedBaseURL, err := url.Parse(basePath)
				if err != nil {
					factory.logger.Error("Error parsing base path: %s\n", zap.Error(err))
				}
				baseHost := parsedBaseURL.Host

				if basePath == "" {
					factory.logger.Info("Base path is not set, proceeding with capture")
					Capture(ctx, factory.logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestampTest, resTimestampTest, opts)
					continue
				}
				if parsedHTTPReq.Host == baseHost {
					if strings.HasPrefix(parsedHTTPReq.URL.Path, parsedBaseURL.Path) {
						factory.logger.Info("Capturing test cases for request that matched with base path")
						Capture(ctx, factory.logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestampTest, resTimestampTest, opts)
					} else {
						factory.logger.Info("Skipping capture for request due to mismatch of host from basepath url")
						continue
					}
				} else {
					factory.logger.Info("Skipping capture for request due to mismatch of host from basepath url")
					continue
				}
			} else if tracker.IsInactive(factory.inactivityThreshold) {
				trackersToDelete = append(trackersToDelete, connID)
			}
		}
	}

	// Delete all the processed trackers.
	for _, key := range trackersToDelete {
		delete(factory.connections, key)
	}
}

// GetOrCreate returns a tracker that related to the given conn and transaction ids. If there is no such tracker
// we create a new one.
func (factory *Factory) GetOrCreate(connectionID ID) *Tracker {
	factory.mutex.Lock()
	defer factory.mutex.Unlock()
	tracker, ok := factory.connections[connectionID]
	if !ok {
		factory.connections[connectionID] = NewTracker(connectionID, factory.logger)
		return factory.connections[connectionID]
	}
	return tracker
}
