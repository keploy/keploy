package http

import (
	"net/url"
	"slices"
	"strings"

	"go.keploy.io/server/v3/config"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

func AddEndpointNoise(noise map[string]map[string][]string, rules []config.EndpointNoise, req models.HTTPReq) map[string]map[string][]string {
	if len(rules) == 0 {
		return noise
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return noise
	}
	var out map[string]map[string][]string
	for _, r := range rules {
		if !endpointMatches(r, string(req.Method), u.Path) {
			continue
		}
		if out == nil {
			out = cloneNoise(noise)
		}
		for k, v := range r.Body {
			addNoise(out, "body", matcherUtils.ExactKey(k), v)
		}
		for k, v := range r.Header {
			addNoise(out, "header", matcherUtils.ExactKey(k), v)
		}
	}
	if out == nil {
		return noise
	}
	return out
}

func endpointMatches(r config.EndpointNoise, method, path string) bool {
	if r.Method != "" && !strings.EqualFold(r.Method, method) {
		return false
	}
	return pathMatches(r.Path, path)
}

func pathMatches(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	want := strings.Split(strings.Trim(pattern, "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if len(w) > 2 && strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if w != got[i] {
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
