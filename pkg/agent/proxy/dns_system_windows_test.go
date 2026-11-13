//go:build windows

package proxy

import (
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A first buffer too small for the adapter list is grown to the size Windows
// asks for: the list read from a 1-byte start is the one read at once.
func TestOnWindowsTheAdaptersAreReadWhateverTheFirstBufferSize(t *testing.T) {
	whole, err := machineDNSAdapters()
	if err != nil || len(whole) == 0 {
		t.Fatalf("reading the adapters: %d adapters, err %v", len(whole), err)
	}
	orig := dnsAdaptersBufSize
	dnsAdaptersBufSize = 1
	t.Cleanup(func() { dnsAdaptersBufSize = orig })
	grown, err := machineDNSAdapters()
	if err != nil {
		t.Fatalf("reading the adapters from a 1-byte buffer: %v", err)
	}
	if len(grown) != len(whole) || !slices.Equal(resolversOfAdapters(grown), resolversOfAdapters(whole)) {
		t.Fatalf("from a 1-byte buffer: %d adapters, resolvers %v; at once: %d, %v",
			len(grown), resolversOfAdapters(grown), len(whole), resolversOfAdapters(whole))
	}
}

// A GetAdaptersAddresses failure is an error, not an empty list.
func TestOnWindowsAFailedAdapterReadIsAnError(t *testing.T) {
	orig := getAdaptersAddresses
	getAdaptersAddresses = func(uint32, uint32, uintptr, *windows.IpAdapterAddresses, *uint32) error {
		return windows.ERROR_INVALID_PARAMETER
	}
	t.Cleanup(func() { getAdaptersAddresses = orig })
	adapters, err := machineDNSAdapters()
	if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) || adapters != nil {
		t.Fatalf("got %v, err %v; want ERROR_INVALID_PARAMETER", adapters, err)
	}
}

// Every adapter of the list and every DNS server of each is read, an IPv4
// server with the IPv4 metric, a link-local IPv6 one with the IPv6 metric
// and its scope ID as its zone; a server with no address is skipped.
func TestOnWindowsEachAdapterAndServerOfTheListIsRead(t *testing.T) {
	var in4, in6, down syscall.RawSockaddrAny
	*(*windows.RawSockaddrInet4)(unsafe.Pointer(&in4)) = windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: [4]byte{10, 8, 0, 1}}
	*(*windows.RawSockaddrInet6)(unsafe.Pointer(&in6)) = windows.RawSockaddrInet6{
		Family: windows.AF_INET6, Addr: netip.MustParseAddr("fe80::1").As16(), Scope_id: 12,
	}
	*(*windows.RawSockaddrInet4)(unsafe.Pointer(&down)) = windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: [4]byte{10, 0, 0, 53}}
	none := &windows.IpAdapterDnsServerAdapter{}
	v6 := &windows.IpAdapterDnsServerAdapter{Address: windows.SocketAddress{Sockaddr: &in6}, Next: none}
	v4 := &windows.IpAdapterDnsServerAdapter{Address: windows.SocketAddress{Sockaddr: &in4}, Next: v6}
	second := &windows.IpAdapterAddresses{
		OperStatus: 2, IfType: 6, Ipv4Metric: 5,
		FirstDnsServerAddress: &windows.IpAdapterDnsServerAdapter{Address: windows.SocketAddress{Sockaddr: &down}},
	}
	first := windows.IpAdapterAddresses{OperStatus: 1, IfType: 6, Ipv4Metric: 25, Ipv6Metric: 35, FirstDnsServerAddress: v4, Next: second}

	orig := getAdaptersAddresses
	getAdaptersAddresses = func(_ uint32, _ uint32, _ uintptr, out *windows.IpAdapterAddresses, size *uint32) error {
		if *size < uint32(unsafe.Sizeof(first)) {
			*size = uint32(unsafe.Sizeof(first))
			return windows.ERROR_BUFFER_OVERFLOW
		}
		*out = first
		return nil
	}
	t.Cleanup(func() { getAdaptersAddresses = orig })

	var adapters []dnsAdapter
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("reading the list panicked: %v", r)
			}
		}()
		adapters, err = machineDNSAdapters()
		return err
	}()
	runtime.KeepAlive(second)
	runtime.KeepAlive(v4)
	if err != nil {
		t.Fatal(err)
	}
	want := []dnsAdapter{
		{up: true, servers: []dnsAdapterServer{
			{addr: netip.MustParseAddr("10.8.0.1"), metric: 25},
			{addr: netip.MustParseAddr("fe80::1").WithZone("12"), metric: 35},
		}},
		{servers: []dnsAdapterServer{{addr: netip.MustParseAddr("10.0.0.53"), metric: 5}}},
	}
	if got, want := describeAdapters(adapters), describeAdapters(want); got != want {
		t.Fatalf("read %s, want %s", got, want)
	}
}

func describeAdapters(adapters []dnsAdapter) string {
	out := ""
	for _, a := range adapters {
		out += fmt.Sprintf("[up=%v loopback=%v", a.up, a.loopback)
		for _, s := range a.servers {
			out += fmt.Sprintf(" %s/%d", s.addr, s.metric)
		}
		out += "]"
	}
	return out
}
