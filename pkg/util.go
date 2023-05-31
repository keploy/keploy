package pkg

import (
	"net/http"
	"strings"

	"github.com/araddon/dateparse"
	"github.com/gorilla/mux"
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

// IsTime verifies whether a given string represents a valid date or not.
func IsTime(stringDate string) bool {
	s := strings.TrimSpace(stringDate)
	_, err := dateparse.ParseAny(s)
	return err == nil
}
