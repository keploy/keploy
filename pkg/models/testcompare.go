package models

type AbsResult struct {
	Kind       StringResult `json:"kind" bson:"kind" yaml:"kind"`
	Name       StringResult `json:"name" bson:"name" yaml:"name"`
	Req        ReqCompare   `json:"req" bson:"req" yaml:"req"`
	Resp       RespCompare  `json:"resp" bson:"resp" yaml:"resp"`
	CurlResult StringResult `json:"curl_result" bson:"curl_result" yaml:"curl_result"`
}

type ReqCompare struct {
	MethodResult    StringResult      `json:"method_result" bson:"method_result" yaml:"method_result"`
	URLResult       StringResult      `json:"url_result" bson:"url_result" yaml:"url_result"`
	URLParamsResult []URLParamsResult `json:"url_params_result" bson:"url_params_result" yaml:"url_params_result"`
	ProtoMajor      IntResult         `json:"proto_major" bson:"proto_major" yaml:"proto_major"`
	ProtoMinor      IntResult         `json:"proto_minor" bson:"proto_minor" yaml:"proto_minor"`
	HeaderResult    []HeaderResult    `json:"headers_result" bson:"headers_result" yaml:"headers_result"`
	BodyResult      BodyResult        `json:"body_result" bson:"body_result" yaml:"body_result"`
}

type RespCompare struct {
	StatusCode    IntResult      `json:"status_code" bson:"status_code" yaml:"status_code"`
	HeadersResult []HeaderResult `json:"headers_result" bson:"headers_result" yaml:"headers_result"`
	BodyResult    BodyResult     `json:"body_result" bson:"body_result" yaml:"body_result"`
}

type StringResult struct {
	Normal   bool   `json:"normal" bson:"normal" yaml:"normal"`
	Expected string `json:"expected" bson:"expected" yaml:"expected"`
	Actual   string `json:"actual" bson:"actual" yaml:"actual"`
}

type URLParamsResult struct {
	Normal   bool   `json:"normal" bson:"normal" yaml:"normal"`
	Expected Params `json:"expected" bson:"expected" yaml:"expected"`
	Actual   Params `json:"actual" bson:"actual" yaml:"actual"`
}

type Params struct {
	Key   string `json:"key" bson:"key" yaml:"key"`
	Value string `json:"value" bson:"value" yaml:"value"`
}
