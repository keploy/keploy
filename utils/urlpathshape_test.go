package utils

import (
	"net/url"
	"testing"
)

// A path's shape writes each segment that holds a value {} and keeps what
// holds none: an empty segment, a missing leading "/" (the request line
// "OPTIONS *"), and a nil URL.
func TestURLPathShapeKeepsWhatHoldsNoValue(t *testing.T) {
	for _, c := range []struct {
		u    *url.URL
		want string
	}{
		{nil, ""},
		{&url.URL{}, ""},
		{&url.URL{Path: "*"}, "{}"},
		{&url.URL{Path: "/a//b/"}, "/{}//{}/"},
		{&url.URL{Path: "/a/b%2Fc", RawPath: "/a/b%2Fc"}, "/{}/{}"},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("URLPathShape(%v) panicked: %v", c.u, r)
				}
			}()
			if got := URLPathShape(c.u); got != c.want {
				t.Errorf("URLPathShape(%v) = %q, want %q", c.u, got, c.want)
			}
		}()
	}
}
