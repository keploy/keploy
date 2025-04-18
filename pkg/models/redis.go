package models

import (
	"time"
)

type RedisSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	RedisRequests    []RedisRequests   `json:"RequestBin,omitempty"`
	RedisResponses   []RedisResponses  `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}

type RedisRequests struct {
	Origin  OriginType      `json:"Origin,omitempty" yaml:"origin" bson:"origin,omitempty"`
	Message []RedisBodyType `json:"Message,omitempty" yaml:"message" bson:"message,omitempty"`
}

type RedisResponses struct {
	Origin  OriginType      `json:"Origin,omitempty" yaml:"origin" bson:"origin,omitempty"`
	Message []RedisBodyType `json:"Message,omitempty" yaml:"message" bson:"message,omitempty"`
}

type RedisBodyType struct {
	Type string
	Size int
	Data interface{}
}

type RedisElement struct {
	Length int
	Value  interface{}
}

type RedisMapBody struct {
	Key   RedisElement
	Value RedisElement
}
