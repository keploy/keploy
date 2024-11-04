//go:build !windows

package hooks

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// IPv4ToUint32 converts a string representation of an IPv4 address to a 32-bit integer.
func IPv4ToUint32(ipStr string) (uint32, error) {
	ipAddr := net.ParseIP(ipStr)
	if ipAddr != nil {
		ipAddr = ipAddr.To4()
		if ipAddr != nil {
			return binary.BigEndian.Uint32(ipAddr), nil
		}
		return 0, errors.New("not a valid IPv4 address")
	}
	fmt.Println("failed to parse IP address", ipStr)
	return 0, errors.New("failed to parse IP address")
}

// ToIPv4MappedIPv6 converts an IPv4 address to an IPv4-mapped IPv6 address.
func ToIPv4MappedIPv6(ipv4 string) ([4]uint32, error) {
	var result [4]uint32

	// Parse the input IPv4 address
	ip := net.ParseIP(ipv4)
	if ip == nil {
		return result, errors.New("invalid IPv4 address")
	}

	// Check if the input is an IPv4 address
	ip = ip.To4()
	if ip == nil {
		return result, errors.New("not a valid IPv4 address")
	}

	// Convert IPv4 address to IPv4-mapped IPv6 address
	// IPv4-mapped IPv6 address is ::ffff:a.b.c.d
	ipv6 := "::ffff:" + ipv4

	// Parse the resulting IPv6 address
	ip6 := net.ParseIP(ipv6)
	if ip6 == nil {
		return result, errors.New("failed to parse IPv4-mapped IPv6 address")
	}

	// Convert the IPv6 address to a 16-byte representation
	ip6Bytes := ip6.To16()
	if ip6Bytes == nil {
		return result, errors.New("failed to convert IPv6 address to bytes")
	}

	// Populate the result array
	for i := 0; i < 4; i++ {
		result[i] = uint32(ip6Bytes[i*4])<<24 | uint32(ip6Bytes[i*4+1])<<16 | uint32(ip6Bytes[i*4+2])<<8 | uint32(ip6Bytes[i*4+3])
	}

	return result, nil
}

// detectCgroupPath returns the first-found mount point of type cgroup2
// and stores it in the cgroupPath global variable.
func detectCgroupPath(logger *zap.Logger) (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer func() {
		err := f.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close /proc/mounts file")
		}
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// example fields: cgroup2 /sys/fs/cgroup/unified cgroup2 rw,nosuid,nodev,noexec,relatime 0 0
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1], nil
		}
	}

	return "", errors.New("cgroup2 not mounted")
}

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

func GetPortToSendToKernel(_ context.Context, rules []config.BypassRule) []uint {
	// if the rule only contains port, then it should be sent to kernel
	ports := []uint{}
	for _, rule := range rules {
		if rule.Host == "" && rule.Path == "" {
			if rule.Port != 0 {
				ports = append(ports, rule.Port)
			}
		}
	}
	return ports
}
