package testdb

import (
	"testing"
)

// TestPopulateNoise_AcceptsYamlV3MapShape regression-tests #3872: the previous
// implementation type-asserted the noise sub-map as
// map[models.AssertionType]interface{} which YAML never produces, so noise
// assertions were silently dropped on load.
func TestPopulateNoise_AcceptsYamlV3MapShape(t *testing.T) {
	noise := map[string][]string{}
	raw := map[string]interface{}{
		"header.X-Request-ID": []interface{}{"foo", "bar"},
		"body.timestamp":      []interface{}{},
	}
	populateNoise(raw, noise)

	if got, want := len(noise["header.X-Request-ID"]), 2; got != want {
		t.Errorf("header.X-Request-ID len = %d, want %d", got, want)
	}
	if noise["header.X-Request-ID"][0] != "foo" {
		t.Errorf("header.X-Request-ID[0] = %q, want %q", noise["header.X-Request-ID"][0], "foo")
	}
	if _, ok := noise["body.timestamp"]; !ok {
		t.Error("body.timestamp key should be present (initialised even when empty)")
	}
}

// TestPopulateNoise_AcceptsYamlV2MapShape covers the older library output
// where keys arrive as interface{}.
func TestPopulateNoise_AcceptsYamlV2MapShape(t *testing.T) {
	noise := map[string][]string{}
	raw := map[interface{}]interface{}{
		"header.X-Request-ID": []interface{}{"foo"},
	}
	populateNoise(raw, noise)
	if got, want := noise["header.X-Request-ID"][0], "foo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestPopulateNoise_AcceptsConcreteSliceShape covers programmatic
// constructions where the noise map round-trips without YAML in between.
func TestPopulateNoise_AcceptsConcreteSliceShape(t *testing.T) {
	noise := map[string][]string{}
	raw := map[string][]string{
		"header.Cookie": {"session", "tracking"},
	}
	populateNoise(raw, noise)
	if got, want := len(noise["header.Cookie"]), 2; got != want {
		t.Errorf("got %d entries, want %d", got, want)
	}
}
