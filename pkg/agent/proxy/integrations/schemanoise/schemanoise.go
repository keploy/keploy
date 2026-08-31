// Package schemanoise is the deprecated import path for the mock-noise engine.
//
// Deprecated: use go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise
// instead. The feature was renamed from "schema noise" to "mock noise" across
// flags, config keys, identifiers and logs; this package remains only so that
// modules which import the old path — github.com/keploy/integrations and
// github.com/keploy/enterprise — keep compiling while they migrate.
//
// Everything here forwards to mocknoise. The types are TYPE ALIASES, not
// wrappers, so schemanoise.Engine and mocknoise.Engine are the same type: a
// package that has migrated and one that has not can still exchange values,
// and an Adapter implemented against either path satisfies both. That is what
// makes the three repos migratable in any order.
//
// This shim is removed once integrations and enterprise are both on mocknoise.
package schemanoise

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
)

// Adapter is an alias for mocknoise.Adapter.
//
// Deprecated: use mocknoise.Adapter.
type Adapter = mocknoise.Adapter

// JSONDiffer is an alias for mocknoise.JSONDiffer.
//
// Deprecated: use mocknoise.JSONDiffer.
type JSONDiffer = mocknoise.JSONDiffer

// Engine is an alias for mocknoise.Engine.
//
// Deprecated: use mocknoise.Engine.
type Engine = mocknoise.Engine

// New forwards to mocknoise.New.
//
// Deprecated: use mocknoise.New.
func New(a Adapter, detection, strict bool) *Engine {
	return mocknoise.New(a, detection, strict)
}

// DetectJSONDrift forwards to mocknoise.DetectJSONDrift.
//
// Deprecated: use mocknoise.DetectJSONDrift.
func DetectJSONDrift(recordedBody, liveBody []byte, known map[string][]string, isRecordedNoise func(string) bool) (drift map[string][]string, comparable bool) {
	return mocknoise.DetectJSONDrift(recordedBody, liveBody, known, isRecordedNoise)
}

// MergeLearned forwards to mocknoise.MergeLearned.
//
// Deprecated: use mocknoise.MergeLearned.
func MergeLearned(existing, detected map[string][]string) map[string][]string {
	return mocknoise.MergeLearned(existing, detected)
}

// MergeKnown forwards to mocknoise.MergeKnown.
//
// Deprecated: use mocknoise.MergeKnown.
func MergeKnown(a, b map[string][]string) map[string][]string {
	return mocknoise.MergeKnown(a, b)
}

// StripBodyPrefix forwards to mocknoise.StripBodyPrefix.
//
// Deprecated: use mocknoise.StripBodyPrefix.
func StripBodyPrefix(in map[string][]string) map[string][]string {
	return mocknoise.StripBodyPrefix(in)
}

// AddBodyPrefix forwards to mocknoise.AddBodyPrefix.
//
// Deprecated: use mocknoise.AddBodyPrefix.
func AddBodyPrefix(in map[string][]string) map[string][]string {
	return mocknoise.AddBodyPrefix(in)
}
