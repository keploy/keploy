package async

import (
	"net/url"
	"path"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// MatchesHTTPLane reports whether an HTTP call to host and urlPath, whose query
// query gives, belongs to an "http" lane: lane.Match's host glob (path.Match)
// and its path, a pathRegex or else a path glob, and lane.MatchQuery's
// key=value pairs. Every criterion the lane names must hold, and a lane that
// names none takes no call. host is the request's host as it named it (an
// HTTP/1 Host header, an HTTP/2 :authority), port included when it named one.
//
// It is the one reading of an "http" lane's match block, which the HTTP/1 and
// the HTTP/2 parser both serve (LaneTyper), so a lane takes the same calls
// whichever protocol the client spoke. query is called only when the lane
// names a query, so a caller need not parse one it does not have to.
func MatchesHTTPLane(lane models.AsyncLane, host, urlPath string, query func() url.Values) bool {
	matched := false
	if hg := lane.Match["host"]; hg != "" {
		matched = true
		if ok, _ := path.Match(hg, host); !ok {
			return false
		}
	}
	// pathRegex, when set, is the path check (a regex alternative to the glob
	// match.path — for URLs a glob can't express, e.g. numeric-id segments).
	// Reuses pkg/matcher's cached, fail-closed regex helper.
	if pr := lane.Match["pathRegex"]; pr != "" {
		matched = true
		if ok, _ := matcher.MatchesAnyRegex(urlPath, []string{pr}); !ok {
			return false
		}
	} else if pg := lane.Match["path"]; pg != "" {
		matched = true
		if ok, _ := path.Match(pg, urlPath); !ok {
			return false
		}
	}
	if len(lane.MatchQuery) > 0 {
		matched = true
		var q url.Values
		if query != nil {
			q = query()
		}
		for k, v := range lane.MatchQuery {
			if q.Get(k) != v {
				return false
			}
		}
	}
	return matched
}
