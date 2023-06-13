package connection

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.uber.org/zap"
)

// Factory is a routine-safe container that holds a trackers with unique ID, and able to create new tracker.
type Factory struct {
	connections         map[structs.ConnID]*Tracker
	inactivityThreshold time.Duration
	mutex               *sync.RWMutex
	respChannel         chan *models.HttpResp
	logger              *zap.Logger
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration, respChannel chan *models.HttpResp, logger *zap.Logger) *Factory {
	return &Factory{
		connections:         make(map[structs.ConnID]*Tracker),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
		respChannel:         respChannel,
		logger:              logger,
	}
}

// func (factory *Factory) HandleReadyConnections(k *keploy.Keploy) {
func (factory *Factory) HandleReadyConnections(db platform.TestCaseDB, getDeps func() []*models.Mock, resetDeps func() int) {

	trackersToDelete := make(map[structs.ConnID]struct{})
	for connID, tracker := range factory.connections {
		if tracker.IsComplete() {
			trackersToDelete[connID] = struct{}{}
			if len(tracker.sentBuf) == 0 && len(tracker.recvBuf) == 0 {
				continue
			}

			parsedHttpReq, err1 := ParseHTTPRequest(tracker.recvBuf)
			parsedHttpRes, err2 := ParseHTTPResponse(tracker.sentBuf, parsedHttpReq)
			if err1 != nil {
				factory.logger.Error("failed to parse the http request from byte array", zap.Error(err1))
				continue
			}
			if err2 != nil {
				factory.logger.Error("failed to parse the http response from byte array", zap.Error(err2))
				continue
			}

			switch models.GetMode() {
			case models.MODE_RECORD:
				// capture the ingress call for record cmd
				capture(db, parsedHttpReq, parsedHttpRes, getDeps, factory.logger)
				// fmt.Println("\nbefore reseting the deps array: ", getDeps())

				resetDeps()
				// fmt.Println("after reseting the deps array: ", getDeps(), "\n ")
			case models.MODE_TEST:
				respBody, err := ioutil.ReadAll(parsedHttpRes.Body)
				if err != nil {
					factory.logger.Error("failed to read the http response body", zap.Error(err), zap.Any("mode", models.MODE_TEST))
					return
				}
				resetDeps()
				factory.respChannel <- &models.HttpResp{
					StatusCode: parsedHttpRes.StatusCode,
					Header:     pkg.ToYamlHttpHeader(parsedHttpRes.Header),
					Body:       string(respBody),
				}
			}

		} else if tracker.Malformed() {
			trackersToDelete[connID] = struct{}{}
		} else if tracker.IsInactive(factory.inactivityThreshold) {
			trackersToDelete[connID] = struct{}{}
		}
	}
	factory.mutex.Lock()
	defer factory.mutex.Unlock()
	for key := range trackersToDelete {
		delete(factory.connections, key)
	}
}

// GetOrCreate returns a tracker that related to the given connection and transaction ids. If there is no such tracker
// we create a new one.
func (factory *Factory) GetOrCreate(connectionID structs.ConnID) *Tracker {
	factory.mutex.Lock()
	defer factory.mutex.Unlock()
	tracker, ok := factory.connections[connectionID]
	if !ok {
		factory.connections[connectionID] = NewTracker(connectionID, factory.logger)
		return factory.connections[connectionID]
	}
	return tracker
}

func capture(db platform.TestCaseDB, req *http.Request, resp *http.Response, getDeps func() []*models.Mock, logger *zap.Logger) {
	// meta := map[string]string{
	// 	"method": req.Method,
	// }
	// httpMock := &models.Mock{
	// 	Version: models.V1Beta2,
	// 	Name:    "",
	// 	Kind:    models.HTTP,
	// }

	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		logger.Error("failed to read the http request body", zap.Error(err))
		return
	}
	// reqBody, err = json.Marshal(reqBody)
	// if err != nil {
	// 	logger.Error("failed to marshal the http request body", zap.Error(err))
	// 	return
	// }
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read the http response body", zap.Error(err))
		return
	}
	// respBody, err = json.Marshal(respBody)
	// if err != nil {
	// 	logger.Error("failed to marshal the http response body", zap.Error(err))
	// 	return
	// }
	// b, err := req.URL.MarshalBinary()
	// if err != nil {
	// 	logger.Error("failed to parse the request URL", zap.Error(err))
	// 	return
	// }

	// encode the message into yaml
	mocks := getDeps()
	mockIds := []string{}
	for i, v := range mocks {
		mockIds = append(mockIds, fmt.Sprintf("%v-%v", v.Name, i))
	}

	// err = httpMock.Spec.Encode(&spec.HttpSpec{
	// 	Metadata: meta,
	// 	Request: spec.HttpReqYaml{
	// 		Method:     spec.Method(req.Method),
	// 		ProtoMajor: req.ProtoMajor,
	// 		ProtoMinor: req.ProtoMinor,
	// 		// URL:        req.URL.String(),
	// 		// URL: fmt.Sprintf("%s://%s%s?%s", req.URL.Scheme, req.Host, req.URL.Path, req.URL.RawQuery),
	// 		URL: fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
	// 		//  URL: string(b),
	// 		Header:    pkg.ToYamlHttpHeader(req.Header),
	// 		Body:      string(reqBody),
	// 		URLParams: pkg.UrlParams(req),
	// 	},
	// 	Response: spec.HttpRespYaml{
	// 		StatusCode: resp.StatusCode,
	// 		Header:     pkg.ToYamlHttpHeader(resp.Header),
	// 		Body:       string(respBody),
	// 	},
	// 	Created:    time.Now().Unix(),
	// 	Assertions: make(map[string][]string),
	// 	// Mocks: mockIds,
	// })
	// if err != nil {
	// 	logger.Error("failed to encode http spec for testcase", zap.Error(err))
	// 	return
	// }
	// write yaml

	// err = db.Insert(httpMock, getDeps())
	err = db.Insert(&models.TestCase{
		Version: models.V1Beta2,
		Name:    "",
		Kind:    models.HTTP,
		Created:    time.Now().Unix(),
		HttpReq: models.HttpReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			// URL:        req.URL.String(),
			// URL: fmt.Sprintf("%s://%s%s?%s", req.URL.Scheme, req.Host, req.URL.Path, req.URL.RawQuery),
			URL: fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			//  URL: string(b),
			Header:    pkg.ToYamlHttpHeader(req.Header),
			Body:      string(reqBody),
			URLParams: pkg.UrlParams(req),
		},
		HttpResp: models.HttpResp{
			StatusCode: resp.StatusCode,
			Header:     pkg.ToYamlHttpHeader(resp.Header),
			Body:       string(respBody),
		},
		Mocks: mocks,
	})
	if err != nil {
		logger.Error("failed to record the ingress requests", zap.Error(err))
		return
	}
}

func ParseHTTPRequest(requestBytes []byte) (*http.Request, error) {

	// Parse the request using the http.ReadRequest function
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBytes)))
	if err != nil {
		return nil, err
	}

	return request, nil
}

func ParseHTTPResponse(data []byte, request *http.Request) (*http.Response, error) {
	buffer := bytes.NewBuffer(data)
	reader := bufio.NewReader(buffer)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	return response, nil
}
