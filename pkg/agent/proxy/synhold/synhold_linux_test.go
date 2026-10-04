//go:build linux

package synhold

import (
	"net/netip"
	"testing"
	"time"
)

// TestEndpointsReadsTheSYN: a held SYN is named by its source (the
// application's end) and destination (the proxy's), for IPv4 with and
// without IP options and for IPv6.
func TestEndpointsReadsTheSYN(t *testing.T) {
	v4 := func(ihl int) []byte {
		p := make([]byte, ihl*4+20)
		p[0] = 0x40 | byte(ihl)
		p[9] = 6 // TCP
		copy(p[12:], []byte{127, 0, 0, 1})
		copy(p[16:], []byte{172, 17, 0, 2})
		tcp := p[ihl*4:]
		tcp[0], tcp[1] = 0xd4, 0x31 // 54321
		tcp[2], tcp[3] = 0x41, 0x95 // 16789
		tcp[4], tcp[5], tcp[6], tcp[7] = 0xde, 0xad, 0xbe, 0xef
		return p
	}
	v6 := func(next byte) []byte {
		p := make([]byte, 60)
		p[0] = 0x60
		p[6] = next
		p[23] = 1 // ::1
		p[39] = 1 // ::1
		p[40], p[41] = 0xd4, 0x31
		p[42], p[43] = 0x41, 0x95
		p[44], p[45], p[46], p[47] = 0xde, 0xad, 0xbe, 0xef
		return p
	}
	for _, tc := range []struct {
		name          string
		pkt           []byte
		client, proxy string
		ok            bool
	}{
		{"ipv4", v4(5), "127.0.0.1:54321", "172.17.0.2:16789", true},
		{"ipv4 with options", v4(7), "127.0.0.1:54321", "172.17.0.2:16789", true},
		{"ipv6", v6(6), "[::1]:54321", "[::1]:16789", true},
		{"ipv6 with an extension header", v6(0), "", "", false},
		{"ipv4 not tcp", func() []byte { p := v4(5); p[9] = 17; return p }(), "", "", false},
		{"truncated", v4(5)[:26], "", "", false},
		{"bad ihl", func() []byte { p := v4(5); p[0] = 0x43; return p }(), "", "", false},
		{"empty", nil, "", "", false},
		{"not ip", []byte{0x20, 0, 0}, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, isn, ok := endpoints(tc.pkt)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if c != netip.MustParseAddrPort(tc.client) || p != netip.MustParseAddrPort(tc.proxy) {
				t.Fatalf("got %s -> %s, want %s -> %s", c, p, tc.client, tc.proxy)
			}
			if isn != 0xdeadbeef {
				t.Fatalf("initial sequence number %#x, want 0xdeadbeef", isn)
			}
		})
	}
}

// TestMarksAreDistinctAndNonZero: a zero mark is every unmarked packet's, and
// two equal marks would answer one outcome with the other's reject.
func TestMarksAreDistinctAndNonZero(t *testing.T) {
	for i := 0; i < 1000; i++ {
		m, err := newMarks()
		if err != nil {
			t.Fatal(err)
		}
		all := []uint32{m.refuse, m.host, m.net}
		for i := range all {
			if all[i] == 0 {
				t.Fatalf("marks %+v", m)
			}
			for j := i + 1; j < len(all); j++ {
				if all[i] == all[j] {
					t.Fatalf("marks %+v", m)
				}
			}
		}
	}
}

func TestIfnameIsPaddedLikeTheKernelsCompare(t *testing.T) {
	b := ifname("lo")
	if len(b) != 16 || b[0] != 'l' || b[1] != 'o' || b[2] != 0 || b[15] != 0 {
		t.Fatalf("ifname(lo) = %v", b)
	}
}

// TestForgetExpiredDropsOnlyWhatExpired: answers expire oldest first, and a
// SYN answered again after its first answer keeps the newer one.
func TestForgetExpiredDropsOnlyWhatExpired(t *testing.T) {
	h := &Holder{answered: map[synKey]answer{}}
	now := time.Now()
	k := func(port uint16) synKey {
		return synKey{client: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), isn: uint32(port)}
	}
	add := func(key synKey, until time.Time) {
		h.answered[key] = answer{out: Accept, until: until}
		h.expiry = append(h.expiry, answeredAt{key: key, until: until})
	}
	add(k(1), now.Add(-2*time.Second))
	add(k(2), now.Add(-time.Second))
	add(k(3), now.Add(time.Second))
	add(k(2), now.Add(2*time.Second)) // answered again, later
	h.forgetExpired(now)
	if _, ok := h.answered[k(1)]; ok {
		t.Fatal("an expired answer was kept")
	}
	if a, ok := h.answered[k(2)]; !ok || !a.until.After(now) {
		t.Fatal("a re-answered SYN lost its newer answer to its older one's expiry")
	}
	if _, ok := h.answered[k(3)]; !ok {
		t.Fatal("an unexpired answer was dropped")
	}
	if len(h.expiry) != 2 {
		t.Fatalf("%d entries left to expire, want 2", len(h.expiry))
	}
}
