package models

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// AsyncLanesEnvVar is the agent env var that carries a recording's async-egress
// lanes (base64-encoded JSON of []AsyncLane). It is the wire contract between a
// config producer that HAS the lanes (e.g. the k8s-proxy, from its record API)
// and an in-cluster agent that has no keploy.yml and reads all of its config
// from env vars into config.Config.Async.Lanes at startup.
//
// Both ends MUST use this const and the Encode/Decode pair below so the
// contract (env-var name + base64-JSON shape) lives in exactly one place and
// cannot silently drift between the producing and consuming services.
const AsyncLanesEnvVar = "KEPLOY_ASYNC_LANES"

// EncodeAsyncLanesEnv returns the base64-JSON AsyncLanesEnvVar value for lanes,
// or "" when there are none — so a producer adds no env entry for a non-async
// recording and the consumer leaves async off.
func EncodeAsyncLanesEnv(lanes []AsyncLane) string {
	if len(lanes) == 0 {
		return ""
	}
	b, err := json.Marshal(lanes)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeAsyncLanesEnv decodes an AsyncLanesEnvVar value into lanes. Empty input
// yields nil lanes with no error (async simply stays off).
func DecodeAsyncLanesEnv(s string) ([]AsyncLane, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", AsyncLanesEnvVar, err)
	}
	var lanes []AsyncLane
	if err := json.Unmarshal(raw, &lanes); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", AsyncLanesEnvVar, err)
	}
	return lanes, nil
}
