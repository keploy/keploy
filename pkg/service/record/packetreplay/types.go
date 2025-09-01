//go:build linux

package packetreplay

import (
	"context"
	"errors"
	"time"

	corePkg "go.keploy.io/server/v2/pkg/core"
)

const (
	DefaultProxyPort = 16789
	DefaultDestPort  = 16790
	DefaultProxyAddr = "127.0.0.1:16789"
	PreserveTiming   = false
	WriteDelay       = 10 * time.Millisecond
)

var (
	ErrMissingPCAP         = errors.New("pcap path is required")
	ErrNoProxyRelatedFlows = errors.New("no proxy-related payloads found in pcap")
)

type ReplayOptions struct {
	PreserveTiming bool
	WriteDelay     time.Duration
}

type direction int

const (
	DirToProxy direction = iota
	DirFromProxy
)

type flowKeyDup struct {
	srcIP   string
	dstIP   string
	srcPort uint16
	dstPt   uint16
	payload []byte
	ts      time.Time
	dir     direction
}

type StreamSeq struct {
	port    uint16
	events  []flowKeyDup
	firstTS time.Time
}

type FakeDestInfo struct{}

func NewFakeDestInfo() *FakeDestInfo { return &FakeDestInfo{} }

func (f *FakeDestInfo) Get(ctx context.Context, srcPort uint16) (*corePkg.NetworkAddress, error) {
	return &corePkg.NetworkAddress{
		AppID:    12345,
		Version:  4,
		IPv4Addr: 0x7F000001,            // 127.0.0.1
		IPv6Addr: [4]uint32{0, 0, 0, 1}, // ::1
		Port:     DefaultDestPort,
	}, nil
}

func (f *FakeDestInfo) Delete(ctx context.Context, srcPort uint16) error {
	return nil
}
