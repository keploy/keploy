package matcher

import (
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// A response often holds a map keyed by ids rather than by field names: a
// search result keyed by option id, a cart keyed by line id, a time series
// keyed by epoch second. The noise vocabulary (noisepath.go) keys object keys
// verbatim, so a field under such a map has one path per id, and a volatile
// field under every id needs one entry per id. Those entries are correct but
// they say one thing many times over: a response keyed by a few dozen option
// ids carried hundreds of entries for three clock-derived fields, and a bound
// on how much noise a case may carry (counted in entries, meant as fields) then
// refused a case whose only new drift was a single trace id.
//
// NoiseIDKey is the vocabulary's word for "any id key at this level". An array
// position is already a segment the walker never emits, so every element of
// `items` shares the path `items`; an id key is not dropped the same way,
// because a map can hold ordinary keys beside its ids and an id map can sit
// where an ordinary key of the same name sits in another response. It is
// spelled instead, as one segment:
//
//	results.options.{id}.traceId
//
// names `traceId` under every id key of `options`, and nothing under an
// ordinary key there.
//
// The rules that keep the spelling from changing what an existing entry means:
//
//   - An entry without the segment matches exactly as before. An entry naming
//     one id ("results.options.<uuid>.traceId") still names that id only;
//     reading it as every id would silence the same field under ids whose
//     value never drifted (auto-noise writes per-id entries for the ids whose
//     field drifted, not for all of them).
//   - An entry with the segment is matched against the traversal key with its
//     id keys collapsed (CollapseIDKeys), by the same test as any other entry.
//     Its own id keys are collapsed too, so a hand-written entry naming a
//     literal id beside the segment ("hotels.<uuid>.options.{id}.x") reads as
//     "hotels.{id}.options.{id}.x"; no keploy tool writes that spelling.
//   - Only an id key collapses. What counts is deliberately narrow (IsIDKey), so
//     a year, an HTTP status, a date or a short database id stays an ordinary
//     key that an entry must name.
//   - An entry with the segment speaks for the ids the recording has. It
//     excuses a value that moved under one of them, or a field a replay dropped
//     there, as an entry per id would. It never excuses a key the recording
//     lacks (noiseIndex.matchAdded): a new id, or a field appearing under an id
//     that did not carry it, is a change, as it is for an entry per id, which
//     is only ever written for the ids that carried the field.
//
// The braces cannot occur in an id, are not a token of any other noise dialect
// ("[]", "$.a[0]", "~0"/"~1", the "*" body wildcard), and an older matcher reads
// an entry carrying the segment as a substring no key contains: it matches
// nothing there, so the field stays asserted rather than being silenced. A JSON
// key that is literally "{id}" is matched as if it were an id key.

// NoiseIDKey is the path segment that stands for any id key at its level.
const NoiseIDKey = "{id}"

// minIDHexLen and minIDDigitsLen bound the shortest hex and all-digit keys read
// as ids. 16 hex characters is a 64-bit id; a MongoDB ObjectId is 24, an MD5
// 32, a SHA-1 40. Ten digits is the shortest epoch second of this century and
// past any year, HTTP status, YYYYMMDD date or small serial id.
const (
	minIDHexLen    = 16
	minIDDigitsLen = models.IDKeyMinDigits
)

// IsIDKey reports whether an object key is an id rather than a field name: a
// canonical UUID, a ULID, a run of at least minIDHexLen hex characters with a
// digit among them, or at least minIDDigitsLen digits. Case is ignored. The
// whole key must be the id; a key that only contains one ("opt_<uuid>") is a
// name.
func IsIDKey(key string) bool {
	return isUUIDKey(key) || isULIDKey(key) || isLongHexKey(key) || isLongDigitsKey(key)
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isUUIDKey: 8-4-4-4-12 hex, any version or variant.
func isUUIDKey(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexByte(s[i]) {
				return false
			}
		}
	}
	return true
}

// isULIDKey: 26 characters of Crockford's base 32 (no I, L, O or U), the first
// 0-7 because a ULID's leading 48 bits are a millisecond timestamp.
func isULIDKey(s string) bool {
	if len(s) != 26 || s[0] < '0' || s[0] > '7' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z' && c != 'i' && c != 'l' && c != 'o' && c != 'u':
		default:
			return false
		}
	}
	return true
}

