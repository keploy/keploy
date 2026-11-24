package http

import (
	"net/url"
	"slices"
	"strings"

	"go.keploy.io/server/v3/config"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

func AddEndpointNoise(noise map[string]map[string][]string, rules []config.EndpointNoise, tc *models.TestCase) map[string]map[string][]string {
	if len(rules) == 0 || tc == nil {
		return noise
	}
	u, err := url.Parse(tc.HTTPReq.URL)
	if err != nil {
		return noise
	}
	var out map[string]map[string][]string
	var ownBody, ownHeader map[string]bool
	for _, r := range rules {
		if !endpointMatches(r, string(tc.HTTPReq.Method), u) {
			continue
		}
		if out == nil {
			out = cloneNoise(noise)
			ownBody, ownHeader = ownNoise(tc.Noise)
		}
		for k, v := range r.Body {
			if !ownBody[matcherUtils.ExactPath(k)] {
				addNoise(out, "body", matcherUtils.ExactKey(k), v)
			}
		}
		for k, v := range r.Header {
			if !ownHeader[strings.ToLower(k)] {
				addNoise(out, "header", matcherUtils.ExactKey(k), v)
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
