package models

import (
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"
)

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
	// Name identifies the lane. It is OPTIONAL: leave it empty and a
	// deterministic name is derived from the routing identity (see
	// EffectiveName). Set it only when a human-readable label is wanted in the
	// recorded metadata / replay verdict.
	Name string `json:"name,omitempty" yaml:"name,omitempty" mapstructure:"name"`
	Type string `json:"type" yaml:"type" mapstructure:"type"` // owning parser, e.g. "http"
	// Match is the transport-interpreted match block (e.g. host/path globs).
	Match map[string]string `json:"match" yaml:"match" mapstructure:"match"`
	// MatchQuery, when set, additionally requires the request's query to carry
	// each key=value. Lets a lane target only the long-poll variant of an
	// endpoint (e.g. watch=true) while leaving a same-path boot call
	// (watch=false) as ordinary non-async egress.
	MatchQuery     map[string]string `json:"matchQuery,omitempty" yaml:"matchQuery,omitempty" mapstructure:"matchQuery"`
	VolatileParams []string          `json:"volatileParams,omitempty" yaml:"volatileParams,omitempty" mapstructure:"volatileParams"`
}

// EffectiveName returns the caller-supplied Name, or a deterministic name
// derived from the lane's ROUTING identity (type + match + matchQuery) when
// Name is empty. The derived name is stable across record and replay for the
// same routing config, so it works as the join key stamped on mocks
// (MetaAsyncLane) at record and looked up by the replay engine.
//
// volatileParams is deliberately EXCLUDED: it is replay-time tuning a user may
// set differently between the record and replay runs, and letting it shift the
// name would break that record→replay join.
func (l AsyncLane) EffectiveName() string {
	if l.Name != "" {
		return l.Name
	}
	h := fnv.New64a()
	writeCanonicalIdentity(h, l)
	prefix := l.Type
	if prefix == "" {
		prefix = "lane"
	}
	if slug := laneSlug(l); slug != "" {
		prefix += "-" + slug
	}
	// 32 bits of the identity hash — ample to keep a handful of caller-declared
	// lanes collision-free, short enough to stay readable.
	return prefix + "-" + fmt.Sprintf("%08x", uint32(h.Sum64()))
}

// WithEffectiveNames returns a copy of lanes with every empty Name filled in by
// EffectiveName; caller-supplied names are left untouched. The input slice and
// its elements are not modified (only the Name field of the copies is set).
func WithEffectiveNames(lanes []AsyncLane) []AsyncLane {
	if len(lanes) == 0 {
		return lanes
	}
	out := make([]AsyncLane, len(lanes))
	copy(out, lanes)
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = out[i].EffectiveName()
		}
	}
	return out
}

// writeCanonicalIdentity writes a stable, order-independent encoding of the
// lane's routing fields so equal routing yields an equal hash regardless of map
// iteration order.
func writeCanonicalIdentity(w io.Writer, l AsyncLane) {
	io.WriteString(w, l.Type)
	io.WriteString(w, "\x00match\x00")
	writeSortedKV(w, l.Match)
	io.WriteString(w, "\x00query\x00")
	writeSortedKV(w, l.MatchQuery)
}

func writeSortedKV(w io.Writer, m map[string]string) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		io.WriteString(w, k)
		io.WriteString(w, "=")
		io.WriteString(w, m[k])
		io.WriteString(w, "\x00")
	}
}

// laneSlug builds a short, readable, alnum-hyphen token from the lane's path
// (or host) so a generated name still hints at what it matches in recorded
// metadata and the replay verdict. Empty when the lane has no path/host.
func laneSlug(l AsyncLane) string {
	src := l.Match["pathRegex"]
	if src == "" {
		src = l.Match["path"]
	}
	if src == "" {
		src = l.Match["host"]
	}
	if src == "" {
		return ""
	}
	// Split on any run of non-alnum; Join drops leading/trailing/collapsed
	// separators for us, leaving a clean hyphen token.
	parts := strings.FieldsFunc(strings.ToLower(src), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	s := strings.Join(parts, "-")
	const maxSlug = 24
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "-")
	}
	return s
}
