package config

import "testing"

// The deprecated schemaNoise* keys must keep working. Both are booleans, so
// "absent" and "explicitly false" are indistinguishable on the struct — the
// isSet callback (wired to viper.IsSet in production) is what separates them.
func TestResolveMockNoiseAliases(t *testing.T) {
	set := func(keys ...string) func(string) bool {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[k] = true
		}
		return func(k string) bool { return m[k] }
	}

	t.Run("deprecated key alone is honoured", func(t *testing.T) {
		c := &Config{}
		c.Test.SchemaNoiseStrict = true
		used := c.ResolveMockNoiseAliases(set("test.schemaNoiseStrict"))

		if !c.Test.MockNoiseStrict {
			t.Fatal("schemaNoiseStrict: true was dropped instead of folded into mockNoiseStrict")
		}
		if len(used) != 1 || used[0] != "test.schemaNoiseStrict" {
			t.Fatalf("expected the deprecated key to be reported for warning, got %v", used)
		}
	})

	// The regression the isSet callback exists to prevent: without it the new
	// field's zero value would silently win.
	t.Run("deprecated true is not clobbered by the canonical zero value", func(t *testing.T) {
		c := &Config{}
		c.Test.SchemaNoiseDetection = true
		c.ResolveMockNoiseAliases(set("test.schemaNoiseDetection"))
		if !c.Test.MockNoiseDetection {
			t.Fatal("canonical zero value clobbered the deprecated setting")
		}
	})

	t.Run("canonical wins when both are set", func(t *testing.T) {
		c := &Config{}
		c.Test.SchemaNoiseStrict = true
		c.Test.MockNoiseStrict = false
		used := c.ResolveMockNoiseAliases(set("test.schemaNoiseStrict", "test.mockNoiseStrict"))

		if c.Test.MockNoiseStrict {
			t.Fatal("the deprecated key overrode an explicitly-set canonical key")
		}
		if len(used) != 0 {
			t.Fatalf("the deprecated key was not used, so it must not be warned about: %v", used)
		}
	})

	t.Run("an explicit deprecated false is honoured", func(t *testing.T) {
		c := &Config{}
		c.Test.MockNoiseStrict = true // e.g. a non-zero default
		c.Test.SchemaNoiseStrict = false
		c.ResolveMockNoiseAliases(set("test.schemaNoiseStrict"))
		if c.Test.MockNoiseStrict {
			t.Fatal("schemaNoiseStrict: false was ignored")
		}
	})

	t.Run("nothing set changes nothing and warns about nothing", func(t *testing.T) {
		c := &Config{}
		if used := c.ResolveMockNoiseAliases(set()); len(used) != 0 {
			t.Fatalf("got %v", used)
		}
		if c.Test.MockNoiseDetection || c.Test.MockNoiseStrict {
			t.Fatal("fields changed with no keys set")
		}
	})

	t.Run("nil receiver", func(t *testing.T) {
		var c *Config
		if used := c.ResolveMockNoiseAliases(set()); used != nil {
			t.Fatalf("got %v", used)
		}
	})
}
