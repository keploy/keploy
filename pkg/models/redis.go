package models

import (
	"time"
)

type RedisSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	RedisRequests    []RedisRequests   `json:"RequestBin,omitempty" yaml:"requestBin,omitempty"`
	RedisResponses   []RedisResponses  `json:"ResponseBin,omitempty" yaml:"responseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
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
	Type string      `json:"type,omitempty" yaml:"type" bson:"type,omitempty"`
	Size int         `json:"size,omitempty" yaml:"size" bson:"size,omitempty"`
	Data interface{} `json:"data,omitempty" yaml:"data" bson:"data,omitempty"`
}

type RedisElement struct {
	Length int         `json:"length,omitempty" yaml:"length" bson:"length,omitempty"`
	Value  interface{} `json:"value,omitempty" yaml:"value" bson:"value,omitempty"`
}

type RedisMapBody struct {
	Key   RedisElement `json:"key,omitempty" yaml:"key" bson:"key,omitempty"`
	Value RedisElement `json:"value,omitempty" yaml:"value" bson:"value,omitempty"`
}
