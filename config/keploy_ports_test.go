package config

import (
	"maps"
	"testing"
)

func TestKeployPorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Config)
		want map[uint16]KeployListener
	}{
		{"none set", func(*Config) {}, map[uint16]KeployListener{}},
		{"the agent's API", func(c *Config) { c.Agent.AgentPort = 16789 }, map[uint16]KeployListener{16789: KeployAPI}},
		{"the proxy, from the agent's flags", func(c *Config) { c.Agent.ProxyPort = 6123 }, map[uint16]KeployListener{6123: KeployProxy}},
		{"the proxy, from keploy.yml", func(c *Config) { c.ProxyPort = 6123 }, map[uint16]KeployListener{6123: KeployProxy}},
		{"the DNS server, from the agent's flags", func(c *Config) { c.Agent.DnsPort = 6124 }, map[uint16]KeployListener{6124: KeployDNS}},
		{"the DNS server, from keploy.yml", func(c *Config) { c.DNSPort = 6124 }, map[uint16]KeployListener{6124: KeployDNS}},
		{"all of them, two alike", func(c *Config) {
			c.Agent.AgentPort, c.Agent.ProxyPort, c.ProxyPort, c.Agent.DnsPort, c.DNSPort = 16789, 6123, 6123, 6124, 26789
		}, map[uint16]KeployListener{16789: KeployAPI, 6123: KeployProxy, 6124: KeployDNS, 26789: KeployDNS}},
		// The agent moved its DNS server onto the port keploy.yml gave the
		// proxy, and its proxy elsewhere: the agent's own ports say what
		// listens there.
		{"the agent's moves over keploy.yml's ports", func(c *Config) {
			c.ProxyPort, c.DNSPort, c.Agent.ProxyPort, c.Agent.DnsPort = 6123, 6124, 6125, 6123
		}, map[uint16]KeployListener{6123: KeployDNS, 6124: KeployDNS, 6125: KeployProxy}},
		// Not a TCP port: it names none, and cut to 16 bits it would name
		// one keploy does not listen on.
		{"past the last port", func(c *Config) { c.ProxyPort = 1<<16 + 6123 }, map[uint16]KeployListener{}},
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
