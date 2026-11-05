package config

import (
	"maps"
	"testing"
)

func TestKeployPorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Config)
		want map[uint16]bool
	}{
		{"none set", func(*Config) {}, map[uint16]bool{}},
		{"the agent's API", func(c *Config) { c.Agent.AgentPort = 16789 }, map[uint16]bool{16789: true}},
		{"the proxy, from the agent's flags", func(c *Config) { c.Agent.ProxyPort = 6123 }, map[uint16]bool{6123: true}},
		{"the proxy, from keploy.yml", func(c *Config) { c.ProxyPort = 6123 }, map[uint16]bool{6123: true}},
		{"the DNS server, from the agent's flags", func(c *Config) { c.Agent.DnsPort = 6124 }, map[uint16]bool{6124: true}},
		{"the DNS server, from keploy.yml", func(c *Config) { c.DNSPort = 6124 }, map[uint16]bool{6124: true}},
		{"all of them, two alike", func(c *Config) {
			c.Agent.AgentPort, c.Agent.ProxyPort, c.ProxyPort, c.Agent.DnsPort, c.DNSPort = 16789, 6123, 6123, 6124, 26789
		}, map[uint16]bool{16789: true, 6123: true, 6124: true, 26789: true}},
		// Not a TCP port: it names none, and cut to 16 bits it would name
		// one keploy does not listen on.
		{"past the last port", func(c *Config) { c.ProxyPort = 1<<16 + 6123 }, map[uint16]bool{}},
	} {
		conf := &Config{}
		tc.set(conf)
		if got := KeployPorts(conf); !maps.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("no configuration: panicked: %v", r)
			}
		}()
		if got := KeployPorts(nil); len(got) != 0 {
			t.Errorf("no configuration: got %v, want none", got)
		}
	}()
}
