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

	factory.logger.Debug("Processing active trackers",
		zap.Int("number_of_connections", len(factory.connections)))

	for connID, tracker := range factory.connections {
		select {
		case <-ctx.Done():
			return
		default:
			// For gRPC (HTTP2) requests, handle them natively
			if tracker.protocol == HTTP2 {
				// Get the completed stream
				stream := tracker.getHTTP2CompletedStream()
				if stream != nil {
					// Skip outgoing requests (internal service-to-service calls) and HTTP gateway requests
					if isInternalConnection(connID) || stream.IsOutgoing || pkg.IsHTTPGatewayRequest(stream) {
						factory.logger.Debug("Skipping internal/outgoing/gateway gRPC connection", zap.Any("stream", stream))
						continue
					}

					factory.logger.Debug("Processing HTTP2/gRPC request",
						zap.Any("connection_id", connID))

					// Get timestamps from the stream
					CaptureGRPC(ctx, factory.logger, t, stream, stream.StartTime, stream.EndTime)
				}
			} else {
				// Handle HTTP1 requests
				ok, requestBuf, responseBuf, reqTimestampTest, resTimestampTest := tracker.isHTTP1Complete()
				if ok {
					factory.logger.Debug("Found complete HTTP request/response",
						zap.Any("connection_id", connID),
						zap.String("protocol", string(tracker.protocol)),
						zap.Int("request_buffer_size", len(requestBuf)),
						zap.Int("response_buffer_size", len(responseBuf)))

					// Skip outgoing requests (internal service-to-service calls)
					if isInternalConnection(connID) {
						factory.logger.Debug("Skipping internal connection")
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
	}

	// Delete all the processed trackers.
	if len(trackersToDelete) > 0 {
		factory.logger.Debug("Deleting inactive trackers",
			zap.Int("count", len(trackersToDelete)))
	}
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
		tracker = NewTracker(connectionID, factory.logger)
		factory.connections[connectionID] = tracker
	}
	return tracker
}
