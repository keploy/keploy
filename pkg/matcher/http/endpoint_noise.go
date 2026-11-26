package http

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"go.keploy.io/server/v3/config"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var (
	arrayPosition = regexp.MustCompile(`\[\d+\]`)
	leadingWord   = regexp.MustCompile(`^[A-Za-z][A-Za-z-]*\s+\S`)
)

func WarnEndpointNoiseProblems(logger *zap.Logger, rules []config.EndpointNoise) {
	if logger == nil {
		return
	}
	for i, r := range rules {
		rule := fmt.Sprintf("test.globalNoise.endpoints[%d]: %s", i, strings.TrimSpace(r.Method+" "+r.Path))
		warn := func(reason, next string) {
			logger.Warn("an endpoint noise rule ignores nothing", zap.String("rule", rule), zap.String("reason", reason), zap.String("next_step", next))
		}
		path := strings.TrimSpace(r.Path)
		switch {
		case path == "":
			warn("it has no path, so it matches no request", "add the endpoint's path, for example path: /version")
		case notAPath(path):
			warn("its path holds a host, a query, a fragment or a method, and keploy compares the request's path alone, so it matches no request", "write the path alone, for example /version")
		}
		for _, f := range slices.Sorted(maps.Keys(r.Body)) {
			if arrayPosition.MatchString(f) {
				warn("body field "+f+" names one element of an array, and a rule's field names every element, so it matches no field", "write the field without the position: "+arrayPosition.ReplaceAllString(f, ""))
			}
		}
		if len(r.Body) == 0 && len(r.Header) == 0 {
			warn("it names no body field or header", "add a body field or header name to the rule")
		}
	}
}

func notAPath(path string) bool {
	if strings.Contains(path, "://") || strings.ContainsAny(path, "?#") {
		return true
	}
	if strings.HasPrefix(path, "/") {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return strings.ContainsAny(first, ".:") || leadingWord.MatchString(path)
}

func AddEndpointNoise(noise map[string]map[string][]string, rules []config.EndpointNoise, tc *models.TestCase) map[string]map[string][]string {
	if len(rules) == 0 || tc == nil {
		return noise
	}
	u, err := url.Parse(tc.HTTPReq.URL)
	if err != nil {
		return noise
	}
	var out map[string]map[string][]string
	var takenBody, takenHeader map[string]bool
	for _, r := range rules {
		if !endpointMatches(r, string(tc.HTTPReq.Method), u) {
			continue
		}
		if out == nil {
			out = cloneNoise(noise)
			takenBody, takenHeader = ownNoise(tc.Noise)
		}
		for _, k := range slices.Sorted(maps.Keys(r.Body)) {
			if name := matcherUtils.ExactPath(k); !takenBody[name] {
				takenBody[name] = true
				addNoise(out, "body", matcherUtils.ExactKey(k), r.Body[k])
			}
		}
		for _, k := range slices.Sorted(maps.Keys(r.Header)) {
			if name := strings.ToLower(k); !takenHeader[name] {
				takenHeader[name] = true
				addNoise(out, "header", matcherUtils.ExactKey(k), r.Header[k])
			}
		}
	}
	if out == nil {
		return noise
	}
	return out
}

func ownNoise(noise map[string][]string) (body, header map[string]bool) {
	b, h, _ := matcherUtils.SplitNoise(noise, nil)
	body = make(map[string]bool, len(b))
	for k := range b {
		body[strings.ToLower(k)] = true
	}
	header = make(map[string]bool, len(h))
	for k := range h {
		header[strings.ToLower(k)] = true
	}
	return body, header
}

func endpointMatches(r config.EndpointNoise, method string, u *url.URL) bool {
	if r.Method != "" && !strings.EqualFold(r.Method, method) {
		return false
	}
	return pathMatches(r.Path, u)
}

func pathMatches(pattern string, u *url.URL) bool {
	if pattern == "" {
		return false
	}
	want := strings.Split(strings.Trim(pattern, "/"), "/")
	got := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		g, err := url.PathUnescape(got[i])
		if err != nil {
			return false
		}
		if len(w) > 2 && strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			if g == "" {
				return false
			}
			continue
		}
		if pw, err := url.PathUnescape(w); err == nil {
			w = pw
		}
		if w != g {
			return false
		}
	}
	return true
}

func cloneNoise(src map[string]map[string][]string) map[string]map[string][]string {
	out := make(map[string]map[string][]string, len(src)+2)
	for section, fields := range src {
		out[section] = make(map[string][]string, len(fields))
		for k, v := range fields {
			out[section][k] = slices.Clone(v)
		}
	}
	return out
}

func addNoise(noise map[string]map[string][]string, section, key string, patterns []string) {
	if noise[section] == nil {
		noise[section] = map[string][]string{}
	}
	noise[section][key] = slices.Clone(patterns)
}
