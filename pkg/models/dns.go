package models

import "time"

// DNSReq captures the DNS question information recorded by the proxy DNS server.
type DNSReq struct {
	Name   string `json:"name" yaml:"name"`
	Qtype  uint16 `json:"qtype" yaml:"qtype"`
	Qclass uint16 `json:"qclass" yaml:"qclass"`
}

// DNSResp captures the DNS answer section as zone-format strings.
type DNSResp struct {
	Rcode              int  `json:"rcode" yaml:"rcode" bson:"rcode"`
	Authoritative      bool `json:"authoritative" yaml:"authoritative" bson:"authoritative"`
	RecursionAvailable bool `json:"recursionAvailable" yaml:"recursionAvailable" bson:"recursionAvailable"`
	Truncated          bool `json:"truncated" yaml:"truncated" bson:"truncated"`

	Answers []string `json:"answers,omitempty" yaml:"answers,omitempty" bson:"answers,omitempty"`
	Ns      []string `json:"ns,omitempty" yaml:"ns,omitempty" bson:"ns,omitempty"`
	Extra   []string `json:"extra,omitempty" yaml:"extra,omitempty" bson:"extra,omitempty"`
}

// DNSSchema is the YAML/JSON representation for DNS mocks.
type DNSSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Request          DNSReq            `json:"request" yaml:"request"`
	Response         DNSResp           `json:"response" yaml:"response"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}
