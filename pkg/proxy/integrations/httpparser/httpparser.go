package httpparser

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"time"
	"github.com/agnivade/levenshtein"
	"unicode"


	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

// IsOutgoingHTTP function determines if the outgoing network call is HTTP by comparing the
// message format with that of an HTTP text message.
func IsOutgoingHTTP(buffer []byte) bool {
	return bytes.HasPrefix(buffer[:], []byte("HTTP/")) ||
		bytes.HasPrefix(buffer[:], []byte("GET ")) ||
		bytes.HasPrefix(buffer[:], []byte("POST ")) ||
		bytes.HasPrefix(buffer[:], []byte("PUT ")) ||
		bytes.HasPrefix(buffer[:], []byte("PATCH ")) ||
		bytes.HasPrefix(buffer[:], []byte("DELETE ")) ||
		bytes.HasPrefix(buffer[:], []byte("OPTIONS ")) ||
		bytes.HasPrefix(buffer[:], []byte("HEAD "))
}

func ProcessOutgoingHttp(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		// *deps = append(*deps, encodeOutgoingHttp(requestBuffer,  clientConn,  destConn, logger))
		h.AppendMocks(encodeOutgoingHttp(requestBuffer, clientConn, destConn, logger))
		// h.TestCaseDB.WriteMock(encodeOutgoingHttp(requestBuffer, clientConn, destConn, logger))
	case models.MODE_TEST:
		decodeOutgoingHttp(requestBuffer, clientConn, destConn, h, logger)
	default:
		logger.Info(Emoji+"Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

func findStringMatch(req string, mockString []string) int {
	minDist := int(^uint(0) >> 1) // Initialize with max int value
	bestMatch := -1
	for idx, req := range mockString {
		if !IsAsciiPrintable(mockString[idx]) {
			continue
		}

		dist := levenshtein.ComputeDistance(req, mockString[idx])
		if dist == 0 {
			return 0
		}

		if dist < minDist {
			minDist = dist
			bestMatch = idx
		}
	}
	return bestMatch
}

func HttpDecoder(encoded string) ([]byte, error) {
	// decode the string to a buffer.

	data := []byte(encoded)
	return data, nil
}

func AdaptiveK(length, kMin, kMax, N int) int {
	k := length / N
	if k < kMin {
		return kMin
	} else if k > kMax {
		return kMax
	}
	return k
}

func CreateShingles(data []byte, k int) map[string]struct{} {
	shingles := make(map[string]struct{})
	for i := 0; i < len(data)-k+1; i++ {
		shingle := string(data[i : i+k])
		shingles[shingle] = struct{}{}
	}
	return shingles
}

// JaccardSimilarity computes the Jaccard similarity between two sets of shingles.
func JaccardSimilarity(setA, setB map[string]struct{}) float64 {
	intersectionSize := 0
	for k := range setA {
		if _, exists := setB[k]; exists {
			intersectionSize++
		}
	}

	unionSize := len(setA) + len(setB) - intersectionSize

	if unionSize == 0 {
		return 0.0
	}
	return float64(intersectionSize) / float64(unionSize)
}

func findBinaryMatch(configMocks []*models.Mock, reqBuff []byte, h *hooks.Hook) int {

	mxSim := -1.0
	mxIdx := -1
	// find the fuzzy hash of the mocks
	for idx, mock := range configMocks {
		encoded, _ := HttpDecoder(mock.Spec.HttpReq.Body)
		k := AdaptiveK(len(reqBuff), 3, 8, 5)
		shingles1 := CreateShingles(encoded, k)
		shingles2 := CreateShingles(reqBuff, k)
		similarity := JaccardSimilarity(shingles1, shingles2)
		fmt.Printf("Jaccard Similarity: %f\n", similarity)
		if mxSim < similarity {
			mxSim = similarity
			mxIdx = idx
		}
	}
	return mxIdx
}

func IsAsciiPrintable(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func HttpEncoder(buffer []byte) string {
	//Encode the buffer to string
	encoded := string(buffer)
	return encoded
}
func Fuzzymatch(tcsMocks []*models.Mock, reqBuff []byte, h *hooks.Hook) (bool, string) {
	com := HttpEncoder(reqBuff)
	for idx, mock := range tcsMocks {
		encoded, _ := HttpDecoder(mock.Spec.HttpReq.Body)
		if string(encoded) == string(reqBuff) || mock.Spec.HttpReq.Body == com {
			fmt.Println("matched in first loop")
			tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
			h.SetConfigMocks(tcsMocks)
			return true, mock.Spec.HttpReq.Body
		}
	}
	// convert all the configmocks to string array
	mockString := make([]string, len(tcsMocks))
	for i := 0; i < len(tcsMocks); i++ {
		mockString[i] = string(tcsMocks[i].Spec.HttpReq.Body)
	}
	// find the closest match
	if IsAsciiPrintable(string(reqBuff)) {
		idx := findStringMatch(string(reqBuff), mockString)
		if idx != -1 {
			nMatch := tcsMocks[idx].Spec.HttpReq.Body
			tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
			h.SetConfigMocks(tcsMocks)
			fmt.Println("Returning mock from String Match !!")
			return true, nMatch
		}
	}
	idx := findBinaryMatch(tcsMocks, reqBuff, h)
	if idx != -1 {
		nMatch := tcsMocks[idx].Spec.HttpReq.Body
		tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
		h.SetConfigMocks(tcsMocks)
		return true, nMatch
	}
	return false, ""
}

// decodeOutgoingHttp
func decodeOutgoingHttp(requestBuffer []byte, clienConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	//Matching algorithmm
	//Get the mocks
	tcsMocks := h.GetTcsMocks()
	var bestMatch string
	//Parse the request buffer
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBuffer)))
	if err != nil {
		logger.Error(Emoji+"failed to parse the http request message", zap.Error(err))
		return
	}
	for idx, mock := range tcsMocks {
		//Check if the uri matches
		if mock.Spec.HttpReq.URL != req.URL.String(){
			//If it is not the same, remove the mock
			tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
		}
		//Check if the method matches
		if mock.Spec.HttpReq.Method != models.Method(req.Method){
			//If it is not the same, remove the mock
			tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
		}
		//Check if the header keys match
		for key, _ := range mock.Spec.HttpReq.Header {
			if _, exists := req.Header[key]; !exists {
				//If it is not the same, remove the mock
				tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
			}
		}
		//Check the query params.
		for key, _ := range mock.Spec.HttpReq.URLParams {
			if _, exists := req.URL.Query()[key]; !exists {
				//If it is not the same, remove the mock
				tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
			}
		}
		//Fuzzy matching for the body
		_, bestMatch = Fuzzymatch(tcsMocks, requestBuffer, h)
		}

	tcsMocks= h.GetTcsMocks()
	var bestMatchIndex int
	for idx, mock := range tcsMocks {
		if mock.Spec.HttpReq.Body == bestMatch {
			bestMatchIndex = idx
		}
	}
	if h.GetDepsSize() == 0 {
		// logger.Error("failed to mock the output for unrecorded outgoing http call")
		return
	}

	// var httpSpec spec.HttpSpec
	// err := deps[0].Spec.Decode(&httpSpec)
	// if err != nil {
	// 	logger.Error("failed to decode the yaml spec for the outgoing http call")
	// 	return
	// }
	// httpSpec := deps[0]
	httpSpec := h.FetchDep(bestMatchIndex)
	// fmt.Println("http mock in test: ", httpSpec)

	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", httpSpec.Spec.HttpReq.ProtoMajor, httpSpec.Spec.HttpReq.ProtoMinor, httpSpec.Spec.HttpResp.StatusCode, http.StatusText(int(httpSpec.Spec.HttpResp.StatusCode)))

	// Generate the response headers
	header := pkg.ToHttpHeader(httpSpec.Spec.HttpResp.Header)
	var headers string
	for key, values := range header {
		for _, value := range values {

			headerLine := fmt.Sprintf("%s: %s\r\n", key, value)
			headers += headerLine
		}
	}

	// Generate the response body
	// bodyBytes, _ := ioutil.ReadAll(bytes.NewReader(httpSpec.Response.BodyData))
	// body := string(bodyBytes)
	body := httpSpec.Spec.HttpResp.Body
	// Concatenate the status line, headers, and body
	responseString := statusLine + headers + "\r\n" + body
	_, err = clienConn.Write([]byte(responseString))
	if err != nil {
		logger.Error(Emoji+"failed to write the mock output to the user application", zap.Error(err))
		return
	}
	// pop the mocked output from the dependency queue
	// deps = deps[1:]
	h.PopFront()
}

