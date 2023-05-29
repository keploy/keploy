package connection

import (
	"fmt"
	// "os"
	"sync"
	"time"

	keploy "go.keploy.io/server/pkg/hooks/keploy"
	"go.keploy.io/server/pkg/hooks/structs"
	// "go.keploy.io/server/pkg/models"
)

// Factory is a routine-safe container that holds a trackers with unique ID, and able to create new tracker.
type Factory struct {
	connections         map[structs.ConnID]*Tracker
	inactivityThreshold time.Duration
	mutex               *sync.RWMutex
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration) *Factory {
	return &Factory{
		connections:         make(map[structs.ConnID]*Tracker),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
	}
}
func (factory *Factory) HandleReadyConnections(k *keploy.Keploy) {
	trackersToDelete := make(map[structs.ConnID]struct{})
	for connID, tracker := range factory.connections {
		if tracker.IsComplete() {
			trackersToDelete[connID] = struct{}{}
			if len(tracker.sentBuf) == 0 && len(tracker.recvBuf) == 0 {
				continue
			}
			// fmt.Printf(“========================>\nFound HTTP payload\nRequest->\n%s\n\nResponse->\n%s\n\n<========================\n”, tracker.recvBuf, tracker.sentBuf)
			fmt.Printf("\n======\nSize of req:[%v] || res[%v]\n======", len(tracker.recvBuf), len(tracker.sentBuf))
			// keploy logic------->
			parsedHttpReq, err1 := keploy.ParseHTTPRequest(tracker.recvBuf)
			parsedHttpRes, err2 := keploy.ParseHTTPResponse(tracker.sentBuf, parsedHttpReq)
			if err1 != nil {
				fmt.Println("unable to parse http request from byte[]", err1)
				continue
			}
			if err2 != nil {
				fmt.Println("unable to parse http response from byte[]", err2)
				continue
			}
			// port, err := keploy.ExtractPortFromHost(parsedHttpReq.Host)
			// if err != nil {
			//  fmt.Println(“unable to get port from request:“, err)
			// }

			// port := os.Getenv("PORT")
			// fmt.Println("PORT:", port)
			// if port == "" {
			// 	return
			// }
			// host := os.Getenv("HOST")
			// fmt.Println("HOST:", host)
			// if host == "" {
			// 	return
			// }
			// tPath := os.Getenv("KEPLOY_TEST_PATH")
			// fmt.Println("KEPLOY_TEST_PATH:", tPath)
			// if tPath == "" {
			// 	return
			// }

			// mPath := os.Getenv("KEPLOY_MOCK_PATH")
			// fmt.Println("KEPLOY_MOCK_PATH:", mPath)
			// if mPath == "" {
			// 	return
			// }

			// cfg := keploy.Config{
			// 	App: keploy.AppConfig{
			// 		Port:     port,
			// 		Host:     host,
			// 		TestPath: tPath,
			// 		MockPath: mPath,
			// 	},
			// }
			// k := keploy.New(cfg)
			// k := keploy.KeployInitializer()
			if parsedHttpReq.Header.Get("KEPLOY_TEST_ID") != "" && keploy.GetMode() == keploy.MODE_TEST {
				// id := parsedHttpReq.Header.Get("KEPLOY_TEST_ID")
				// resBody, err2 := keploy.GetResponseBody(parsedHttpRes)

				// if err2 != nil {
				// 	fmt.Println("unable to extract response body:", err2)
				// 	return
				// }
				// httpResp := models.HttpResp{
				// 	StatusCode:    parsedHttpRes.StatusCode,
				// 	StatusMessage: parsedHttpRes.Status,
				// 	ProtoMajor:    parsedHttpRes.ProtoMajor,
				// 	ProtoMinor:    parsedHttpRes.ProtoMinor,
				// 	Header:        parsedHttpRes.Header,
				// 	Body:          resBody,
				// }
				// k.PutResp(id, httpResp)
				// fmt.Println("sent a response for simulate before putting to channel", " id: ", id, " resp: ", httpResp)
				// keploy.HoldSimulate <- true
				// println("sent a response for simulate")
				continue
			}

			if keploy.GetMode() == keploy.MODE_TEST {
				continue
			}
			// fmt.Println(“Keploy instance ready for capturing test case:“, k)
			fmt.Println("Keploy mode in HandleReadyConnections:", keploy.GetMode())
			if keploy.GetMode() == keploy.MODE_RECORD {
				println("parsed request: ", parsedHttpReq, " and response: ", parsedHttpRes)
				// keploy.CaptureHttpTC(k, parsedHttpReq, parsedHttpRes)
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
		factory.connections[connectionID] = NewTracker(connectionID)
		// println(“created new tracker...“)
		return factory.connections[connectionID]
	}
	// else {
	//  println(“tracker already exists...“)
	// }
	return tracker
}
