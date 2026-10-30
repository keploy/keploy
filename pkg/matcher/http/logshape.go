package http

import (
	"sort"

	"go.keploy.io/server/v3/pkg/models"
)

// differingHeaders are the names of the headers results holds whose values
// differ (not Normal), sorted, each once: a log line names a header that
// differs, never its value, which is the call's content (a token, a cookie).
func differingHeaders(results []models.HeaderResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range results {
		name := r.Expected.Key
		if name == "" {
			name = r.Actual.Key
		}
		if r.Normal || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// paramNames are the names of a URL's parameters, each by its shape
// (models.KeyShape), sorted: a log line names a parameter, never its value.
func paramNames(params map[string]string) []string {
	out := make([]string, 0, len(params))
	for k := range params {
		out = append(out, models.KeyShape(k))
	}
	sort.Strings(out)
	return out
}

// noiseFields are the field paths noise marks, each by its shape
// (models.FieldShape), sorted: a path may hold a map key that is a value.
func noiseFields(noise map[string][]string) []string {
	out := make([]string, 0, len(noise))
	for f := range noise {
		out = append(out, models.FieldShape(f))
	}
	sort.Strings(out)
	return out
}