// encodeOutgoingHttp function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func encodeOutgoingHttp(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) *models.Mock {
	defer destConn.Close()

	// write the request message to the actual destination server
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err))
		return nil
	}

	// read the response from the actual server
	respBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error(Emoji+"failed to read the response message from the destination server", zap.Error(err))
		return nil
	}

	// write the response message to the user client
	_, err = clientConn.Write(respBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write response message to the user client", zap.Error(err))
		return nil
	}

	// converts the request message buffer to http request
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBuffer)))
	if err != nil {
		logger.Error(Emoji+"failed to parse the http request message", zap.Error(err))
		return nil
	}

	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			// TODO right way to log errors
			logger.Error(Emoji+"failed to read the http request body", zap.Error(err))
			return nil
		}
	}

	// converts the response message buffer to http response
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(respBuffer)), req)
	if err != nil {
		logger.Error(Emoji+"failed to parse the http response message", zap.Error(err))
		return nil
	}
	var respBody []byte
	if resp.Body != nil { // Read
		if resp.Header.Get("Content-Encoding") == "gzip" {
			resp.Body, err = gzip.NewReader(resp.Body)
			if err != nil {
				logger.Error(Emoji+"failed to read the the http repsonse body", zap.Error(err))
				return nil
			}
		}
		var err error
		respBody, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			logger.Error(Emoji+"failed to read the the http repsonse body", zap.Error(err))
			return nil
		}
	}

	// store the request and responses as mocks
	meta := map[string]string{
		"name":      "Http",
		"type":      models.HttpClient,
		"operation": req.Method,
	}
	// httpMock := &models.Mock{
	// 	Version: models.V1Beta2,
	// 	Name:    "",
	// 	Kind:    models.HTTP,
	// }

	// // encode the message into yaml
	// err = httpMock.Spec.Encode(&spec.HttpSpec{
	// 		Metadata: meta,
	// 		Request: spec.HttpReqYaml{
	// 			Method:     spec.Method(req.Method),
	// 			ProtoMajor: req.ProtoMajor,
	// 			ProtoMinor: req.ProtoMinor,
	// 			URL:        req.URL.String(),
	// 			Header:     pkg.ToYamlHttpHeader(req.Header),
	// 			Body:       string(reqBody),
	// 			URLParams: pkg.UrlParams(req),
	// 		},
	// 		Response: spec.HttpRespYaml{
	// 			StatusCode: resp.StatusCode,
	// 			Header:     pkg.ToYamlHttpHeader(resp.Header),
	// 			Body: string(respBody),
	// 		},
	// })
	// if err != nil {
	// 	logger.Error("failed to encode the http messsage into the yaml")
	// 	return nil
	// }

	// return httpMock

	return &models.Mock{
		Version: models.V1Beta2,
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: meta,
			HttpReq: &models.HttpReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHttpHeader(req.Header),
				Body:       string(reqBody),
				URLParams:  pkg.UrlParams(req),
			},
			HttpResp: &models.HttpResp{
				StatusCode: resp.StatusCode,
				Header:     pkg.ToYamlHttpHeader(resp.Header),
				Body:       string(respBody),
			},
			Created: time.Now().Unix(),
		},
	}

	// if val, ok := Deps[string(port)]; ok {
	// keploy.Deps = append(keploy.Deps, httpMock)
}
