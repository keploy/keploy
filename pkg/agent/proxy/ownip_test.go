package proxy

import (
	"net"
	"testing"
)

func TestOwnIP(t *testing.T) {
	for _, c := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"203.0.113.9", false},
	} {
		if got := ownIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("ownIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}
