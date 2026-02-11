package models

import "time"

// HTTP2Req represents an HTTP/2 outgoing request captured by the proxy.
type HTTP2Req struct {
	Method    string            `json:"method" yaml:"method"`
	URL       string            `json:"url" yaml:"url"`
	Authority string            `json:"authority" yaml:"authority"`
	Scheme    string            `json:"scheme" yaml:"scheme"`
	Headers   map[string]string `json:"headers" yaml:"headers"`
	Body      string            `json:"body" yaml:"body"`
	Timestamp time.Time         `json:"timestamp" yaml:"timestamp"`
}

// HTTP2Resp represents an HTTP/2 outgoing response captured by the proxy.
type HTTP2Resp struct {
	StatusCode int               `json:"status_code" yaml:"status_code"`
	Headers    map[string]string `json:"headers" yaml:"headers"`
	Body       string            `json:"body" yaml:"body"`
	Trailers   map[string]string `json:"trailers,omitempty" yaml:"trailers,omitempty"`
	Timestamp  time.Time         `json:"timestamp" yaml:"timestamp"`
}
