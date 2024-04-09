package conn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	// "text/template/parse"
	"go.uber.org/zap"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
)

var Emoji = "\U0001F430" + " Keploy:"

// Factory is a routine-safe container that holds a trackers with unique ID, and able to create new tracker.
type Factory struct {
	connections         map[ID]chan SocketDataEvent
	inactivityThreshold time.Duration
	mutex               *sync.RWMutex
	logger              *zap.Logger
	t                   chan *models.TestCase
	mu                  sync.Mutex
	workers             int
	connectionQueue     chan ID
	EventChan           chan Event
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration, logger *zap.Logger, t chan *models.TestCase, event chan Event) *Factory {
	return &Factory{
		connections:         make(map[ID]chan SocketDataEvent),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
		logger:              logger,
		t:                   t,
		workers:             10,
		connectionQueue:     make(chan ID, 100),
		EventChan:           event,
	}
}

// ProcessActiveTrackers iterates over all conn the trackers and checks if they are complete. If so, it captures the ingress call and
// deletes the tracker. If the tracker is inactive for a long time, it deletes it.
func (factory *Factory) ProcessActiveTrackers(ctx context.Context, t chan *models.TestCase, event Event) {
	// Check the type of event.
	var connectionId ID
	switch event.Type {
	case "open":
		connectionId = event.Msg.OpenEvent.ConnID
		factory.mu.Lock()
		if factory.connections[connectionId] != nil {
			return
		}
		workerChan := make(chan SocketDataEvent, 1000)
		factory.connections[connectionId] = workerChan
		fmt.Println("This is the connection Id", connectionId)
		go factory.Worker(ctx, t, workerChan)
		factory.mu.Unlock()
		fmt.Println("successfully created the go routine for the connection id ", connectionId)
	case "data":
		// fmt.Println("got a data event")
		factory.mu.Lock()
		connectionId = event.Msg.DataEvent.ConnID
		fmt.Println("Connection id for the channel", connectionId)
		factory.mu.Unlock()
		if factory.connections[connectionId] == nil {
			println("got a nil channel")
			workerChan := make(chan SocketDataEvent, 1000)
			factory.connections[connectionId] = workerChan
			go factory.Worker(ctx, t, workerChan)
			workerChan <- *event.Msg.DataEvent
			return
		}
		factory.connections[connectionId] <- *event.Msg.DataEvent
	case "close":
		connectionId = event.Msg.CloseEvent.ConnID
		close(factory.connections[connectionId])
		delete(factory.connections, connectionId)
	}
}

func (factory *Factory) Worker(ctx context.Context, t chan *models.TestCase, workerChan chan SocketDataEvent) {
	var lastEventType TrafficDirectionEnum = -1
	var lastEvent SocketDataEvent
	var twoSecondTimer = time.NewTimer(2 * time.Second)
	var req []byte
	var res []byte
	for {
		select {
		case dataEvent := <-workerChan:
			fmt.Println("got a data event")
			switch dataEvent.Direction {
			case IngressTraffic:
				initializedLength := bytes.IndexByte(dataEvent.Msg[:], 0)
				req = append(req, dataEvent.Msg[:initializedLength]...)
				// fmt.Println("This is the req", string(req))
			case EgressTraffic:
				initializedLength := bytes.IndexByte(dataEvent.Msg[:], 0)
				res = append(res, dataEvent.Msg[:initializedLength]...)
			}
			if dataEvent.Direction == IngressTraffic && lastEventType == EgressTraffic {
				// This means that the testcase is ready to be recorded.
				parseTc(ctx, t, factory, req, res, lastEvent)
				req = []byte{}
				res = []byte{}
			}
			lastEventType = dataEvent.Direction
			lastEvent = dataEvent
			twoSecondTimer.Reset(2 * time.Second)
		case <-twoSecondTimer.C:
			fmt.Println("The direction of the last event type is", lastEventType, lastEvent.ConnID)
			if lastEventType == EgressTraffic {
				// We expect the response to be complete now.
				parseTc(ctx, t, factory, req, res, lastEvent)
				req = []byte{}
				res = []byte{}
			}
			lastEventType = -1
		case <-time.After(4 * time.Second):
			// This means that the connection is inactive for a long time.
			// We should close the connection.
			fmt.Println("Closing the connection", lastEvent.ConnID)
			return
		case <-ctx.Done():
			return
		}
	}
}

func parseTc(ctx context.Context, t chan *models.TestCase, factory *Factory, req []byte, res []byte, lastEvent SocketDataEvent) {
	parsedHTTPReq, err := pkg.ParseHTTPRequest(req)
	if err != nil {
		factory.logger.Error("failed to parse the http request from byte array", zap.Any("request", string(req)))
		fmt.Println("This is the last event conn id", lastEvent.ConnID)
		return
	}
	parsedHTTPRes, err := pkg.ParseHTTPResponse(res, parsedHTTPReq)
	if err != nil {
		factory.logger.Error("failed to parse the http response from byte array", zap.Any("response", string(res)))
		fmt.Println("This is the last event conn id", lastEvent.ConnID)
		return
	}

	fmt.Println("locked the factory")
	capture(context.Background(), factory.logger, t, parsedHTTPReq, parsedHTTPRes, time.Now(), time.Now())

}

