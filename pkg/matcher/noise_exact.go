package matcher

import (
	"slices"
	"strings"
)

const ExactKeyPrefix = "\x00."

func ExactKey(path string) string {
	return ExactKeyPrefix + path
}

func ExactPath(path string) string {
	var segs []string
	for i, s := range strings.Split(strings.ReplaceAll(path, "[]", ""), ".") {
		if s != "" && (i > 0 || s != "$") {
			segs = append(segs, s)
		}
	}
	return strings.ToLower(strings.Join(segs, "."))
}

func exactPath(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, ExactKeyPrefix)
	if !ok {
		return "", false
	}
	return ExactPath(rest), true
}

func exactCovers(path, entry string) bool {
	if entry == "" {
		return false
	}
	path = strings.TrimLeft(strings.ReplaceAll(path, "[]", ""), ".")
	return path == entry || strings.HasPrefix(path, entry+".")
}

func ExactKeyCovers(path, key string) (rank string, covers, exact bool) {
	p, ok := exactPath(key)
	if !ok {
		return "", false, false
	}
	rank, idKeys := NoiseEntryKey(p)
	at := strings.ToLower(path)
	if idKeys {
		at = CollapseIDKeys(at)
	}
	return rank, exactCovers(at, rank), true
}

func HeaderKeyCovers(name, key string) bool {
	return headerKeyCovers(strings.ToLower(name), key)
}

func headerKeyCovers(nameLower, key string) bool {
	if p, ok := strings.CutPrefix(key, ExactKeyPrefix); ok {
		return nameLower == strings.ToLower(p)
	}
	return strings.Contains(nameLower, strings.ToLower(key))
}

func HeaderNoiseEntry(name string, noise map[string][]string) (key string, patterns []string, ok bool) {
	nameLower := strings.ToLower(name)
	var keys [2]string
	var found [2]bool
	for k := range noise {
		if !headerKeyCovers(nameLower, k) {
			continue
		}
		i := 0
		if strings.HasPrefix(k, ExactKeyPrefix) {
			i = 1
		}
		if !found[i] || len(k) > len(keys[i]) || (len(k) == len(keys[i]) && k < keys[i]) {
			keys[i], found[i] = k, true
		}
	}
	other, rule := noise[keys[0]], noise[keys[1]]
	switch {
	case !found[1]:
		return keys[0], other, found[0]
	case !found[0], len(rule) == 0 && len(other) > 0:
		return keys[1], rule, true
	case len(other) == 0:
		return keys[0], other, true
	}
	return keys[0], append(slices.Clip(other), rule...), true
}

func eitherEntry(rule *noiseEntry, other noiseEntry) noiseEntry {
	if rule == nil || len(other.regexps) == 0 && !other.typeStrict {
		return other
	}
	if len(rule.regexps) == 0 && (!rule.typeStrict || len(other.regexps) > 0) {
		return *rule
	}
	if len(other.regexps) == 0 {
		return other
	}
	other.regexps = append(slices.Clip(other.regexps), rule.regexps...)
	return other
}
