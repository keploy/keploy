package models

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
)

// StartupDeclarable reports whether a call identical to m, made in a test's
// window, can be declared a call every test may make, so that m, a plain
// startup: recording (BackgroundRecording.Startup: made before the first
// test, covered by no declared rule), is that test's own call under the
// proven switch, its miss saying how to declare it. A recording of a kind a
// rule system registered (RegisterStartupDeclarableKinds) is. An HTTP or
// HTTP/2 recording is when its recorder stated its lifetime: a keploy.yaml
// sessionCalls entry then declares it (Mock.DeclaredSessionCall), and one
// that states none no entry can. Any other is not: a plain startup:
// recording of it is the set's background in a test's window too, as in
// v3.7.18, since its user has no way to declare the call. Nor is an HTTP or
// HTTP/2 recording of an API whose calls share one method and path, telling
// one from another by a header or the body alone (namesCallInRequest),
// whatever rule system is registered: a sessionCalls entry names a call by
// its method and path, so it cannot name one of them apart from the others.
func StartupDeclarable(m *Mock) bool {
	if m == nil || namesCallInRequest(m) {
		return false
	}
	startupDeclarable.mu.RLock()
	registered := startupDeclarable.kinds[m.Kind] > 0
	startupDeclarable.mu.RUnlock()
	if registered {
		return true
	}
	if m.Kind != HTTP && m.Kind != HTTP2 {
		return false
	}
	_, stated := statedLifetime(m)
	return stated
}

// RegisterStartupDeclarableKinds makes every recording of kinds
// StartupDeclarable: a rule system that lets a user declare a call of each
// of them (enterprise's startup: true rules) calls it once, at init, in
// every process that marks
// a set's background recordings: the agent marks them
// (agent.backgroundRecordingsOf). keploy's enterprise binary is its own
// agent (the agent it starts, natively or in its container, is the same
// binary), so a registration in its init reaches the agent too. unregister
// undoes this call alone (for tests).
func RegisterStartupDeclarableKinds(kinds ...Kind) (unregister func()) {
	startupDeclarable.mu.Lock()
	defer startupDeclarable.mu.Unlock()
	if startupDeclarable.kinds == nil {
		startupDeclarable.kinds = map[Kind]int{}
	}
	for _, k := range kinds {
		startupDeclarable.kinds[k]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			startupDeclarable.mu.Lock()
			defer startupDeclarable.mu.Unlock()
			for _, k := range kinds {
				if startupDeclarable.kinds[k]--; startupDeclarable.kinds[k] <= 0 {
					delete(startupDeclarable.kinds, k)
				}
			}
		})
	}
}

var startupDeclarable struct {
	mu    sync.RWMutex
	kinds map[Kind]int
}

// namesCallInRequest reports whether m is an HTTP or HTTP/2 recording of an
// API whose calls share one method and path and name the call in a header or
// the body, each told by a precise marker:
//   - AWS JSON (DynamoDB, SQS, Kinesis): an X-Amz-Target header;
//   - AWS Query (SQS, SNS, STS on older SDKs): a form-encoded body
//     (Content-Type application/x-www-form-urlencoded) with both Action and
//     Version parameters;
//   - GraphQL: a JSON body whose top-level object has a string query field;
//   - JSON-RPC: a JSON body whose top-level object has a jsonrpc field.
//
// Header names are read in any case. A recording it misreads as one keeps
// v3.7.18's behaviour (StartupDeclarable), the safe direction.
func namesCallInRequest(m *Mock) bool {
	var header map[string]string
	var body string
	switch {
	case m.Kind == HTTP && m.Spec.HTTPReq != nil:
		header, body = m.Spec.HTTPReq.Header, m.Spec.HTTPReq.Body
	case m.Kind == HTTP2 && m.Spec.HTTP2Req != nil:
		header, body = m.Spec.HTTP2Req.Headers, m.Spec.HTTP2Req.Body
	default:
		return false
	}
	if _, ok := headerValue(header, "X-Amz-Target"); ok {
		return true
	}
	if ct, _ := headerValue(header, "Content-Type"); strings.Contains(strings.ToLower(ct), "application/x-www-form-urlencoded") {
		if q, err := url.ParseQuery(body); err == nil && q.Has("Action") && q.Has("Version") {
			return true
		}
	}
	var top map[string]json.RawMessage
	if b := strings.TrimSpace(body); !strings.HasPrefix(b, "{") || json.Unmarshal([]byte(b), &top) != nil {
		return false
	}
	if q, ok := top["query"]; ok && len(q) > 0 && q[0] == '"' {
		return true
	}
	_, rpc := top["jsonrpc"]
	return rpc
}

// headerValue is the value of the header name in h, its name read in any
// case.
func headerValue(h map[string]string, name string) (string, bool) {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}
