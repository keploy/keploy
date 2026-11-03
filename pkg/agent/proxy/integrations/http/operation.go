package http

import (
	"net/http"
	"runtime"
	"sync"
	"weak"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// liveOperation is the label of the operation a live call carries
// (models.HTTPOperationLabel), read from the request as a recording holds
// one: its method, URL, headers, query parameters and body. "" when it
// carries none, or no reader is registered.
func liveOperation(request *http.Request, body []byte) string {
	if request == nil || request.URL == nil {
		return ""
	}
	return models.HTTPOperationLabel(&models.HTTPReq{
		Method:    models.Method(request.Method),
		URL:       request.URL.String(),
		Header:    pkg.ToYamlHTTPHeader(request.Header),
		Body:      string(body),
		URLParams: pkg.URLParams(request),
	})
}

// recordedOperations keeps the label of the operation each recorded request
// carries, so a miss, which compares itself with every recording of its pool
// (pickClosestCandidate), reads each recording's body once, not once per
// miss.
var recordedOperations = &operationTable{}

// operationTable keeps the label of the operation a recorded request carries
// (models.HTTPOperationLabel, "" for none), by the identity of the request
// (Mock.Spec.HTTPReq). Pooled mocks are never changed, and the mock manager's
// copies of a pooled mock share its request (answerTable), so a request's
// label is the one read first. The table holds no request: an entry goes
// when its request is collected.
type operationTable struct {
	entries sync.Map // weak.Pointer[models.HTTPReq] -> string
}

// of is the label of the operation r carries, read the first time r is asked
// for: "" for a nil request.
func (t *operationTable) of(r *models.HTTPReq) string {
	if r == nil {
		return ""
	}
	key := weak.Make(r)
	if v, ok := t.entries.Load(key); ok {
		return v.(string)
	}
	label := models.HTTPOperationLabel(r)
	if _, loaded := t.entries.LoadOrStore(key, label); !loaded {
		runtime.AddCleanup(r, t.forget, key)
	}
	return label
}

// forget drops a collected request's entry.
func (t *operationTable) forget(key weak.Pointer[models.HTTPReq]) {
	t.entries.Delete(key)
}
