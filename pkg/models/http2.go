package models

import "time"

// HTTP2Req represents an HTTP/2 outgoing request captured by the proxy.
type HTTP2Req struct {
	Method    Method            `json:"method" yaml:"method"`
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

// HTTP2Schema is the YAML/JSON representation for HTTP/2 outgoing mocks.
type HTTP2Schema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Request          HTTP2Req          `json:"req" yaml:"req"`
	Response         HTTP2Resp         `json:"resp" yaml:"resp"`
	Created          int64             `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}
