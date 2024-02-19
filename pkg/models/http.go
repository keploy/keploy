package models

import "time"

type Method string

type HttpSchema struct {
	Metadata         map[string]string      `json:"metadata" yaml:"metadata"`
	Request          HttpReq                `json:"req" yaml:"req"`
	Response         HttpResp               `json:"resp" yaml:"resp"`
	Objects          []*OutputBinary        `json:"objects" yaml:"objects"`
	Assertions       map[string]interface{} `json:"assertions" yaml:"assertions,omitempty"`
	Created          int64                  `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time              `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time              `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}

type HttpReq struct {
	Method     Method            `json:"method" yaml:"method"`
	ProtoMajor int               `json:"proto_major" yaml:"proto_major"` // e.g. 1
	ProtoMinor int               `json:"proto_minor" yaml:"proto_minor"` // e.g. 0
	URL        string            `json:"url" yaml:"url"`
	URLParams  map[string]string `json:"url_params" yaml:"url_params,omitempty"`
	Header     map[string]string `json:"header" yaml:"header"`
	Body       string            `json:"body" yaml:"body"`
	Binary     string            `json:"binary" yaml:"binary,omitempty"`
	Form       []FormData        `json:"form" yaml:"form,omitempty"`
	Timestamp  time.Time         `json:"timestamp" yaml:"timestamp"`
}

type FormData struct {
	Key    string   `json:"key" bson:"key" yaml:"key"`
	Values []string `json:"values" bson:"values,omitempty" yaml:"values,omitempty"`
	Paths  []string `json:"paths" bson:"paths,omitempty" yaml:"paths,omitempty"`
}

type HttpResp struct {
	StatusCode    int               `json:"status_code" yaml:"status_code"` // e.g. 200
	Header        map[string]string `json:"header" yaml:"header"`
	Body          string            `json:"body" yaml:"body"`
	StatusMessage string            `json:"status_message" yaml:"status_message"`
	ProtoMajor    int               `json:"proto_major" yaml:"proto_major"`
	ProtoMinor    int               `json:"proto_minor" yaml:"proto_minor"`
	Binary        string            `json:"binary" yaml:"binary,omitempty"`
	Timestamp     time.Time         `json:"timestamp" yaml:"timestamp"`
}
