package matcher

import (
	"sort"
	"strings"

	"go.uber.org/zap"
)

// BodyNoiseCovering names the noise entry that covers a place of an HTTP
// response's JSON body, as the comparison applies noise, or "" when none
// does: the test case's noise over the whole body, a global body "*", then a
// key of the test case's body noise, then one of the global body noise.
// keys are the object keys of the place's path in order, without its array
// indices (the comparison walks every element of an array under the array's
// key, see noisepath.go). A key without a dot names a field at any depth; a
// dotted key names a path; an entry over a field covers everything under it.
// The name says where the entry is: "body.id (this test case)", "body.orders
// (globalNoise)". global is the noise configuration for the test set
// (globalNoise with its test set's entries).
func BodyNoiseCovering(keys []string, tcNoise map[string][]string, global map[string]map[string][]string, logger *zap.Logger) string {
	tcBody, _, skipBody := SplitNoise(tcNoise, logger)
	if skipBody {
		return "body (this test case)"
	}
	if fields, ok := global["body"]["*"]; ok && len(fields) > 0 && fields[0] == "*" {
		return "body.* (globalNoise)"
	}
	if k := bodyKeyCovering(keys, tcBody); k != "" {
		return "body." + k + " (this test case)"
	}
	if k := bodyKeyCovering(keys, global["body"]); k != "" {
		return "body." + k + " (globalNoise)"
	}
	return ""
}

// bodyKeyCovering is a body noise key that covers a place by the comparison's
// rules: a key without a dot names a field anywhere, a dotted key a path,
// and an entry over a field covers everything under it. Keys are tried in
// sorted order, so the name said never depends on map order.
func bodyKeyCovering(keys []string, noise map[string][]string) string {
	if len(noise) == 0 {
		return ""
	}
	path := strings.ToLower(strings.Join(keys, "."))
	names := make([]string, 0, len(noise))
	for k := range noise {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		lk := strings.ToLower(k)
		if !strings.Contains(lk, ".") {
			for _, seg := range keys {
				if strings.EqualFold(seg, lk) {
					return k
				}
			}
			continue
		}
		if strings.Contains(path, lk) {
			return k
		}
	}
	return ""
}
