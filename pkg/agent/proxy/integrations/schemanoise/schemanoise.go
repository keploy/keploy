// Package schemanoise is the deprecated former home of the mock-noise engine.
//
// Deprecated: use go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise.
// The engine was renamed because "schema noise" read as though it belonged to
// schema matching or the smart-set schema pipeline; it belongs to neither. It
// is noise learned on a MOCK.
//
// WHY ALIASES AND NOT WRAPPERS. Every declaration here is a type alias (=), not
// a defined type. Go treats an alias as the same type, so a *mocknoise.Engine
// built by migrated code can be passed to an unmigrated function typed
// *schemanoise.Engine and vice versa. A defined type (`type Engine
// mocknoise.Engine`) would compile here and then fail at every boundary between
// a migrated and an unmigrated caller — which, with the rename landing across
// five repositories that merge in an arbitrary order, is every boundary that
// matters. The same reasoning applies to Adapter: an adapter written against
// either spelling must satisfy the other's interface, and only an alias gives
// that.
//
// This shim exists so the repositories can migrate independently. Delete it
// once integrations, enterprise, k8s-proxy and enterprise-ui are all on the
// mocknoise path and no released artifact still imports this one.
package schemanoise

import "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"

// Adapter is the per-protocol contract for the mock-noise engine.
//
// Deprecated: use mocknoise.Adapter.
type Adapter = mocknoise.Adapter

// Engine runs the mock-noise learn/enforce flow for one protocol.
//
// Deprecated: use mocknoise.Engine.
type Engine = mocknoise.Engine

// JSONDiffer is the default Adapter.Diff implementation for JSON bodies,
// embedded by protocol adapters.
//
// Deprecated: use mocknoise.JSONDiffer.
type JSONDiffer = mocknoise.JSONDiffer

// New builds an Engine for an adapter with the resolved mock-noise flags.
//
// Deprecated: use mocknoise.New.
var New = mocknoise.New

// MergeLearned merges newly detected drift into a mock's existing noise.
//
// Deprecated: use mocknoise.MergeLearned.
var MergeLearned = mocknoise.MergeLearned

// MergeKnown unions two known-noise maps.
//
// Deprecated: use mocknoise.MergeKnown.
var MergeKnown = mocknoise.MergeKnown

// StripBodyPrefix trims the leading "body." from each key.
//
// Deprecated: use mocknoise.StripBodyPrefix.
var StripBodyPrefix = mocknoise.StripBodyPrefix

// AddBodyPrefix re-adds the leading "body." to each key.
//
// Deprecated: use mocknoise.AddBodyPrefix.
var AddBodyPrefix = mocknoise.AddBodyPrefix

// DetectJSONDrift diffs two JSON bodies and returns the drifting field paths.
//
// Deprecated: use mocknoise.DetectJSONDrift.
var DetectJSONDrift = mocknoise.DetectJSONDrift
