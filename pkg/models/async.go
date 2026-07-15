package models

// Async metadata keys stamped on an ordinary egress mock's Spec.Metadata
// when it matches a declared async lane at record time.
const (
	MetaAsync       = "async"       // "true"
	MetaAsyncLane   = "lane"        // lane name
	MetaAnchorAfter = "anchorAfter" // last completed testcase Name, or "startup" (readability)
	MetaAnchorPos   = "anchorPos"   // number of testcases completed before this egress fired (decimal)
	MetaAsyncSeq    = "asyncSeq"    // per-lane order counter (decimal)
)

// AnchorStartup is the MetaAnchorAfter value for async mocks that fired
// before the first testcase completed.
const AnchorStartup = "startup"

// AsyncLane is one declared async lane. Match is opaque to the engine and
// interpreted by the owning parser's MatchesLane. Lives in models so both
// config (yaml) and the proxy async package can reference it without an
// import cycle (mirrors BypassRule / Filter).
type AsyncLane struct {
	Name           string            `json:"name" yaml:"name" mapstructure:"name"`
	Type           string            `json:"type" yaml:"type" mapstructure:"type"` // owning parser, e.g. "http"
	Match          map[string]string `json:"match" yaml:"match" mapstructure:"match"`
	VolatileParams []string          `json:"volatileParams,omitempty" yaml:"volatileParams,omitempty" mapstructure:"volatileParams"`
	NotExercised   string            `json:"notExercised,omitempty" yaml:"notExercised,omitempty" mapstructure:"notExercised"` // skip|fail (default skip)
}
