package http

import "strings"

// pathDiffersAt is the index of the first segment in which the escaped
// paths a and b differ, counted from 0 after the leading "/", for a log line
// that names both paths by their shape only: it says where they differ, not
// what either holds. -1 when they are the same.
func pathDiffersAt(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "/"), "/")
	bs := strings.Split(strings.TrimPrefix(b, "/"), "/")
	for i := 0; i < len(as) || i < len(bs); i++ {
		if i >= len(as) || i >= len(bs) || as[i] != bs[i] {
			return i
		}
	}
	return -1
}
