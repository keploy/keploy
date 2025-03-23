//go:build linux

package conn

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

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
	incomingOpts        models.IncomingOptions
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration, logger *zap.Logger, opts models.IncomingOptions) *Factory {
	return &Factory{
		connections:         make(map[ID]*Tracker),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
		logger:              logger,
		incomingOpts:        opts,
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
				// Skip if this is an idempotency check request
				if isIdempotencyCheck(requestBuf) {
					trackersToDelete = append(trackersToDelete, connID)
					continue
				}
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
				basePath := factory.incomingOpts.BasePath
				parsedBaseURL, err := url.Parse(basePath)
				if err != nil {
					factory.logger.Error("Error parsing base path: %s\n", zap.Error(err))
				}
				baseHost := parsedBaseURL.Host

				if len(strings.TrimSpace(basePath)) == 0 {
					factory.logger.Debug("Base path is not set, proceeding with request capture",
						zap.String("basePath", basePath),
					)
					Capture(ctx, factory.logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestampTest, resTimestampTest, opts)
					continue
				}

				if parsedHTTPReq.Host != baseHost {
					factory.logger.Info("Skipping capture due to host mismatch",
						zap.String("expectedHost", baseHost),
						zap.String("actualHost", parsedHTTPReq.Host),
					)
					continue
				}

				if !strings.HasPrefix(parsedHTTPReq.URL.Path, parsedBaseURL.Path) {
					factory.logger.Info("Skipping capture due to base path mismatch",
						zap.String("expectedBasePath", parsedBaseURL.Path),
						zap.String("actualPath", parsedHTTPReq.URL.Path),
					)
					continue
				}

				factory.logger.Info("Capturing test case for request matching base path",
					zap.String("host", parsedHTTPReq.Host),
					zap.String("path", parsedHTTPReq.URL.Path),
				)

				Capture(ctx, factory.logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestampTest, resTimestampTest, opts)

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

// Helper function to check if a request is an idempotency check
func isIdempotencyCheck(requestBuf []byte) bool {
	req, err := pkg.ParseHTTPRequest(requestBuf)
	if err != nil {
		return false
	}
	return req.Header.Get("Idempotency-Check") == "true"
}