// isLongHexKey: a hex run long enough to be an id, with a digit, so that a long
// word spelled only from a-f is not one.
func isLongHexKey(s string) bool {
	if len(s) < minIDHexLen {
		return false
	}
	digit := false
	for i := 0; i < len(s); i++ {
		if !isHexByte(s[i]) {
			return false
		}
		if s[i] >= '0' && s[i] <= '9' {
			digit = true
		}
	}
	return digit
}

func isLongDigitsKey(s string) bool {
	if len(s) < minIDDigitsLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitArraySuffix separates a path segment from the "[]" markers the request
// and risk dialects append to an array's key ("items[]"), so the key itself can
// be judged.
func splitArraySuffix(seg string) (core, suffix string) {
	core = seg
	for strings.HasSuffix(core, "[]") {
		core = core[:len(core)-2]
	}
	return core, seg[len(core):]
}

// CollapseIDKeys returns path with every segment that is an id key (IsIDKey)
// replaced by NoiseIDKey, and every other segment as it was. A "[]" array
// marker after a segment is kept. It is idempotent, and returns path itself
// when nothing collapses.
func CollapseIDKeys(path string) string {
	if !mayHoldIDKey(path) {
		return path
	}
	segs := strings.Split(path, ".")
	changed := false
	for i, seg := range segs {
		core, suffix := splitArraySuffix(seg)
		if IsIDKey(core) {
			segs[i] = NoiseIDKey + suffix
			changed = true
		}
	}
	if !changed {
		return path
	}
	return strings.Join(segs, ".")
}

// mayHoldIDKey is a cheap necessary condition for CollapseIDKeys to change
// anything: some run between dots at least as long as the shortest id key.
// Most traversal keys are short field names and fail it in one pass.
func mayHoldIDKey(path string) bool {
	run := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			run = 0
			continue
		}
		run++
		if run >= minIDDigitsLen {
			return true
		}
	}
	return false
}

// namesIDKeys reports whether a noise entry uses the NoiseIDKey segment, which
// is what makes it match against collapsed keys.
func namesIDKeys(entry string) bool {
	if !strings.Contains(entry, NoiseIDKey) {
		return false
	}
	for _, seg := range strings.Split(entry, ".") {
		if core, _ := splitArraySuffix(seg); core == NoiseIDKey {
			return true
		}
	}
	return false
}

// NoiseEntryKey is the lowercased key a noise entry is matched by, and whether
// the entry names id keys (uses the NoiseIDKey segment). For such an entry the
// key has its id keys collapsed too, so one spelled with a literal id beside
// the segment still compares like for like. Entries resolve most specific
// first by this key's length.
func NoiseEntryKey(entry string) (keyLower string, idKeys bool) {
	keyLower = strings.ToLower(entry)
	if namesIDKeys(keyLower) {
		return CollapseIDKeys(keyLower), true
	}
	return keyLower, false
}

// NoiseKeyCovers reports whether a DOTTED body-noise entry addresses the
// traversal key key, as the JSON walker resolves it: the entry is a substring
// of the key, compared without case, and an entry naming id keys is compared
// with the key's id keys collapsed. A dot-free entry is a global field name
// and is not resolved here.
func NoiseKeyCovers(key, entry string) bool {
	e, idKeys := NoiseEntryKey(entry)
	k := strings.ToLower(key)
	if idKeys {
		k = CollapseIDKeys(k)
	}
	return strings.Contains(k, e)
}

