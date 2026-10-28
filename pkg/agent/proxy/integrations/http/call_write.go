package http

import (
	"go.keploy.io/server/v3/pkg/models"
)

// callWrite is what an HTTP recording's call writes, for
// models.RegisterCallWrite: "HTTP POST" for a POST, PUT, PATCH or DELETE,
// the methods RFC 9110 does not define as safe. A GET, HEAD, OPTIONS or
// TRACE reads. Any other method, a method spelled in another case (methods
// are case-sensitive), and a recording with no request are calls keploy
// cannot tell, so none is a write. The method is the protocol's own word for
// it: a POST that only queries (a search endpoint) counts as a write.
func callWrite(m *models.Mock) (string, bool) {
	if m == nil || m.Spec.HTTPReq == nil {
		return "", false
	}
	switch method := m.Spec.HTTPReq.Method; method {
	case models.Method("POST"), models.Method("PUT"), models.Method("PATCH"), models.Method("DELETE"):
		return "HTTP " + string(method), true
	}
	return "", false
}

func init() {
	models.RegisterCallWrite(models.HTTP, callWrite)
}
