package conn

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration, logger *zap.Logger) *Factory {
	return &Factory{
		connections:         make(map[ID]*Tracker),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
		logger:              logger,
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
				capture(ctx, factory.logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestampTest, resTimestampTest, opts)

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

func capture(_ context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time, opts models.IncomingOptions) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http request body")
		return
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the http response body")
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.LogError(logger, err, "failed to read the http response body")
		return
	}

	if isFiltered(logger, req, opts) {
		logger.Debug("The request is a filtered request")
		return
	}

	t <- &models.TestCase{
		Version: models.GetVersion(),
		Name:    pkg.ToYamlHTTPHeader(req.Header)["Keploy-Test-Name"],
		Kind:    models.HTTP,
		Created: time.Now().Unix(),
		HTTPReq: models.HTTPReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			// URL:        req.URL.String(),
			// URL: fmt.Sprintf("%s://%s%s?%s", req.URL.Scheme, req.Host, req.URL.Path, req.URL.RawQuery),
			URL: fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			//  URL: string(b),
			Header:    pkg.ToYamlHTTPHeader(req.Header),
			Body:      string(reqBody),
			URLParams: pkg.URLParams(req),
			Timestamp: reqTimeTest,
		},
		HTTPResp: models.HTTPResp{
			StatusCode:    resp.StatusCode,
			Header:        pkg.ToYamlHTTPHeader(resp.Header),
			Body:          string(respBody),
			Timestamp:     resTimeTest,
			StatusMessage: http.StatusText(resp.StatusCode),
		},
		Noise: map[string][]string{},
		// Mocks: mocks,
	}
}
