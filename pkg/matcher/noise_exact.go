package matcher

import "strings"

const ExactKeyPrefix = "$."

func ExactKey(path string) string {
	return ExactKeyPrefix + path
}

func exactPath(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, ExactKeyPrefix)
	if !ok {
		return "", false
	}
	var segs []string
	for _, s := range strings.Split(strings.ReplaceAll(rest, "[]", ""), ".") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return strings.ToLower(strings.Join(segs, ".")), true
}

func exactCovers(path, entry string) bool {
	if entry == "" {
		return false
	}
	path = strings.ReplaceAll(path, "[]", "")
	return path == entry || strings.HasPrefix(path, entry+".")
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
