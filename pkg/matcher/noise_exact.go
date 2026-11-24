package matcher

import "strings"

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