// GetOrCreate returns a tracker that related to the given conn and transaction ids. If there is no such tracker
// we create a new one.
// func (factory *Factory) GetOrCreate(connectionID ID) *Tracker {
// 	factory.mutex.Lock()
// 	defer factory.mutex.Unlock()
// 	tracker, ok := factory.connections[connectionID]
// 	if !ok {
// 		// factory.connections[connectionID] = NewTracker(connectionID, factory.logger)
// 		factory.connectionQueue <- connectionID
// 		if factory.workers < 0 {
// 			fmt.Println("waiting for the worker to be free", factory.workers)
// 			time.Sleep(2 * time.Second)
// 		}
// 		go func() {
// 			connectionIDP := <-factory.connectionQueue
// 			factory.mu.Lock()
// 			factory.workers--
// 			factory.mu.Unlock()
// 			close := false
// 			// timeout := time.After(5 * time.Second)
// 			var lastEventType TrafficDirectionEnum // need a value for this variable
// 			var reqBytes, respBytes []byte
// 			lastEventType = -1
// 			fmt.Println("This is the last event type", lastEventType)
// 			for {
// 				select {
// 				case event := <-factory.connections[connectionIDP].eventChannel.DataChan:
// 					fmt.Println("This is the event direction", event.Direction)
// 					fmt.Println("This is the len of user reqs", len(factory.connections[connectionIDP].userReqs))
// 					fmt.Println("This is the len of the user resps", len(factory.connections[connectionIDP].userResps))
// 					if event.Direction == EgressTraffic {
// 						reqBytes = factory.connections[connectionIDP].userReqs[0]
// 						respBytes = factory.connections[connectionIDP].resp
// 					}
// 					if lastEventType == EgressTraffic && event.Direction == IngressTraffic {
// 						// This means that we have received the response for the request.
// 						// Parse the request and the response.
// 						fmt.Println("This is the len of user reqs", factory.connections[connectionIDP].req)
// 						// fmt.Println("Request Buf", string(requestBuf))
// 					}
// 					lastEventType = event.Direction

// 				case <-factory.connections[connectionIDP].eventChannel.CloseChan:
// 					close = true
// 				case <-time.After(2 * time.Second):
// 					fmt.Println("Processing after 2 seconds", lastEventType)
// 					if lastEventType == EgressTraffic {
// 						// We expect the response to be completed in the 2 sec
// 						parsedHTTPReq, err := pkg.ParseHTTPRequest(reqBytes)
// 						if err != nil {
// 							factory.logger.Error("failed to parse the http request from byte array", zap.Any("request bytes", reqBytes))
// 						}
// 						parsedHttpRes, err := pkg.ParseHTTPResponse(respBytes, parsedHTTPReq)
// 						if err != nil {
// 							factory.logger.Error("failed to parse the http response from byte array", zap.Any("resp bytes", respBytes))
// 						}
// 						factory.mu.Lock()
// 						// fmt.Println("This is the request and the response", string(reqBytes), string(respBytes))
// 						capture(context.Background(), factory.logger, factory.t, parsedHTTPReq, parsedHttpRes, time.Now(), time.Now())
// 						factory.mu.Unlock()
// 						lastEventType = -1
// 					}
// 					fmt.Println("This is the value of close", close)
// 					if close {
// 						fmt.Println("Closing the connection")
// 						factory.mu.Lock()
// 						factory.workers++
// 						factory.mu.Unlock()
// 						fmt.Println("This is the value of workers", factory.workers)
// 						return
// 					}
// 				case <-time.After(3 * time.Second):
// 					fmt.Println("This is the last event type", lastEventType)
// 					fmt.Println("We are now exiting the loop")
// 					factory.mu.Lock()
// 					factory.workers++
// 					factory.mu.Unlock()
// 					fmt.Println("This is the value of workers after 3 seconds", factory.workers)
// 					return
// 					// case <-ctx.Done(): // TODO: Add condition for this.
// 					// 	return
// 				}
// 			}
// 		}()
// 		return factory.connections[connectionID]
// 	}
// 	return tracker
// }

func capture(_ context.Context, logger *zap.Logger, t chan *models.TestCase, req *http.Request, resp *http.Response, reqTimeTest time.Time, resTimeTest time.Time) {
	fmt.Println("capturing the testcase now")
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
	tc := &models.TestCase{
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
	t <- tc
	if err != nil {
		utils.LogError(logger, err, "failed to record the ingress requests")
		return
	}
}
