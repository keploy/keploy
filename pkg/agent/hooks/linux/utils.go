//go:build linux

package linux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func GetSelfInodeNumber() (uint64, error) {
	p := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(p)
	if err != nil {
		return 0, errors.New("failed to get inode of the keploy process")
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	Ino := (f.Sys().(*syscall.Stat_t)).Ino
	if Ino != 0 {
		return Ino, nil
	}
	return 0, nil
}

func GetContainerIP() (string, error) {
	// Get all network interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	// Iterate over the interfaces
	for _, i := range interfaces {
		// Skip down or loopback interfaces
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}

		// Get the addresses for the current interface
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}

		// Iterate over the addresses
		for _, addr := range addrs {
			var ip net.IP
			// The address can be of type *net.IPNet or *net.IPAddr
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				// Check if it's an IPv4 address
				if ipnet.IP.To4() != nil {
					ip = ipnet.IP
				}
			}

			if ip != nil {
				// Found a valid IPv4 address, return it
				return ip.String(), nil
			}
		}
	}

	return "", fmt.Errorf("could not find a non-loopback IP for the container")
}

// Uint32ToNetIP converts a uint32 to a net.IP type.
func Uint32ToNetIP(ip_uint32 uint32) net.IP {
	ip := make(net.IP, 4)
	// Assuming the uint32 is Big Endian (Network Byte Order),
	// which is the standard for IP addresses.
	binary.BigEndian.PutUint32(ip, ip_uint32)
	return ip
}
