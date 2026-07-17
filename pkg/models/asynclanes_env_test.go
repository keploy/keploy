package models

import "testing"

func TestEncodeAsyncLanesEnvEmpty(t *testing.T) {
	if got, err := EncodeAsyncLanesEnv(nil); got != "" || err != nil {
		t.Fatalf("nil lanes must encode to (\"\", nil), got (%q, %v)", got, err)
	}
	if got, err := EncodeAsyncLanesEnv([]AsyncLane{}); got != "" || err != nil {
		t.Fatalf("empty lanes must encode to (\"\", nil), got (%q, %v)", got, err)
	}
}

func TestDecodeAsyncLanesEnvEmpty(t *testing.T) {
	lanes, err := DecodeAsyncLanesEnv("")
	if err != nil || lanes != nil {
		t.Fatalf("empty input must decode to (nil, nil), got (%v, %v)", lanes, err)
	}
}

func TestAsyncLanesEnvRoundTrip(t *testing.T) {
	in := []AsyncLane{{
		Name:           "config-watch",
		Type:           "httpPoll",
		Match:          map[string]string{"pathRegex": "^/v1/buckets/app-config$"},
		MatchQuery:     map[string]string{"watch": "true"},
		VolatileParams: []string{"version"},
	}}
	enc, err := EncodeAsyncLanesEnv(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" {
		t.Fatal("non-empty lanes must encode to a non-empty value")
	}
	out, err := DecodeAsyncLanesEnv(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Type != "httpPoll" || out[0].Name != "config-watch" ||
		out[0].Match["pathRegex"] != "^/v1/buckets/app-config$" ||
		out[0].MatchQuery["watch"] != "true" || len(out[0].VolatileParams) != 1 ||
		out[0].VolatileParams[0] != "version" {
		t.Fatalf("round-trip lost lane data: %+v", out)
	}
}

func TestDecodeAsyncLanesEnvBadInput(t *testing.T) {
	if _, err := DecodeAsyncLanesEnv("!!!not-base64!!!"); err == nil {
		t.Fatal("invalid base64 must return an error")
	}
}