// CollapseIDKeyedPaths is the rule a producer that judges each path on its own
// (a value that reads as a time, say) applies to a field under a map keyed by
// ids. doc is the parsed JSON document (encoding/json's types). Every node of
// it under a key, an object, an array, a scalar or a null, is a path, named as
// the matcher names it: an object extends the path by key, an array keeps it.
// qualifies says whether a path is to be noise; it is asked once per path.
//
// A path naming no id key is returned when it qualifies, and so is a dot-free
// one, which is a global field name and is never collapsed. A field under id
// keys is returned once, with its id keys spelled NoiseIDKey, only when that
// one entry silences, of the keys doc has, exactly what its per-id paths
// would:
//
//   - it qualifies under every id key that carries it, and
//   - every other path the entry reaches, by the matcher's own test
//     (NoiseKeyCovers), qualifies itself or is reached by one of those per-id
//     paths already. "options.{id}.hold" also reaches another id's
//     "holdReason", another id map's "addonOptions.<id>.hold" and a nested
//     "packages.<id>.options.<id>.hold".
//
// A key doc lacks is not judged here: the matcher never excuses one by an
// entry naming id keys (noiseIndex.matchAdded), so a later response that adds
// an id, or adds the field under an id that lacked it, still fails.
//
// Otherwise its qualifying per-id paths are returned, as they always were. The
// result is sorted.
func CollapseIDKeyedPaths(doc interface{}, qualifies func(path string) bool) []string {
	type node struct {
		path, lower string
		ok          bool
	}
	var nodes []node
	seen := map[string]bool{}
	var walk func(path string, v interface{})
	walk = func(path string, v interface{}) {
		if path != "" && !seen[path] {
			seen[path] = true
			nodes = append(nodes, node{path: path, lower: strings.ToLower(path), ok: qualifies(path)})
		}
		switch v := v.(type) {
		case map[string]interface{}:
			for k, child := range v {
				walk(childNoisePath(path, k), child)
			}
		case []interface{}:
			for _, child := range v {
				walk(path, child)
			}
		}
	}
	walk("", doc)

	// reach holds every node an entry naming id keys can reach, by its
	// collapsed lowercased key, which is what such an entry is matched
	// against: a node under an id key, or under a key spelled "{id}".
	reach := map[string][]int{}
	type group struct {
		all     bool
		members []int
	}
	groups := map[string]*group{}
	var out []string
	for i, n := range nodes {
		if c := CollapseIDKeys(n.lower); strings.Contains(c, NoiseIDKey) {
			reach[c] = append(reach[c], i)
		}
		collapsed := ""
		if strings.Contains(n.path, ".") {
			if c := CollapseIDKeys(n.path); c != n.path {
				collapsed = c
			}
		}
		if collapsed == "" {
			if n.ok {
				out = append(out, n.path)
			}
			continue
		}
		g := groups[collapsed]
		if g == nil {
			g = &group{all: true}
			groups[collapsed] = g
		}
		g.members = append(g.members, i)
		g.all = g.all && n.ok
	}
	// reachesOnly reports whether every node the collapsed entry reaches
	// qualifies, or is reached by a qualifying per-id member already.
	reachesOnly := func(collapsed string, members []int) bool {
		entry, _ := NoiseEntryKey(collapsed)
		var byMember *memberCover
		for key, at := range reach {
			if !strings.Contains(key, entry) {
				continue
			}
			for _, i := range at {
				if nodes[i].ok {
					continue
				}
				if byMember == nil {
					paths := make([]string, len(members))
					for j, m := range members {
						paths[j] = nodes[m].path
					}
					byMember = newMemberCover(paths)
				}
				if !byMember.covers(nodes[i].path) {
					return false
				}
			}
		}
		return true
	}
	// g.all is only the cheap test first: a member that does not qualify is a
	// node the entry reaches, so reachesOnly refuses it as well.
	for collapsed, g := range groups {
		if g.all && reachesOnly(collapsed, g.members) {
			out = append(out, collapsed)
			continue
		}
		for _, i := range g.members {
			if nodes[i].ok {
				out = append(out, nodes[i].path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// memberCover answers whether one of a group's per-id paths addresses a
// traversal key, by the matcher's test for an entry (NoiseKeyCovers): for a
// path naming no id key, that it is a substring of the key, without case. It
// looks up each substring of the key as long as some path, so a key is not
// compared with each of thousands of ids in turn. A path that itself names id
// keys (a key spelled "{id}" on it) is tested by NoiseKeyCovers.
type memberCover struct {
	byLen  map[int]map[string]bool
	idKeys []string
}

func newMemberCover(paths []string) *memberCover {
	c := &memberCover{byLen: map[int]map[string]bool{}}
	for _, p := range paths {
		e, idKeys := NoiseEntryKey(p)
		if idKeys {
			c.idKeys = append(c.idKeys, p)
			continue
		}
		set := c.byLen[len(e)]
		if set == nil {
			set = map[string]bool{}
			c.byLen[len(e)] = set
		}
		set[e] = true
	}
	return c
}

func (c *memberCover) covers(key string) bool {
	k := strings.ToLower(key)
	for n, set := range c.byLen {
		for i := 0; i+n <= len(k); i++ {
			if set[k[i:i+n]] {
				return true
			}
		}
	}
	for _, p := range c.idKeys {
		if NoiseKeyCovers(key, p) {
			return true
		}
	}
	return false
}
