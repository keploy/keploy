package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// isPollLaneRequest is the record-time predicate that lets recordV2 disarm the
// supervisor's hang watchdog for a long-poll connection. It must match ONLY
// poll-type lanes, and by request shape (path/query) alone so it works before
// any mock is emitted.
func TestIsPollLaneRequest(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	h.SetAsyncEngine(async.NewEngine(zaptest.NewLogger(t),
		[]models.AsyncLane{{
			Name:       "config-watch",
			Type:       "httpPoll",
			Match:      map[string]string{"pathRegex": "^/v1/buckets/stream-relay$"},
			MatchQuery: map[string]string{"watch": "true"},
		}},
		map[string]async.AsyncParser{"http": h}))

	pollReq := []byte("GET /v1/buckets/stream-relay?watch=true&version=1 HTTP/1.1\r\nHost: cfg.local\r\n\r\n")
	if !h.isPollLaneRequest(pollReq) {
		t.Fatalf("watch=true request should match the httpPoll lane")
	}

	// watch=false is the one-time boot fetch — an ordinary (non-poll) call.
	bootReq := []byte("GET /v1/buckets/stream-relay?watch=false HTTP/1.1\r\nHost: cfg.local\r\n\r\n")
	if h.isPollLaneRequest(bootReq) {
		t.Fatalf("watch=false request must not match the httpPoll lane")
	}

	// A different path is not in the lane at all.
	otherReq := []byte("GET /health HTTP/1.1\r\nHost: cfg.local\r\n\r\n")
	if h.isPollLaneRequest(otherReq) {
		t.Fatalf("unrelated path must not match the httpPoll lane")
	}
}

// A matching async lane that is NOT a poll (type "http") must NOT suspend the
// watchdog: only long-poll lanes are exempt from hang detection.
func TestIsPollLaneRequestNonPollLaneNotMatched(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	h.SetAsyncEngine(async.NewEngine(zaptest.NewLogger(t),
		[]models.AsyncLane{{
			Name:       "config-watch",
			Type:       "http",
			Match:      map[string]string{"pathRegex": "^/v1/buckets/stream-relay$"},
			MatchQuery: map[string]string{"watch": "true"},
		}},
		map[string]async.AsyncParser{"http": h}))

	pollReq := []byte("GET /v1/buckets/stream-relay?watch=true&version=1 HTTP/1.1\r\nHost: cfg.local\r\n\r\n")
	if h.isPollLaneRequest(pollReq) {
		t.Fatalf("a non-poll (type http) async lane must not be treated as a poll")
	}
}

// With no async engine configured, nothing is a poll lane.
func TestIsPollLaneRequestNilEngine(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	if h.isPollLaneRequest([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")) {
		t.Fatalf("nil async engine must not match any poll lane")
	}
}

// Malformed request bytes must return false, not panic — the request read
// on the record path can hand over garbage on a decode error.
func TestIsPollLaneRequestMalformed(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	h.SetAsyncEngine(async.NewEngine(zaptest.NewLogger(t),
		[]models.AsyncLane{{
			Name:       "config-watch",
			Type:       "httpPoll",
			Match:      map[string]string{"pathRegex": "^/v1/buckets/stream-relay$"},
			MatchQuery: map[string]string{"watch": "true"},
		}},
		map[string]async.AsyncParser{"http": h}))

	if h.isPollLaneRequest([]byte("\x00\x01 not a valid http request \xff")) {
		t.Fatalf("malformed request must not match a poll lane")
	}
}
