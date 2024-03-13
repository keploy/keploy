package models

type AbsResult struct {
	Kind       StringResult `json:"kind" bson:"kind" yaml:"kind"`
	Name       StringResult `json:"name" bson:"name" yaml:"name"`
	ReqResult  ReqResult    `json:"req_result" bson:"req_result" yaml:"req_result"`
	RespResult RespResult   `json:"resp_result" bson:"resp_result" yaml:"resp_result"`
	CurlResult StringResult `json:"curl_result" bson:"curl_result" yaml:"curl_result"`
}

type ReqResult struct {
	MethodResult    StringResult      `json:"method_result" bson:"method_result" yaml:"method_result"`
	UrlResult       StringResult      `json:"url_result" bson:"url_result" yaml:"url_result"`
	UrlParamsResult []URLParamsResult `json:"url_params_result" bson:"url_params_result" yaml:"url_params_result"`
	ProtoMajor      IntResult         `json:"proto_major" bson:"proto_major" yaml:"proto_major"`
	ProtoMinor      IntResult         `json:"proto_minor" bson:"proto_minor" yaml:"proto_minor"`
	HeaderResult    []HeaderResult    `json:"headers_result" bson:"headers_result" yaml:"headers_result"`
	BodyResult      BodyResult        `json:"body_result" bson:"body_result" yaml:"body_result"`
	HostResult      StringResult      `json:"host_result" bson:"host_result" yaml:"host_result"`
}

type RespResult struct {
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
