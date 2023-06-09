package pkg

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/araddon/dateparse"
	"github.com/gorilla/mux"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/models/spec"
	"go.uber.org/zap"
)

// UrlParams returns the Url and Query parameters from the request url.
func UrlParams(r *http.Request) map[string]string {
	params := mux.Vars(r)

	result := params
	qp := r.URL.Query()
	for i, j := range qp {
		var s string
		if _, ok := result[i]; ok {
			s = result[i]
		}
		for _, e := range j {
			if s != "" {
				s += ", " + e
			} else {
				s = e
			}
		}
		result[i] = s
	}
	return result
}

// ToYamlHttpHeader converts the http header into yaml format
func ToYamlHttpHeader(httpHeader http.Header) map[string]string {
	header := map[string]string{}
	for i, j := range httpHeader {
		header[i] = strings.Join(j, ",")
	}
	return header
}

func ToHttpHeader(mockHeader map[string]string) http.Header {
	header := http.Header{}
	for i, j := range mockHeader {
		match := IsTime(j)
		if match {
			//Values like "Tue, 17 Jan 2023 16:34:58 IST" should be considered as single element
			header[i] = []string{j}
			continue
		}
		header[i] = strings.Split(j, ",")
	}
	return header
}


// IsTime verifies whether a given string represents a valid date or not.
func IsTime(stringDate string) bool {
	s := strings.TrimSpace(stringDate)
	_, err := dateparse.ParseAny(s)
	return err == nil
}


func SimulateHttp (tc models.Mock, httpSpec *spec.HttpSpec, logger *zap.Logger, getResp func() *spec.HttpRespYaml) (*spec.HttpRespYaml, error) {
	resp := &spec.HttpRespYaml{}

	// httpSpec := &spec.HttpSpec{}
	// err := tc.Spec.Decode(httpSpec)
	// if err!=nil {
	// 	logger.Error("failed to unmarshal yaml doc for simulation of http request", zap.Error(err))
	// 	return nil, err
	// }
	req, err := http.NewRequest(string(httpSpec.Request.Method), httpSpec.Request.URL, bytes.NewBufferString(httpSpec.Request.Body))
	if err != nil {
		logger.Error("failed to create a http request from the yaml document", zap.Error(err))
		return nil, err
	}
	req.Header = ToHttpHeader(httpSpec.Request.Header)
	req.Header.Set("KEPLOY_TEST_ID", tc.Name)
	req.ProtoMajor = httpSpec.Request.ProtoMajor
	req.ProtoMinor = httpSpec.Request.ProtoMinor
	req.Close = true

	// httpresp, err := k.client.Do(req)
	client := &http.Client{}
	client.Do(req)
	if err != nil {
		logger.Error("failed sending testcase request to app", zap.Error(err))
		return nil, err
	}

	// get the response from the hooks
	resp = getResp()

	// defer httpresp.Body.Close()
	// println("before blocking simulate")

	return resp, nil
}