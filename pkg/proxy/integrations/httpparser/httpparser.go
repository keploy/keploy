package httpparser

import (
	"bufio"
	"bytes"
	"io"
	"io/ioutil"
	"net"
	"net/http"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/models/spec"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

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

// CaptureHTTPMessage function parses the HTTP request and response text messages to capture outgoing network calls as mocks.
func CaptureHTTPMessage(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) *models.Mock {
	defer destConn.Close()

	// write the request message to the actual destination server
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return nil
	}

	// read the response from the actual server
	respBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read the response message from the destination server", zap.Error(err))
		return nil
	}

	// write the response message to the user client
	_, err = clientConn.Write(respBuffer)
	if err != nil {
		logger.Error("failed to write response message to the user client", zap.Error(err))
		return nil
	}

	// converts the request message buffer to http request
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(requestBuffer)))
	if err != nil {
		logger.Error("failed to parse the http request message", zap.Error(err))
		return nil
	}

	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			// TODO right way to log errors
			logger.Error("failed to read the http request body", zap.Error(err))
			return nil
		}
	}

	// converts the response message buffer to http response
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(respBuffer)), req)
	if err != nil {
		logger.Error("failed to parse the http response message", zap.Error(err))
		return nil
	}
	var respBody []byte
	if resp.Body != nil { // Read
		var err error
		respBody, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			logger.Error("failed to read the the http repsonse body", zap.Error(err))
			return nil
		}
	}

	// store the request and responses as mocks
	meta := map[string]string{
		"name":      "Http",
		"type":      models.HttpClient,
		"operation": req.Method,
	}
	httpMock := &models.Mock{
		Version: models.V1Beta2,
		Name:    "",
		Kind:    models.HTTP,
	}

	// encode the message into yaml
	err = httpMock.Spec.Encode(&spec.HttpSpec{
			Metadata: meta,
			Request: spec.HttpReqYaml{
				Method:     spec.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHttpHeader(req.Header),
				Body:       string(reqBody),
				URLParams: pkg.UrlParams(req),
			},
			Response: spec.HttpRespYaml{
				StatusCode: resp.StatusCode,
				Header:     pkg.ToYamlHttpHeader(resp.Header),
				Body: string(respBody),
			},
	})
	if err != nil {
		logger.Error("failed to encode the http messsage into the yaml")
		return nil
	}
	return httpMock

		// if val, ok := Deps[string(port)]; ok {
		// keploy.Deps = append(keploy.Deps, httpMock)
}
